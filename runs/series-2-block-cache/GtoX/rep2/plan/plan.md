# Implementation plan: `blockcache`

Bounded concurrent single-flight block cache. Package `blockcache`, module already at `worktree/go.mod` (`go 1.25`). No third-party dependencies. Public tests live in `instrument/resolve_public_test.go` and the worktree copy `blockcache_public_test.go`. Do not edit the public tests to make them pass.

This revision addresses `plan/plan_findings.md`. The point-by-point response is `plan/plan_rebuttal.md`.

## Public API

Exact surface from the spec. Do not add exported names.

```go
package blockcache

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

- `New` panics if `capacity < 1` or `load == nil`. Panic value is unspecified; the public test only checks `recover() != nil`.
- `ErrClosed` is a sentinel. Callers use `errors.Is`. Return the sentinel itself (wrapping is legal, not used).
- `Close` always returns `nil`, including a second call and a call that overlaps an in-progress `Close`.
- Concrete type is unexported (`*cache`). `New` returns the interface. A compile-time check `var _ Cache = (*cache)(nil)` keeps the method set honest.
- Keys are opaque strings. `""` is a valid key. No normalization.
- Doc comments on the exported declarations match the spec so the contract sits next to the code.

## Internal data model

One mutex owns the maps, the LRU list, the flight result fields, and the closed flag. Loads run outside the lock. Waiters never hold the lock while blocked.

```go
type cache struct {
    mu       sync.Mutex
    capacity int
    load     Loader

    resident map[string]*entry // key -> resident entry; len <= capacity at every unlock
    order    *list.List        // front = MRU, back = LRU; Value is *entry
    inflight map[string]*flight

    base   context.Context    // passed to every Loader; never a caller's ctx
    cancel context.CancelFunc // first Close calls this once
    closed bool
    wg     sync.WaitGroup     // one Add per started Loader goroutine; Done after publish
    closeMu sync.Mutex        // serializes Close; overlapping Close waits for the same join
}

type entry struct {
    key  string
    val  []byte        // immutable cache-owned copy; never the Loader's slice
    elem *list.Element // position in order
}

type flight struct {
    done chan struct{} // closed under mu after val/err are final; never sent on
    val  []byte        // immutable cache-owned copy on success; nil on error
    err  error         // Loader error, or nil
}
```

`container/list` is the LRU. Front is most-recently-used, back is least-recently-used. Promotion and insertion are strictly ordered under `mu`, so the back element is the unique least-recent entry. The spec allows any least-recent entry on a tie; a total order satisfies that.

Why not `sync.Map` or `golang.org/x/sync/singleflight`: admission, eviction, the closed flag, and flight publication must be one critical section. `singleflight` shares one result slice, forgets differently, and does not implement orphan retention or close-discard.

`base` is `context.WithCancel(context.Background())` created in `New`. Every Loader is called as `load(c.base, key)`. Do not derive the Loader context from the caller, including via `context.WithoutCancel`. Do not cancel a load when its last waiter leaves. A child context per load is unnecessary: `base` is already cache-scoped, and deriving one would add a cancel that must be deferred. Pass `base` directly.

Cache-owned slices (`entry.val`, `flight.val`) are never written after `clone` returns. The cache has no mutate API. Internal aliasing between `flight.val` and `entry.val` is allowed. Aliasing to a Loader buffer or to a slice returned from `Get` is forbidden.

## Copy helper

`append([]byte(nil), src...)` is the wrong clone. Appending zero elements to a nil slice returns nil, so a successful non-nil empty Loader result (`[]byte{}` or `make([]byte, 0)`) would collapse to nil. Callers must be able to observe the Loader's successful value, including nilness.

```go
// clone preserves nilness. nil -> nil; non-nil empty -> new non-nil empty slice;
// non-nil non-empty -> new slice with the same bytes.
func clone(src []byte) []byte {
    if src == nil {
        return nil
    }
    dst := make([]byte, len(src))
    copy(dst, src)
    return dst
}
```

`make([]byte, 0)` is non-nil. Do not replace this with `append`. `bytes.Clone` is also the wrong helper: it returns nil for both nil and empty input.

On the error path, do not clone. A Loader result of `(non-nil bytes, non-nil err)` publishes `val == nil` and that error. `Get` returns a nil slice, not `[]byte{}` and not a copy of the discarded bytes. `TestErrorNotCached` requires `b == nil` on error.

## Concurrency protocol

`mu` is held only for map, list, flight-field, and flag updates, and for the closed check. It is never held across `Loader`, across a blocking wait, or across `wg.Wait`.

Lock order: `closeMu` then `mu`, and only `Close` takes `closeMu`. `Get` and `Len` take only `mu`. No goroutine waits on another goroutine while holding `mu`.

`context` cancel does not run the Loader and does not take `mu`. `Close` still calls `cancel` only after it has released `mu`, so the lock body stays limited to memory updates.

### Get

`Get` is one decision under `mu`, then either an immediate return or a wait. There is no second closed check with different rules.

1. Lock `mu`.
2. If `closed`, unlock and return `(nil, ErrClosed)`. Do not call `Loader`. This is the rule 8 gate. It beats a caller ctx that is already Done.
3. If `resident[key]` exists, `MoveToFront`, snapshot the `entry.val` header, unlock, return `clone(snap)`. Do not consult the caller ctx. Rule 1 has no ctx clause. Rule 7's ctx clause applies to a waiting Get on an in-flight load. A hit does not wait and does not call `Loader`. Snapshot-then-clone outside the lock is race-free because the bytes are immutable and the snapshot keeps the array alive if a later eviction drops the entry.
4. If `inflight[key]` exists, take that `*flight`, unlock, and wait on it (below). Do not start a Loader.
5. Otherwise this caller is the starter. Create `*flight{done: make(chan struct{})}`, insert it in `inflight`, `wg.Add(1)`, unlock, start the Loader goroutine, then wait on the same flight. The starter is also a waiter. `wg.Add` happens before unlock and before `go`, so `Close` cannot `Wait` past a flight it did not see: either this critical section observed `!closed` and Add already happened-before `Close`'s later acquisition of `mu`, or it observed `closed` and did not Add.

A key is never both resident and inflight. Miss handling checks resident first, then inflight, under one lock hold. The publisher removes the flight and optionally admits under one lock hold.

Do not start the Loader inline on the caller's goroutine. Rule 7 requires the load to continue after every waiter has returned. The Loader runs on its own goroutine; the caller only waits.

A pre-cancelled caller ctx does not skip step 5. The uniform miss path is easier to keep correct: start or join one flight, then let arbitration apply rule 7. The load becomes an orphan if every waiter leaves, which rule 7 already requires the cache to tolerate. Joining an existing flight and then short-circuiting on `ctx.Err()` without arbitration would be wrong: the load may already have resolved, and a resolved result beats caller cancellation.

Loader must not call `Get` on this cache for the same key it is loading. That flight completes only when the Loader returns, so the re-entrant `Get` would wait on itself. `Get` for a different key is safe because the Loader does not hold `mu`. The cache does not try to detect re-entry (no goroutine ids).

### Wait arbitration

Every wake goes through one locked arbitration. No `select` case returns a result. A previous sketch that returned `clone(flight.val), flight.err` directly from `case <-flight.done` was wrong: `select` chooses randomly when `done` and `base.Done()` are both ready, and that path skipped the closed check.

```text
for {
    if bytes, err, decided := arbitrate(ctx, flight); decided {
        return bytes, err
    }
    select {
    case <-ctx.Done():
    case <-flight.done:
    case <-c.base.Done():
    }
}
```

`arbitrate` holds `mu` for the whole decision and uses this priority, with no further exceptions:

| `closed` | flight `done` closed | caller `ctx` Done | decision |
| --- | --- | --- | --- |
| yes | any | any | `(nil, ErrClosed)` |
| no | yes, `err != nil` | any | `(nil, err)` — discard bytes |
| no | yes, `err == nil` | any | `(clone(val), nil)` |
| no | no | yes | `(nil, ctx.Err())` |
| no | no | no | not decided; loop |

Rule 7's "while the load is still in flight" is why a resolved flight beats caller cancellation, and why `closed` still beats a resolved flight. Check `closed` first, then `done`, then `ctx.Err()`. Implement the `done` test with a non-blocking `select` under `mu`. Read `flight.val` and `flight.err` only under `mu` and only after that test sees `done` closed. Clone the success bytes after unlocking, from a snapshotted header, same immutability argument as the hit path. On error, return a nil slice without cloning.

The linearization point of a waiting `Get` is this critical section. If it observes `!closed` and a resolved flight, the `Get` is no longer waiting and may return that result even if `Close` acquires `mu` immediately after. If it observes `closed`, it returns `ErrClosed` even when `done` is also closed and `val` is populated. That is the rule 7 / rule 8 priority. Returning from the `done` channel case without taking `mu` is forbidden; it is the bug where a waiter observes a post-cancel Loader result and hands those bytes to the caller.

Wakeups cannot be missed. The only state changes that unblock a waiter are closing `flight.done`, cancelling `base`, or the caller ctx becoming Done. All three close a channel the `select` waits on. If the change happens after `arbitrate` returns undecided and before `select`, the channel is already closed and `select` returns immediately. Then `arbitrate` runs again.

`Close` does not join waiter goroutines. Waiters are the caller's goroutines. They leave via arbitration. `Close` wakes any waiter blocked in `select` by cancelling `base`.

### Publisher (Loader goroutine)

```text
defer wg.Done()          // runs on goroutine exit, after the steps below
result, err := load(c.base, key)
if err != nil {
    result = nil         // error wins; drop the Loader slice
} else {
    result = clone(result)
}
c.mu.Lock()
if !c.closed && err == nil {
    admit(key, result)   // evict until len < capacity, then PushFront
}
f.val, f.err = result, err
close(f.done)            // publish completion
if c.inflight[key] == f {
    delete(c.inflight, key)
}
c.mu.Unlock()
```

Publication and removal are one critical section, in that order: write `val`/`err`, close `done`, then delete `inflight`, then unlock. A `Get` cannot observe "key absent from both maps" until the previous flight's result is already visible to anyone holding `*flight`. That removes the window where an errored flight was deleted, `mu` released, and `done` not yet closed, so a new `Get` started a second Loader while the first flight's waiters were still blocked.

`close(done)` does not block. Doing it under `mu` cannot deadlock: waiters do not hold `mu` while receiving on `done`. They wake, block on `mu`, and arbitrate after the publisher unlocks.

Admission is the publisher's job, not the waiter's. A success with zero remaining waiters still populates the cache when `!closed` (rule 7 orphan). A success observed while `closed` is not inserted (rule 8, including the zero-waiter close window). `closed` is the only discard switch. Do not also branch on `base.Err()`; nothing else cancels `base`, and `Close` sets `closed` before `cancel`.

`wg.Done` runs only after the admission decision. `Close`'s `wg.Wait` therefore cannot finish and return while the publisher might still insert. A deferred `Done` at the start of the goroutine, with admit/publish before return, is the required order. Do not `Done` before the lock.

`admit` while `len(resident) >= capacity` removes `order.Back()` (map and list) until there is room, then `PushFront`s the new entry and stores it in `resident`. Evict before insert, under the same lock hold. The new key is not resident yet, so it cannot evict itself. `Len` never observes `capacity+1`. An in-flight key is not in `order`, so it is not evictable and does not consume a slot. It becomes resident only in this success-and-not-closed step.

On success, `flight.val` and `entry.val` may alias the same cache-owned slice. Waiters and later hits clone on the way out. On error, nothing is admitted, `flight.val` is nil, and the Loader slice is not retained.

The identity check before `delete` is defensive. `Close` does not replace `inflight`. Only the publisher removes its own flight. A newer flight for the key cannot exist until this delete has happened, because starters insert only when the key is absent.

After unlock, the Loader call has already returned. A later `Get` may start a new flight for an error (or for a discarded close result) before this goroutine executes `wg.Done`. Those are two sequential Loader calls, not two in-flight misses: the first call has returned and its result is published. `Close` still waits until `Done`, so the cleanup tail is joined.

### Len

```text
c.mu.Lock()
n := len(c.resident)
c.mu.Unlock()
return n
```

Resident-only. In-flight keys are not in `resident`. Eviction and insert share one critical section, and `Len` takes the same mutex, so a concurrent poll never sees a value above capacity. A completion that has not yet taken `mu` to admit is invisible; that is the allowed one-critical-section lag. After `Close` replaces `resident`, `Len` is 0. `Len` does not take `closeMu` and does not wait for Loaders.

### Close

```text
c.closeMu.Lock()
defer c.closeMu.Unlock()
c.mu.Lock()
if c.closed {
    already := true
} else {
    c.closed = true
    c.resident = make(map[string]*entry) // one assignment; Len sees old map or empty
    c.order = list.New()
}
c.mu.Unlock()
if already {
    return nil
}
c.cancel()   // wakes Loaders and waiters blocked in select
c.wg.Wait()  // every Loader goroutine has finished admit/publish and Done
return nil
```

The sketch above is the control flow. `already` is a local. The important change from a previous sketch is where `closeMu` is released.

- The first `Close` holds `closeMu` across `cancel` and `wg.Wait`. A second `Close` that arrives during that join blocks on `closeMu` until the same join finishes, then acquires `closeMu`, sees `closed`, and returns nil. It does not start a second join. That is the spec's "a second Close arriving while the first is still joining waits for that same join and returns nil."
- Do not release `closeMu` before `wg.Wait` and then re-lock only to read `closed`. That would let the overlapping call return while the first call is still inside `wg.Wait`, which violates the join clause.
- A `Close` that arrives after the first has unlocked `closeMu` is not "arriving while the first is still joining." It sees `closed` under `mu` and returns nil without calling `cancel` or `Wait`. The join has already finished. `WaitGroup.Wait` after the count is zero would also return immediately, but skipping it keeps the single-join story obvious.
- `closed = true` happens before `cancel` and before `Wait`. A `Get` that has not passed the closed check takes `mu`, sees `closed`, and returns `ErrClosed` without `wg.Add` and without calling `Loader`.
- Replacing `resident` and `order` in one assignment each drops every entry without a half-cleared map visible to `Len`. Do not touch `inflight` here. Publishers delete their own keys under `mu`. Replacing the map would race with that protocol.
- `cancel` is outside `mu`. Any waiter that locks `mu` after `closed` was set sees `closed`, including a waiter woken by `base.Done()`: `closed = true` happens-before unlock happens-before `cancel` happens-before the waiter runs again and locks `mu`.
- `wg.Wait` is outside `mu`. The publisher needs `mu` to decide admission and to publish. Waiting while holding `mu` deadlocks. `wg.Wait` stays inside `closeMu`.
- Waiters blocked in `select` wake on `base.Done()` and arbitration returns `ErrClosed` without waiting for the Loader. `Close` itself does wait for the Loader. Both are required.
- A Loader that returns after `closed` is set finds `closed` in the publisher and does not admit. Its flight is still published and removed so waiters and the inflight map are not left dirty. Arbitration will prefer `ErrClosed` for any waiter still in `arbitrate`.
- First `Close` calls `cancel` once. Later `Close` calls do not call `cancel` again. `CancelFunc` is safe to call twice; the flag makes the single call obvious.
- `Close` returns nil in every path.

Do not call the Loader from `Close`. Do not hold `mu` across `cancel` or `Wait`.

## How each rule is satisfied

1. **Hit.** Under `mu`, resident lookup, `MoveToFront`, clone of the cache-owned slice, no Loader call. `TestHitReturnsCopy`: second `Get` does not increment the call counter; overwriting the first slice does not change the second `Get`.

2. **Single-flight miss.** The `Get` that finds neither resident nor inflight is the only starter: it inserts one `*flight` and starts one goroutine. Later `Get`s share that flight until the publisher, still holding `mu`, has closed `done` and deleted the flight. Each waiter clones. Success is admitted in that same publisher section, so a `Get` that arrives after unlock hits instead of loading again. `TestSingleFlight`: 50 goroutines, `synctest.Wait` while the Loader is blocked on `release`, expects `calls == 1` and `returned == false`, then after `close(release)` every result equals `block:k`. The Loader goroutine is started inside the synctest bubble because `Get` is called from the bubble. The cache uses no timers.

3. **Error.** Non-nil `err` skips `admit`, forces `val` to nil, publishes that error, then removes the flight, under one lock hold. Every waiter already holding `*flight` observes that error and a nil slice, unless `closed` wins arbitration. A later `Get` sees neither resident nor inflight and loads again. `TestErrorNotCached`: first error is `errors.Is(err, boom)` with nil bytes; second returns `ok`; `calls == 2`. The Loader error is returned as-is, not wrapped, so `errors.Is` and equality both work. `(non-nil bytes, non-nil err)` is the same path: bytes dropped, error returned, nothing stored.

4. **Ownership / copy.** `clone` on successful publish into cache-owned storage. `clone` again on every `Get` return, hit or waiter. Error returns a nil slice, not a copy of discarded bytes. `clone` preserves nil versus non-nil empty. Callers never receive `entry.val`, `flight.val`, or the Loader's slice. Mutating a returned slice cannot affect the cache or another caller. Two waiters do not share a backing array.

5. **Capacity / LRU.** Admit evicts `order.Back()` until `len(resident) < capacity`, then inserts, under the same `mu` hold that `Len` uses. In-flight flights are not in `resident` or `order`. They become resident only in the success-and-not-closed publish step. `TestLenBounded`: capacity 3, five successful gets, `Len() <= 3` after each, final `Len() == 3`.

6. **Recency.** Hit: `MoveToFront`. Successful admit: `PushFront`. Failed load and close-discard: no recency change. `TestLRUEviction`: capacity 2; load `a`, load `b` (a is LRU), hit `a` (b is LRU), load `c` evicts b; later `Get(a)` does not reload (`count(a) == 1`), `Get(b)` reloads (`count(b) == 2`), `count(c) == 1`.

7. **Caller context and orphaned loads.** Loader receives `c.base`, never the caller ctx. Waiter departure does not cancel the load. Admission does not require a live waiter. A waiting `Get` uses the arbitration table: `ErrClosed`, else the resolved result (nil bytes if the Loader returned an error), else `ctx.Err()` while the flight is unresolved. Leaving a flight in `inflight` until the Loader returns is intentional retention, not a leak.

8. **Close.** Set `closed`, drop resident entries, `cancel`, `wg.Wait` for every Loader goroutine including its admission decision, return nil. Idempotent via `closed` plus `closeMu` held across the join. Post-close Loader results are not stored. A `Get` that loses the race at the closed check returns `ErrClosed` and does not call `Loader`. A `Get` already waiting is woken and arbitration returns `ErrClosed` even if the Loader has published bytes. `TestClosedReturnsErrClosed`: first `Close` is nil, `Get` is `errors.Is(..., ErrClosed)`, second `Close` is nil.

9. **Concurrency.** Exported methods use only the locks above. `flight.val` / `flight.err` are written under `mu` before `close(done)` and read under `mu` after `done` is observed closed. Immutable cache-owned bytes may be read outside `mu` after the header is snapshotted under `mu`. `go test -race` must be clean. No deadlock: the Loader goroutine is the only one `Close` waits for, and that goroutine needs `mu` only when `Close` is not holding it.

## Edge cases the public suite does not cover (still required)

- All waiters cancel while the Loader is running and the cache stays open: Loader still finishes; success is admitted; cancelled waiters already returned `ctx.Err()` from arbitration because the flight was unresolved. A later `Get` hits.
- Load resolves and the caller ctx is also Done, cache still open: arbitration returns the load result, not `ctx.Err()`.
- `Close` races with resolution: arbitration returns `ErrClosed`, nil bytes, even if `flight.done` is closed and `val` holds the payload. The publisher does not admit after it observes `closed`.
- `Close` while a Loader is blocked: waiters already in `select` return `ErrClosed` without waiting for the Loader; `Close` waits for the Loader. The Loader must return when `base` is Done. A Loader that ignores cancel hangs `Close`. That is the Loader breaking its contract.
- `Close` overlapping a `Get` that is about to call `Loader`: the closed check and `inflight` insert share one critical section. Either `Get` inserts and Adds before `closed` is set, and `Close` waits for that goroutine and the publisher discards the result, or `Get` sees `closed` and does not start a Loader.
- `(non-nil, err)` from Loader: waiters get `(nil, err)`; nothing stored; a later `Get` reloads.
- Successful `(nil, nil)` is stored and returned as a nil slice. Successful non-nil empty is stored and each `Get` returns a fresh non-nil empty slice.
- Capacity 1: a successful load of a second key evicts the only resident entry. The new key is inserted at front after the eviction, so it is the one that remains.
- Many in-flight keys with a small capacity: `Len` stays at the resident count. Completions admit one at a time and evict down to capacity. Completion order chooses the survivor. The spec does not require a particular winner among loads that were not yet resident.
- Concurrent `Len` during admit: never above capacity.
- Several concurrent `Close` calls: the ones that arrive while the first still holds `closeMu` block until that join finishes; all return nil; later `Get` is `ErrClosed`.
- Hit of one key while another key is in flight: independent. Same key cannot be both resident and inflight.
- Empty string key: ordinary map key, no special case.

## Invariants to keep visible in the code

- `len(resident) <= capacity` at every unlock of `mu`.
- A key is in at most one of `resident` and `inflight`.
- `wg` count equals the number of Loader goroutines started and not yet finished. `Add` happens under `mu` before the goroutine starts and only after observing `!closed`. `Done` happens only after that goroutine's admission decision and flight publication.
- `flight.done` is closed exactly once, under `mu`, after `val` and `err` are final, and before the flight is removed from `inflight`.
- No waiter returns a load result except from `arbitrate`, and that function checks `closed` before the resolved result.
- `Loader` is invoked only from the goroutine started after a `Get` inserted a new flight and observed `!closed`.
- Cache-owned byte slices are never aliased to a Loader buffer or to a slice returned from `Get`.
- `clone` is the only byte copier, and it preserves nilness.
- Error results published to waiters have a nil `val`.
- `mu` is not held across `Loader`, blocking receive, `cancel`, or `wg.Wait`.
- `closeMu` is held by the first `Close` from before `cancel` until after `wg.Wait` returns.

## Files

- `worktree/cache.go` — `ErrClosed`, `Cache`, `Loader`, `New`, `cache`, `Get`, `Len`, `Close`, `clone`, flight publisher.
- `worktree/blockcache_public_test.go` — already present; do not edit.
- Optional `worktree/cache_test.go` — private tests below. Not part of the exported API. Package `blockcache`, black-box through `Cache`.
- No other files. Keep the package in the module root so `package blockcache` matches the public tests.

## Test approach

Public suite is the acceptance bar. After implementation (not in this planning step):

1. `go test` in `worktree` — `TestHitReturnsCopy`, `TestSingleFlight`, `TestErrorNotCached`, `TestLRUEviction`, `TestLenBounded`, `TestClosedReturnsErrClosed`, `TestNewPanics`.
2. `go test -race` — same suite, then the private tests. Single-flight, copy, and close/resolve arbitration are the race-prone paths.
3. `go vet` — clean.

Private tests, each bounded by a 5s context deadline, using `testing/synctest` where overlap must be deterministic. No sleeps in the cache or in tests to win a race.

Synctest constraint that the cache must satisfy, because `TestSingleFlight` and `t.Cleanup(c.Close)` run inside a bubble. `synctest.Wait` returns only when every other bubble goroutine is durably blocked. A blocked receive on an unbuffered channel counts. A blocked receive on a `time.Timer` or `time.After` channel does not, and a `select` that includes such a channel is not a durable block. `Close` must therefore not use `time.After`, `time.NewTimer`, or any other timer to bound `wg.Wait`. A plain `wg.Wait` is a durable block, so the public test's `synctest.Wait` can observe the Loader still parked on `release` while `Get` callers are parked in `select`. `t.Cleanup` runs `Close` inside the same bubble before `synctest.Test` returns; that `Close` must be able to finish once the Loader returns, which the public test already arranged by closing `release` before `wg.Wait`.

Do not put a timer in the production `Get` wait either. The three-way `select` on `ctx.Done()`, `flight.done`, and `base.Done()` is a durable block when none of those channels is a timer. Caller deadlines live in the caller's context, which the public tests create with `context.WithTimeout`. Those timers belong to the test, not to the cache.

- Nil versus empty: Loader returns `(nil, nil)`, then a second `Get` returns a nil slice and does not reload. A different key whose Loader returns `[]byte{}` yields a non-nil empty slice from `Get`, and mutating that slice does not make a later `Get` non-empty or nil.
- Error with bytes: Loader returns `[]byte("x"), err`; `Get` returns a nil slice and `errors.Is` the error; next `Get` reloads; `Len` stays 0 across the failed load.
- Distinct waiter copies: two overlapped `Get`s, mutate one result, the other still equals the payload.
- Orphan: cancel the only waiter's ctx while the Loader is blocked; that `Get` returns `ctx.Err()`; release the Loader; a later `Get` does not call `Loader` again.
- Resolved beats caller cancel: block the Loader, cancel the caller ctx only after the Loader has been released and has published; `Get` returns the bytes. Easiest shape: cancel ctx and close the release in an order the test controls with synctest, asserting the priority table's resolved-and-not-closed row. If both are ready before arbitration, the result is the bytes.
- Close beats resolution and beats caller cancel: `Get` blocked in `select`, `Close`, expect `errors.Is(err, ErrClosed)` and nil bytes, including when the caller ctx is cancelled in the same window. `Close` returns nil. A follow-up `Get` is `ErrClosed` and does not call `Loader`.
- Close discards a late success: Loader waits on `ctx.Done()` then returns bytes; `Close` from another goroutine; after `Close` returns, `Len() == 0` and a new cache would be required to see the bytes — on this cache a later `Get` is `ErrClosed`. Also the zero-waiter form: cancel the waiter first, then `Close` before releasing a Loader that waits on `ctx.Done()`, and assert nothing is stored (`Close` returns, and the implementation discarded rather than admitted).
- In-flight does not count: capacity 1, one resident key, one blocked load of another key, `Len() == 1` while the load is blocked; release; `Len() == 1` and the LRU victim is the untouched key.
- Capacity 1 eviction and recency, including a hit that saves the sole entry's sibling from eviction.
- Concurrent distinct keys: several goroutines, more keys than capacity, `Len() <= capacity` on every poll, race detector on.
- Idempotent overlapping `Close`: start one `Close` while a Loader is blocked on `<-ctx.Done()`, start a second `Close` and wait until it is durably blocked on `closeMu` (`synctest.Wait`), then let the Loader return. The second `Close` must still be blocked at that `Wait`, and both must return nil only after the Loader has returned. A second `Close` started after the first has returned also returns nil. A later `Get` is `ErrClosed` and does not call `Loader`.
- Single-flight of two keys does not collapse: overlapping loads, two Loader calls, one each.

`testing/synctest` (Go 1.25) is how the public single-flight test forces overlap. The cache has no timers and no background refresher.

## Implementation order

1. Types, `New` panic checks, `clone`, `ErrClosed`, stubs that compile.
2. Hit path with clone-on-read and LRU promote.
3. Miss path: flight insert, Loader goroutine, publisher that admits or records an error, waiter arbitration table.
4. `Close` with `closeMu` held across `cancel` and `wg.Wait`, drop, discard-on-closed in the publisher. No timers.
5. `go test`, `go test -race`, `go vet`.
