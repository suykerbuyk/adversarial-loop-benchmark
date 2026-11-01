# Implementation plan: `blockcache`

Package `blockcache` is a bounded, concurrent, single-flight cache in front of a slow backing store. This plan is the implementation contract. Nothing is implemented here.

No prior adversary findings exist at `plan/plan_findings.md`, so there is no rebuttal file.

## Public API

Ship exactly the surface in the spec. The package lives at the module root (`worktree/`, module path `blockcache`, Go 1.25). Public tests import `package blockcache` and call these names directly.

```go
package blockcache

import (
	"context"
	"errors"
)

// Loader fetches the block for key. The cache calls it at most once per in-flight
// miss for a key (single-flight). It is given a cache-scoped context, not any
// single caller's. A Loader MUST return promptly when its ctx is Done.
type Loader func(ctx context.Context, key string) ([]byte, error)

type Cache interface {
	// Get returns a COPY of the block for key, loading on a miss. Concurrent Gets
	// for the same absent key cause exactly one Loader call; all receive its result.
	Get(ctx context.Context, key string) ([]byte, error)
	// Len is the number of resident entries. Resident-only; it may lag a completion
	// by one critical section and is never above capacity.
	Len() int
	// Close cancels in-flight loads, waits for their Loader calls to return, wakes
	// every waiter with ErrClosed, drops all entries, and returns nil. Idempotent.
	Close() error
}

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache

var ErrClosed = errors.New("blockcache: closed")
```

Concrete type is unexported (`*cache`). `New` returns the `Cache` interface. `ErrClosed` is a sentinel; callers use `errors.Is`. Wrapping is legal and is not required.

`New` preconditions, checked before any allocation that a caller could observe:

- `capacity < 1` panics.
- `load == nil` panics.

Panic value is unspecified (`TestNewPanics` only checks that `recover()` is non-nil). Use a clear string so a misuse fails loudly.

`Close` always returns `nil`.

## Internal data model

One mutex protects the map, the LRU list, the closed flag, and publication of a load result. The Loader runs outside that mutex. Waiters block on a per-flight `done` channel, never on the mutex.

```text
cache
  mu        sync.Mutex
  capacity  int
  load      Loader
  baseCtx   context.Context          // cancelled by Close; parent of every Loader ctx
  cancel    context.CancelFunc
  closed    bool                     // set under mu before cancel is invoked
  resident  map[string]*entry        // successfully stored blocks only
  order     doubly-linked list       // head = MRU, tail = LRU; only resident entries
  inflight  map[string]*flight       // keys with a Loader call that has not finished
  loading   sync.WaitGroup           // one Add per started Loader; Close waits on it

entry
  key   string
  value []byte                       // cache-owned copy; never the Loader's slice
  prev, next *entry                  // intrusive LRU links; nil when not resident

flight
  key    string
  done   chan struct{}               // closed exactly once, after result is published
  result []byte                      // cache-owned copy, or nil
  err    error                       // non-nil means do not store
  ready  bool                        // true once result/err are visible
```

Invariants, all held under `mu` except where noted:

- `len(resident) <= capacity` at every unlock. `Len` reads `len(resident)` under `mu`, so a concurrent poll cannot observe a value above capacity. The "lag by one critical section" allowance is the window between a Loader return and the lock that inserts or discards; `Len` never includes in-flight keys.
- `inflight` and `resident` are disjoint. A key moves from `inflight` to `resident` only on a successful publish that is still allowed to store.
- An in-flight load does not occupy a capacity slot and is not on `order`, so it cannot be evicted.
- `order` contains exactly the resident entries. Head is most recently used. Tail is least recently used. Ties among equally old entries are resolved by list order; any least-recent tail is acceptable.
- `value` slices stored in entries, and the `result` slice on a flight, are owned by the cache. No caller- or Loader-supplied slice is retained.
- `flight.done` is closed only after `result`, `err`, and `ready` are written, and only while holding `mu` (or immediately before unlock, still with those fields written under `mu`). Waiters re-acquire `mu` or read the published fields only after observing the close, using the channel close as the happens-before edge. Practical choice: publish under `mu`, close `done` under `mu`, waiters take `mu` after `<-done` only if they still need the map; the bytes they return are copied under `mu` before they drop the lock, so they never read `result` racily.

Why not `golang.org/x/sync/singleflight`: it shares one result slice among callers, calls the function with the caller's context (or a derived one that cancels when waiters leave, depending on usage), and does not know about LRU or `Close`. The flight table above is small and keeps those rules local. No third-party dependency. `go.mod` stays `module blockcache` / `go 1.25`.

### Copy helper

```text
clone(b []byte) []byte
  if b == nil { return nil }   // preserve nil vs empty; success path may still return empty non-nil
  out := make([]byte, len(b))
  copy(out, b)
  return out
```

Two copies on the success path:

1. When the Loader returns, copy into `flight.result` (and later into `entry.value`) before the Loader's slice can be reused. Do not store the Loader's slice.
2. Every `Get` return copies out of `entry.value` or `flight.result`. Distinct waiters get distinct slices. Mutating one does not affect the cache or another waiter.

A nil Loader success (`nil, nil`) is a successful empty block. Store a nil or an empty cache-owned slice consistently, and hand each caller their own copy (`nil` copied is `nil`; that is fine because there are no bytes to mutate). A non-nil error always wins: discard any accompanying bytes, store nothing, return `(nil, err)`.

### LRU operations (under `mu`)

Intrusive doubly-linked list avoids allocating a `container/list` element per hit and keeps eviction O(1).

- `touch(e)`: unlink `e` if linked, insert at head.
- `evictOne()`: unlink and delete `order`'s tail from `resident`. Called only when `len(resident) == capacity` and a new key is about to be inserted. Never evicts a key that is only in `inflight`.
- Hit path: `touch` the resident entry, copy bytes, unlock, return. No Loader call.
- Successful admit: if `len(resident) == capacity`, `evictOne()` first, then insert the new entry at head. If the key is already resident (a duplicate completion cannot happen for one flight, but a closed-then-irrelevant path must not insert), do not insert.

Recency (rule 6): a hit calls `touch`. A successful admit inserts at head, which is the MRU position. A failed load does not touch anything. An in-flight key has no recency until it resolves successfully.

## Concurrency protocol

### `Get`

```text
Get(ctx, key):
  c.mu.Lock()
  if c.closed:
    unlock; return nil, ErrClosed          // rule 8: do not call Loader
  if e := c.resident[key]; e != nil:
    touch(e)
    out := clone(e.value)
    unlock; return out, nil                // rule 1, rule 4
  if f := c.inflight[key]; f != nil:
    unlock
    return c.wait(ctx, f)                  // join existing flight; no Loader call
  // cold miss: this caller owns the single flight
  f := &flight{key: key, done: make(chan struct{})}
  c.inflight[key] = f
  c.loading.Add(1)
  unlock
  go c.runLoad(f)                          // Loader runs off the Get goroutine
  return c.wait(ctx, f)
```

Starting the Loader in its own goroutine is required by rule 7. If `Get` called the Loader inline, a caller cancel could not return while the load continues, and `Close` could deadlock if it waited for Loaders from inside a `Get` that was itself the Loader. The owner `Get` is just another waiter.

Single-flight (rule 2): the check-and-insert of `inflight[key]` happens under `mu`. Exactly one `Get` observes a missing flight and installs one. Every other concurrent `Get` for that key finds `f` and waits. The public `TestSingleFlight` parks 50 Gets inside the Loader (`<-release`) and asserts `calls == 1` before release. That holds because the second through fiftieth Gets attach to the same `flight` and never call `load`.

### `runLoad`

```text
runLoad(f):
  defer c.loading.Done()
  // Loader context is cache-scoped, never a caller's ctx (rule 7).
  val, err := c.load(c.baseCtx, f.key)
  if err != nil:
    val = nil                            // rule 3: error wins; discard bytes

  c.mu.Lock()
  defer c.mu.Unlock()
  delete(c.inflight, f.key)              // always, so a later Get can retry
  if err != nil || c.closed || c.baseCtx.Err() != nil:
    f.err = err
    if c.closed || c.baseCtx.Err() != nil:
      // result discarded, not stored (rule 8), even if err == nil
      f.err = err                        // waiters already released via Close or their own ctx
    f.result = nil
    f.ready = true
    close(f.done)
    return
  // success, cache still open, base ctx not cancelled
  owned := clone(val)
  f.result = owned
  f.err = nil
  f.ready = true
  if _, ok := c.resident[f.key]; !ok:
    if len(c.resident) == c.capacity:
      evictOne()                         // LRU resident only
    e := &entry{key: f.key, value: owned}
    c.resident[f.key] = e
    insert at MRU head
  close(f.done)
```

Refinement of the cancel/close branch: `Close` cancels `baseCtx` before it waits, so a Loader that returns because of cancellation has `c.closed == true` or `c.baseCtx.Err() != nil` by the time it takes `mu` only if `Close` has set `closed` before cancel. Set `closed = true` under `mu`, unlock, then `cancel()`, so a Loader that loses the race still observes either `closed` or a cancelled base context when it re-locks to publish. Check both. If either is set, do not insert. Delete `inflight` regardless, so the key is neither resident nor in-flight after a discarded completion.

If the Loader returns a real error and the cache is also closing, do not store (both rules agree). Waiters are handled by `Close` (they get `ErrClosed`) or have already left via their own ctx. Publishing `f.err` is still useful for a waiter that observed resolution before it observed close; see waiter priority below.

Do not call the Loader under `mu`. Do not call `Wait` under `mu`.

### `wait` (caller context, close, orphaned load)

```text
wait(ctx, f):
  select {
  case <-f.done:
    // load resolved; copy out under mu so we do not race with the publisher
    // if we instead copied before close(done), a post-close read of f.result
    // is safe because result is immutable after close. Prefer copying under mu
    // for one obvious protocol.
    c.mu.Lock()
    // Rule 7 priority: ErrClosed outranks a resolved value if Close has won.
    if c.closed:
      c.mu.Unlock()
      return nil, ErrClosed
    out := clone(f.result)
    err := f.err
    c.mu.Unlock()
    if err != nil:
      return nil, err
    return out, nil
  case <-c.closedCh:                     // see Close; closed under the same signal as ErrClosed
    return nil, ErrClosed
  case <-ctx.Done():
    return nil, ctx.Err()
  }
```

Rule 7 priority is (a) `ErrClosed` if the cache closes, else (b) `ctx.Err()` if the caller's ctx is Done while the load is still in flight, else (c) the load result.

A single `select` is not a priority. Two cases can be ready at once, and Go chooses pseudo-randomly. Encode priority explicitly:

```text
wait(ctx, f):
  for {
    c.mu.Lock()
    if c.closed:
      c.mu.Unlock()
      return nil, ErrClosed              // (a) always wins
    if f.ready:
      out, err := clone(f.result), f.err
      c.mu.Unlock()
      if err != nil:
        return nil, err                  // (c) error; bytes nil
      return out, nil                    // (c) success copy
    callerDone := ctx.Err()
    c.mu.Unlock()
    if callerDone != nil:
      return nil, callerDone             // (b) only if not closed and not ready
    select {
    case <-f.done:
    case <-c.doneClosing:                // fired when closed is set
    case <-ctx.Done():
    }
    // loop to re-check under mu in priority order
  }
```

Leaving on (b) does not cancel the flight, does not decrement a waiter count that would abort the load, and does not delete `inflight`. The Loader keeps running on `baseCtx`. On success, `runLoad` still inserts (rule 7: orphaned success populates the cache). That is not a leak: the flight ends, `loading.Done()` runs, and the entry is a normal resident that LRU can evict later.

If the caller ctx fires and the load resolves in the same beat, the loop re-checks under `mu`. If `f.ready` and the cache is not closed, return the result (the load is no longer in flight, so (b) does not apply). If `closed`, return `ErrClosed`.

`ctx` is never passed to `load`.

### `Close`

```text
Close():
  c.mu.Lock()
  if c.closed:
    c.mu.Unlock()
    <-c.joined                               // second Close waits for the same join
    return nil
  c.closed = true
  close(c.doneClosing)                       // wakes waiters; they re-check and return ErrClosed
  // drop resident entries now, under mu, so Len() becomes 0 and Gets cannot hit
  c.resident = empty map
  c.order = empty
  // leave inflight in place until each runLoad deletes its key; do not store later
  c.mu.Unlock()
  c.cancel()                                 // Loaders must return promptly
  c.loading.Wait()                           // wait for every Loader call to return
  close(c.joined)                            // release any Close that arrived mid-join
  return nil
```

Idempotent join: the first `Close` owns `loading.Wait()`. A second `Close` that arrives after `closed` is set blocks on `joined` until the first finishes that wait, then returns nil. A second `Close` after `joined` is already closed returns immediately. Both return nil. `TestClosedReturnsErrClosed` covers the after-return case (second `Close` once the first has returned). The mid-join case is required by rule 8 even though the public suite does not force it.

After `closed` is set:

- A `Get` that has not yet passed the closed check takes `mu`, sees `closed`, returns `ErrClosed`, and does not call the Loader.
- A waiter blocked in `wait` wakes on `doneClosing` (or on the next loop) and returns `ErrClosed`.
- A Loader that returns afterward is discarded in `runLoad` (no insert). "Drops all entries" remains true, including the zero-waiter case (load continues, then its success is dropped because `closed` is set) and the close-window case (load returns as cancel fires).

`Close` does not hold `mu` while waiting for Loaders. Loaders do not need `mu` to notice `baseCtx` cancellation. This is the deadlock boundary: `Get` never calls the Loader on its own stack, `Close` never waits while holding `mu`, and `runLoad` never waits on `Close`.

`doneClosing` is a `chan struct{}` created in `New` and closed once. `joined` is the same. `baseCtx` / `cancel` come from `context.WithCancel(context.Background())`.

### `Len`

```text
Len():
  c.mu.Lock()
  n := len(c.resident)
  c.mu.Unlock()
  return n
```

Resident-only. In-flight keys are absent from `resident`. Because eviction happens before insert, still under `mu`, `n` cannot exceed `capacity`. A completion that has not yet re-locked is invisible, which is the allowed one-critical-section lag.

## How each rule is satisfied

1. **Hit.** Under `mu`, `resident[key]` hit copies `entry.value`, moves the entry to the MRU head, and returns. `load` is not called. `TestHitReturnsCopy` asserts the second `Get` does not increment the call counter.

2. **Single-flight miss.** Installing `inflight[key]` under `mu` admits one flight. Other Gets for that key call `wait` on the same `flight`. Each successful waiter receives `clone(flight.result)`, not the shared slice. `runLoad` inserts into `resident` only on success. `TestSingleFlight` uses `testing/synctest`: 50 goroutines block inside one Loader; `synctest.Wait` observes `calls == 1` and `returned == false`, then release delivers `block:k` to every waiter.

3. **Error.** Non-nil `err` skips the insert, deletes `inflight`, and publishes `f.err`. A later `Get` finds neither resident nor in-flight and starts a new Loader call. `(non-nil bytes, non-nil err)` drops the bytes and returns `(nil, err)`. `TestErrorNotCached` expects `errors.Is(err, boom)`, a nil byte slice, then a second `Get` that loads `"ok"` with `calls == 2`.

4. **Ownership.** `runLoad` clones before publish. `Get` clones again on every return path that yields bytes (hit and shared success). The cache does not retain the Loader slice. Callers may mutate their slice. `TestHitReturnsCopy` overwrites the first result with `'X'` and requires the second `Get` to still be `"payload"`. Concurrent waiters in `TestSingleFlight` each compare equal to `"block:k"`; independent copies keep that true even if a waiter mutates (the public test does not mutate, but the clone does).

5. **Capacity / LRU.** Insert path evicts the tail while `len(resident) == capacity` before adding the new key, so the map length never exceeds capacity across an unlock. In-flight flights are not in `resident` and are not on `order`. `Len` is `len(resident)` under `mu`. `TestLenBounded` loads five keys at capacity 3 and requires `Len() <= 3` after each `Get` and `Len() == 3` at the end. `TestLRUEviction` is the recency scenario in rule 6's test: capacity 2, sequence `a, b, hit a, c` evicts `b`.

6. **Recency.** Hit calls `touch` (move to head). Successful admit inserts at head. After `a`, `b`, the tail is `a`. The hit on `a` moves `a` to head and leaves `b` at tail. Admitting `c` evicts `b`. A later `Get(a)` hits (`Loader(a) == 1`); `Get(b)` misses (`Loader(b) == 2`); `Loader(c) == 1`. Ties: if two entries were inserted without an intervening touch, the tail is the older one; the spec allows any least-recent entry.

7. **Caller context and orphaned loads.** `load` is invoked as `c.load(c.baseCtx, key)` only. `wait` returns `ErrClosed` if `closed`, otherwise `ctx.Err()` if the caller is done and `f.ready` is still false, otherwise the flight result. Cancelled waiters do not remove the flight. Success after the last waiter leaves still inserts, unless `Close` has discarded it. The public suite does not cancel a caller mid-flight; hidden tests can. The wait loop above is what makes priority deterministic under `-race` and under `synctest`.

8. **Close.** First `Close` sets `closed`, closes `doneClosing`, clears `resident`, cancels `baseCtx`, waits on `loading`, then signals `joined` and returns nil. Waiters wake with `ErrClosed`. A `Get` that takes `mu` after `closed` is set does not call the Loader. A Loader completion that observes `closed` or a cancelled `baseCtx` does not insert. Second `Close` waits on `joined` if the first has not finished, then returns nil. `TestClosedReturnsErrClosed` checks first `Close` returns nil, subsequent `Get` is `errors.Is(..., ErrClosed)`, and second `Close` returns nil.

9. **Concurrency.** All exported methods take `mu` for map and list access and drop it before blocking on a Loader, on `ctx.Done`, or on `loading.Wait`. The only cross-goroutine payload is written before `close(f.done)` and read after that close (or under `mu`). `go test -race` must be clean. No lock cycle exists: mutex is never held while waiting for a goroutine that needs the mutex. `Close` waiting on `loading` cannot deadlock a Loader, because `runLoad`'s `Done` is deferred and the Loader is required to return when `baseCtx` is cancelled.

## Edge cases the public suite does not pin, but the rules do

These must behave as specified so hidden tests cannot trip a shortcut that only passes `instrument/resolve_public_test.go`.

- Capacity 1: admitting a second successful key evicts the first. A hit on the sole key refreshes MRU and does not evict.
- Same key, capacity full of other keys, one in-flight: the in-flight key is not evicted; when it succeeds, eviction of some other resident happens at insert time.
- Many distinct cold keys in flight at once: each has its own flight and its own Loader call. They do not count toward `Len`. When they complete, inserts evict until `len(resident) == capacity`. Completion order determines who survives; each successful insert evicts the then-LRU resident if full.
- Duplicate key while a flight is in progress and the key is not resident: join, do not start a second Loader. This is the single-flight rule, including when the cache is at capacity.
- Loader returns `(nil, nil)`: success, resident, `Get` returns a nil or empty copy, second `Get` does not reload.
- Loader returns `(bytes, err)`: `Get` returns `(nil, err)`, key absent, next `Get` reloads.
- Caller cancels before acquiring `mu`: still must not start a Loader if the closed check fails; if the cache is open and the key is resident, a hit is allowed because the caller's ctx is not consulted on the hit path in the spec (priority (b) applies while a load is in flight). Practical choice that matches the wording: on the hit path, return the copy even if `ctx` is already Done. On the miss path, if `ctx` is already Done before the flight is installed, still either join an existing flight and return `ctx.Err()` immediately, or, if this caller would be the owner, start the load (so a single cancelled Get still populates on success — rule 7 says a load continues even if all waiters leave, which includes a waiter that was already done) and return `ctx.Err()` from `wait` without waiting. Starting the load for an already-cancelled sole caller matches "orphaned success populates." Do not special-case it into a synchronous call.
- Caller cancels after joining: return `ctx.Err()`, leave the flight running.
- Close during hit: the closed check and the map read share one critical section, so a `Get` either completes the hit or observes `closed`, never both.
- Close with no in-flight loads: `loading.Wait()` returns immediately, entries dropped, nil.
- Close with waiters and a blocked Loader: cancel `baseCtx`, Loader returns, `runLoad` discards, waiters already released with `ErrClosed` via `doneClosing` (do not depend on the Loader returning before waiters wake).
- `Get` after `Close` returned: `ErrClosed`, Loader not called.
- Concurrent `Len` during insert: observes 0..capacity inclusive, never capacity+1.
- Concurrent `Close` and `Get`: no race, no deadlock; result is either a normal get or `ErrClosed`.
- Second `Close` overlapping the first: both return nil; only one `loading.Wait` is the join; the other waits for `joined`.

## File layout

Under `worktree/` (module root):

- `cache.go` — `ErrClosed`, `Loader`, `Cache`, `New`, `cache`, `Get`, `Len`, `Close`, `wait`, `runLoad`, `clone`, LRU helpers.
- `blockcache_public_test.go` — already present; do not edit. It is the public suite copied into the module.

No other packages. No `init`. No global mutable state besides the `ErrClosed` sentinel.

## Test approach

Public suite (must pass, including `-race`):

- `go test` in `worktree/` runs `TestHitReturnsCopy`, `TestSingleFlight`, `TestErrorNotCached`, `TestLRUEviction`, `TestLenBounded`, `TestClosedReturnsErrClosed`, `TestNewPanics`.
- `go test -race` runs the same. `TestSingleFlight` is the race-sensitive overlap test; `synctest.Wait` requires the Loader to block in a durably waiting goroutine, which a channel receive is.
- `go vet` clean.

The public file uses `testing/synctest` and `t.Context()`, which matches `go 1.25` in `go.mod`. Do not raise or lower the Go version.

Hidden-behavior checks to run while implementing, as a private test file that is not part of the deliverable unless the task later asks for tests (the deliverable is the package; these tests are for the implementor to confirm rules the public suite leaves open, then they can stay as extra `_test.go` files if they do not change the API):

- Cancel the caller ctx while the Loader is blocked; assert that `Get` returns `ctx.Err()`, the Loader is still entered once, and after release a new `Get` hits without a second load (orphaned success populated).
- Cancel every waiter, then `Close` before the Loader returns; assert the late success is not stored (`Get` after a fresh cache is a different case — on this cache, after `Close`, `Get` is `ErrClosed`; assert via a Loader-side observe plus `Len()==0` before `Close` returns, and that `Close` unblocks only after the Loader returns).
- Overlapping `Close`: start `Close` while a Loader is blocked, start a second `Close`, release the Loader, both return nil.
- `(bytes, err)` discarded; nil error with bytes stored; capacity-1 eviction; two keys in flight completing into a full cache without `Len` ever exceeding capacity (poll `Len` from another goroutine under `-race`).
- Mutate the slice the Loader returned after it returns; a later `Get` still sees the pre-mutation bytes (proves the cache cloned).
- Mutate one waiter's result while another waiter still holds its result; the other waiter's bytes stay intact.
- 32 goroutines, mixed keys, mixed cancel, one `Close` at the end: `-race` clean and process exits (no leaked Loader goroutine because `Close` joins them).

Failure mode to avoid in tests: `t.Fatalf` inside a `synctest` bubble while other goroutines are parked. The public test already documents this. Private synctest tests must `t.Errorf` until goroutines are released, or they mask the bug as a bubble deadlock.

## Implementation order

1. Types, `New` panic checks, `ErrClosed`, empty `Get`/`Len`/`Close` enough to compile against the public tests.
2. Single-goroutine hit, miss, copy, error-not-cached.
3. LRU list and capacity, matching `TestLRUEviction` and `TestLenBounded`.
4. In-flight map, background `runLoad`, `wait` with close and caller-ctx priority.
5. `Close` cancel + `WaitGroup` join + idempotent `joined` channel.
6. `go test` and `go test -race` and `go vet`.
7. Private edge tests from the list above, then race again.

No implementation in this step. The next step writes `worktree/cache.go` only after this plan is accepted.
