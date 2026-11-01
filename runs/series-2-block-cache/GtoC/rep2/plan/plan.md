# Implementation plan: `blockcache`

Package `blockcache` (module already at `worktree/`, Go 1.25). One implementation file: `worktree/blockcache.go`. Public tests already live in `worktree/blockcache_public_test.go` (copy of `instrument/resolve_public_test.go`). No other packages. No findings file exists, so this is the initial plan.

## Public API

Match the spec exactly. Do not add exported types, helpers, or sentinel errors.

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
	Get(ctx context.Context, key string) ([]byte, error)
	Len() int
	Close() error
}

func New(capacity int, load Loader) Cache

var ErrClosed = errors.New("blockcache: closed")
```

`New` panics if `capacity < 1` or `load == nil` (before any other work). `Close` returns `nil` always, including the second and later calls. `Get` after close returns an error for which `errors.Is(err, ErrClosed)` is true. Wrapping is legal; the implementation returns `ErrClosed` directly. On a Loader error, `Get` returns `(nil, err)` so the public suite's nil-bytes check holds. A successful `Get` never returns a nil slice that aliases cache storage; it returns a freshly allocated copy (empty successful payload is a non-nil empty slice only if the loader returned a non-nil empty slice — copy preserves length, so a nil loader success becomes a nil copy, which is still a distinct "copy" with no retained alias).

## Internal data model

Unexported concrete type behind `Cache`:

```go
type cache struct {
	capacity int
	load     Loader

	ctx    context.Context    // cache-scoped; cancelled by Close
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	closing chan struct{}     // closed once, when Close begins; waiters select on it
	entries map[string]*entry // resident only
	lru     *list             // intrusive doubly-linked list, front = MRU, back = LRU
	inflight map[string]*flight

	loads sync.WaitGroup      // one Add per started Loader goroutine; Close waits
}

type entry struct {
	key  string
	val  []byte // owned by the cache; never handed out
	prev *entry
	next *entry
}

type flight struct {
	done chan struct{} // closed after result is published
	val  []byte        // owned copy, or nil
	err  error
	// ready is set under cache.mu immediately before done is closed,
	// so a waiter that loses the race and re-locks can read the result
	// without waiting if the flight was already deleted from inflight.
	ready bool
}
```

LRU is an intrusive list on `*entry` (sentinel head, or nil-empty with explicit `mru`/`lru` pointers). No `container/list` — avoids `any` and extra allocations, and eviction is O(1). Map + list together are the resident set. `Len()` is `len(entries)` under `mu`.

In-flight loads live only in `inflight`. They are not in `entries` and do not affect `Len()`. A key is in at most one of the two maps.

`flight.done` is closed exactly once, after `val`/`err`/`ready` are stored. Waiters that already hold a `*flight` may outlive its removal from `inflight`.

No `singleflight.Group`: the stock group cancels or shares the caller's context and does not keep an orphaned call running under a cache context. The flight map above is the single-flight table.

## Concurrency protocol

One mutex, `c.mu`, guards `closed`, `entries`, the LRU links, and `inflight`. `Len` and the lookup/insert/evict path take it. The Loader is never called while `mu` is held.

`loads` counts Loader goroutines only. It is `Add`ed under `mu` in the same critical section that inserts the `*flight`, before the goroutine is started, so `Close`'s `Wait` cannot miss a load that has been published to waiters.

Cache context is `context.WithCancel(context.Background())` created in `New`. Callers' contexts are never passed to `Loader` and never cancel it.

Close protocol (idempotent, concurrent-safe):

1. Lock `mu`. If `closed`, unlock and go to step 5 (join). Else set `closed`, `close(closing)`, `cancel()`, drop `entries` (set map to empty or nil; unlink is unnecessary once the map is discarded), leave `inflight` in place so finishing loaders can still find their flight and discard. Unlock.
2. Every current Get waiter is either selecting on `closing` or will observe `closed` on its next lock. They return `ErrClosed`.
3. `loads.Wait()` joins every Loader that was started. A Loader that has not yet been started cannot exist: `Add` happens before `go`, under the same lock that sets `closed`. A Get that races with Close either sees `closed` and does not start a load, or has already `Add`ed and started one, which Wait joins.
4. After Wait returns, no Loader is running. Lock, delete remaining `inflight` entries (their results were already discarded by the load goroutine because `closed` or `ctx` was done — see rule 8), unlock.
5. Concurrent Close calls all reach the same `loads.Wait()`. The first call performs steps 1–4; later calls that observe `closed` skip mutation and still `loads.Wait()`, which returns immediately once the first join has finished, or blocks until that join finishes if they arrive mid-close. Return nil.

`closing` is closed under `mu` before unlock, exactly once. Waiters treat it as the close signal with priority over their own `ctx.Done()` (rule 7).

## Get algorithm

```
Get(ctx, key):
  if ctx == nil: treat as context.Background() is NOT required; a nil ctx panics
                 on ctx.Done() in the standard library, and the spec always passes
                 a context. Do not special-case nil.

  c.mu.Lock()
  if c.closed:
      unlock; return nil, ErrClosed          // rule 8, before any Loader

  if e, ok := c.entries[key]:                 // hit
      move e to MRU
      val := clone(e.val)
      unlock
      return val, nil                         // no Loader

  if f, ok := c.inflight[key]:                // join existing flight
      unlock
      return c.wait(ctx, f)

  // miss: this caller becomes the single loader
  f := &flight{done: make(chan struct{})}
  c.inflight[key] = f
  c.loads.Add(1)
  unlock
  go c.runLoad(key, f)
  return c.wait(ctx, f)
```

`runLoad` (the only goroutine that calls `Loader`):

```
runLoad(key, f):
  defer c.loads.Done()
  val, err := c.load(c.ctx, key)          // cache context, not caller
  if err != nil {
      val = nil                           // error wins; discard bytes (rule 3)
  } else {
      val = clone(val)                    // own the bytes; drop loader slice (rule 4)
  }

  c.mu.Lock()
  defer c.mu.Unlock()
  delete(c.inflight, key)                 // always; flight object stays with waiters

  store := err == nil && !c.closed && c.ctx.Err() == nil
  // ctx.Err() covers cancel that already happened; closed is the same window
  // plus the explicit flag. Both are checked so a loader that returns after
  // cancel, even if it ignored ctx, is not stored (rule 8).

  if store:
      // admit: evict LRU while len(entries) >= capacity
      for len(c.entries) >= c.capacity:
          evict back of LRU list (delete map entry, unlink)
      insert new entry{key, val} at MRU
      // in-flight never occupied a slot, so this is the first time it counts

  f.val, f.err, f.ready = val, err, true  // publish before close(done)
  close(f.done)
  // if !store, f.val is nil and f.err is nil on the cancel/close discard path
  // only when the loader itself succeeded. Waiters must not observe that as a
  // hit: they already returned ErrClosed via closing, or they are waiting.
  // A waiter still in wait() for a discarded success must not receive the bytes.
```

Waiter result when the load is discarded because of close: the waiter returns `ErrClosed` (priority a), not the discarded success. When the load is discarded because the cache context was cancelled, Close is in progress or done, so `closing` is already closed and waiters take priority (a). There is no path that cancels the cache context except Close.

`wait`:

```
wait(ctx, f):
  select {
  case <-c.closing:                         // priority (a), even if f.done also ready
      return nil, ErrClosed
  case <-ctx.Done():
      select {
      case <-c.closing:                     // close wins if both became ready
          return nil, ErrClosed
      default:
          return nil, ctx.Err()            // priority (b); load keeps running
      }
  case <-f.done:
      select {
      case <-c.closing:                     // close wins over a just-finished load
          return nil, ErrClosed
      default:
      }
      // copy under no lock: f.val/f.err are immutable after done is closed
      if f.err != nil {
          return nil, f.err
      }
      return clone(f.val), nil             // each waiter its own copy (rule 2, 4)
  }
```

Select chooses randomly when multiple cases are ready, so a bare three-way select can hand a waiter the load result after Close has already signalled. The nested checks above make `ErrClosed` win whenever `closing` is already closed at the moment the waiter proceeds. That matches rule 7's priority and rule 8's "wakes every current waiter with ErrClosed".

A waiter whose own ctx is already cancelled before it enters `wait` takes the `ctx.Done` case (or `closing` if closed). It does not start a second load: the flight already exists. The orphaned load still runs and, if it succeeds before Close, still populates the cache (rule 7).

Double-check after `f.done` is not required for publication safety: the loader writes `val`/`err`/`ready` before `close(done)`, and Go's channel close happens-before receives. Waiters that never reach `wait` because they re-acquire `mu` and find the key resident take the hit path instead. That happens when a load completes and is stored between a joiner's unlock and its wait — no: joiners snapshot `*flight` before unlock, so they always `wait` on that flight even if a later Get sees the resident entry. Both paths return equal copies.

Stale flight vs new flight: `runLoad` deletes `inflight[key]` under `mu` before `close(done)`. A new Get can start a new flight for the same key only after that delete. The old flight object is not reused. Error path deletes and does not insert, so the next Get is a fresh miss (rule 3). Success path deletes from `inflight` and inserts into `entries` under the same lock, so a concurrent Get cannot observe "neither" and start a duplicate load, and cannot observe both.

## How each rule is satisfied

1. **Hit.** Lookup in `entries` under `mu`. Return `clone(entry.val)`, `moveToFront` (MRU). Loader is not called. Second Get of a resident key does not increment the loader call count (`TestHitReturnsCopy`).

2. **Single-flight miss.** First miss inserts one `*flight` and starts one goroutine. Concurrent Gets see `inflight[key]` and call `wait` on that same flight. `Loader` is invoked once, with the cache context. Each waiter clones `f.val`. `TestSingleFlight`: 50 goroutines block in `wait` (or inside the loader's `<-release`) until `release` is closed; `synctest.Wait` observes `calls == 1` and `returned == false`. The loader blocks on a channel receive, which synctest treats as a durable block. No other goroutine blocks except the waiters on `f.done` / `ctx` / `closing`, all of which are channel operations, so the bubble is idle and `synctest.Wait` returns.

3. **Error.** If `load` returns non-nil `err`, bytes are set to nil and not inserted. `inflight` entry is removed. Every waiter returns `(nil, err)`. A later Get finds neither resident nor in-flight and calls `Loader` again. `(non-nil, non-nil)` is treated as error; bytes are discarded and not copied into the cache. `TestErrorNotCached` expects `errors.Is` and a nil byte slice on the first call, then a successful reload.

4. **Ownership / copy.** `clone` allocates with `make([]byte, len(b))` and `copy`. Used (a) when adopting the loader result into `entry.val` / `f.val`, and (b) on every return from Get, hit or flight. The loader's slice is not stored. Mutating a returned slice cannot change `entry.val` or another caller's slice. Empty/nil: `clone(nil)` returns nil; that is only reachable if a successful loader returned nil, and the public tests use non-nil payloads. On error, return nil bytes without cloning.

5. **Capacity / LRU.** `len(entries) <= capacity` is an invariant restored before every insert: while full, unlink and delete the LRU end. In-flight keys are absent from `entries`, so they do not count and cannot be evicted. They become resident only in `runLoad` after success, under `mu`, after eviction. `Len()` returns `len(entries)` while holding `mu`, so it never observes a map larger than capacity and never counts flights. "May lag a completion by one critical section" is the window where `runLoad` has the result but has not yet acquired `mu` to insert; a concurrent `Len` still sees the old resident set. After that critical section, `Len` includes the new entry and excludes any victim. `TestLenBounded` loads five keys at capacity 3 and checks `Len() <= 3` after each Get and `Len() == 3` at the end.

6. **Recency.** Hit path moves the entry to MRU before unlock. Successful admit inserts at MRU (newest). Eviction always removes the opposite end. Ties: a single LRU pointer has no tie; if two entries were never ordered, insertion order is the order. The spec allows any least-recent on a true tie. `TestLRUEviction`: capacity 2; load `a`, load `b` (a is LRU), hit `a` (b is LRU), load `c` evicts `b`; later Get `a` is a hit (one loader call), Get `b` reloads (two calls).

7. **Caller context and orphaned loads.** `Loader` receives `c.ctx` only. `wait` returning on `ctx.Done` does not cancel `c.ctx`, does not delete the flight, and does not call `loads.Done`. The load goroutine still calls `load`, and on success still inserts. Priority in `wait` is close, then caller ctx, then load result, with the nested selects in the algorithm section so a ready `closing` is not lost to a random select. A Get that arrives after the flight completed and was stored takes the hit path and does not see the original caller's cancellation.

8. **Close.** Sets `closed`, closes `closing`, cancels `c.ctx`, drops `entries` (so `Len()` becomes 0 and hits disappear), then `loads.Wait()` until every started Loader returns. Waiters blocked in `wait` unblock on `closing` and return `ErrClosed`. A Loader return after cancel observes `c.closed` or `c.ctx.Err() != nil` under `mu` and does not insert; any bytes are discarded. A Get that has not passed the closed check takes the `c.closed` branch and does not call `Loader`. Second `Close` sees `closed`, waits on the same `WaitGroup` (already satisfied or still joining), returns nil. `Close` itself returns nil. `TestClosedReturnsErrClosed` covers post-close Get and idempotent Close. In-flight close is not in the public suite but is required by the spec and covered by the private tests below.

9. **Concurrency.** All exported methods take `mu` or use the WaitGroup / channels as specified. No lock is held across `load` or across `wait`. Lock order is unique (`mu` only), so no lock-cycle deadlock. `loads.Add` happens before `go` and before unlock, so `Wait` cannot return early. Channel closes happen once (`closing`, each `flight.done`). Map writes happen only under `mu`. `clone` reads byte slices only after they are published (stored under `mu`, or after `<-f.done`). Race detector should be clean. Goroutine count is bounded by the number of distinct in-flight keys, and each exits after `load` returns, including after cancel, because a compliant Loader returns when `c.ctx` is done. A non-compliant Loader can stall `Close`; the spec requires Loaders to return promptly on cancel, so `Close` is allowed to wait.

## Copy helper

```go
func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
```

## Files to write

- `worktree/blockcache.go` — package implementation only.
- Do not modify `blockcache_public_test.go` or `go.mod`.
- Do not add a README.

## Test approach

Public suite (already present; run, do not edit):

| Test | Rules |
|---|---|
| `TestHitReturnsCopy` | 1, 4 |
| `TestSingleFlight` | 2, 4 (equal bytes; synctest overlap) |
| `TestErrorNotCached` | 3 |
| `TestLRUEviction` | 5, 6 |
| `TestLenBounded` | 5 |
| `TestClosedReturnsErrClosed` | 8 (after close, idempotent Close) |
| `TestNewPanics` | New preconditions |

Commands from `worktree/`:

```
go test -count=1 .
go test -race -count=1 .
go vet .
```

`TestSingleFlight` uses `testing/synctest`, which is in the standard library on Go 1.25. The bubble stays valid only if every block is a channel/select/sleep that synctest knows: the plan uses `chan struct{}` for flight completion, close, and the test's release channel, and does not spawn extra helper goroutines inside Get.

Private tests, added only if a local check is needed during implementation, then removed so the delivered tree matches the public suite plus `blockcache.go`. They are not part of the deliverable unless the grader ignores extra `_test.go` files. Prefer running them via `go test` in a temp copy or as a deleted file so the worktree stays clean. Cases the public suite does not pin, which the implementation must still satisfy:

- Concurrent Gets for one key, caller cancel on a subset: cancelled waiters get `ctx.Err()`, the loader runs once, a waiter that stays receives the bytes, and a later Get hits (no second load). Proves orphaned-load population (rule 7).
- Close while a Loader is blocked, then the Loader returns: Close blocks until the Loader returns, waiters get `ErrClosed`, the key is not resident, a post-close Get does not call Loader. Use a started channel so Close begins only after the Loader has entered.
- Two concurrent `Close` calls while a Loader is blocked: both return nil, and neither returns before the Loader returns.
- `( []byte("x"), err )` from Loader: waiters get the error and nil bytes; a following Get reloads; the discarded slice is not served.
- Capacity 1 with an in-flight key and a different resident key: `Len()==1` during the flight; the in-flight key is not evicted; on success the resident LRU key is evicted if it is the victim and `Len()==1`.
- Several distinct keys in flight at once with capacity 1: `Len()` stays 0 until completions; completions admit one by one and evict so `Len()` never exceeds 1. Completion order determines the survivor.
- Hit under contention: many goroutines Get the same resident key while another key is loaded to force eviction of an untouched key. The hot key survives.
- Successful empty payload `[]byte{}`: Get returns a non-nil empty slice that the caller can mutate (append) without affecting the next Get.
- Idempotent Close with no prior Get, and Get of a never-seen key after Close, both return `ErrClosed` and do not call Loader.
- `errors.Is` on a wrapped loader error is preserved (return the loader error unchanged, do not wrap it).

Race focus: `go test -race` on the public suite plus a short hammer (N goroutines, mixed keys, capacity 2, one closer) if used during development.

## Invariants (check during implementation)

- `len(entries) <= capacity` whenever `mu` is not held by the inserter mid-update; always true on unlock.
- A key is never in both `entries` and `inflight`.
- `flight.done` is closed at most once, and only after `val`/`err` are written.
- `loads` counter returns to zero after every load goroutine exits, including error, success, and cancel paths (`defer Done`).
- `Close` does not return while any `runLoad` is still inside `c.load`.
- No call to `c.load` occurs while `closed` is true.
- Every byte slice stored in `entry.val` or `flight.val` was allocated by `clone`, not by the caller or the Loader.

## Risks to avoid

- Holding `mu` while calling `Loader` or while waiting: deadlocks Close and serializes unrelated keys; also prevents `synctest.Wait` from observing an in-flight load if the loader cannot start.
- Using the caller's context as the Loader context: a cancelled waiter would cancel the shared load and violate orphaned-load population.
- Caching errors or caching `(bytes, err)`: fails `TestErrorNotCached` and rule 3.
- Returning the resident slice without copying: fails the mutation check in `TestHitReturnsCopy`.
- Counting in-flight keys in `Len` or inserting them before the load returns: can push `Len` above capacity or evict a key that is not yet resident.
- Storing a load that returns after `cancel()`: violates "drops all entries" / discard-after-cancel.
- `Close` that does not `Wait` on loaders: violates rule 8 and can race the loader's map write with a discarded cache.
- A select that can return the load result when `closing` is already closed: violates waiter priority.
- Starting the load goroutine before `WaitGroup.Add`: Close can return and the late `Add` panics.
- Evicting an arbitrary map entry instead of the LRU end: fails `TestLRUEviction`.
