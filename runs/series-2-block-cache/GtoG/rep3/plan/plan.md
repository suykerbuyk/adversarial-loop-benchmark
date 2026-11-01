# Implementation plan: `blockcache`

Package `blockcache` (module `blockcache`, Go 1.25). This revision addresses every item in `plan/plan_findings.md`. Do not implement in this step.

Public tests live at `instrument/resolve_public_test.go` (also copied as `worktree/blockcache_public_test.go`). They call `New`, `Get`, `Len`, `Close`, and compare with `errors.Is(err, ErrClosed)`. `TestSingleFlight` runs inside `testing/synctest`. A goroutine in that bubble is durably blocked only when it can be unblocked only by the same bubble. `synctest.Wait` returns only once every other goroutine in the bubble is durably blocked. Locking `sync.Mutex` is not a durable block. A channel created in the bubble panics if a goroutine outside the bubble sends, receives, or closes it.

## Public API

Ship exactly the surface in the spec, in package `blockcache`:

```go
type Loader func(ctx context.Context, key string) ([]byte, error)

type Cache interface {
    Get(ctx context.Context, key string) ([]byte, error)
    Len() int
    Close() error
}

func New(capacity int, load Loader) Cache
var ErrClosed = errors.New("blockcache: closed")
```

`New` panics if `capacity < 1` or `load == nil` (message is unspecified; tests only require a panic). `Close` returns `nil` always, including the second and later calls. `Get` returns a non-nil error with a nil byte slice on failure. Wrapping `ErrClosed` is legal; callers use `errors.Is`.

Suggested files (implementation only; tests stay external):

- `cache.go` — types, `New`, `Get`, `Len`, `Close`, copy helper, `decide` helper.
- `cache_test.go` — extra tests beyond the public suite (planned; see Test approach).

The public suite is `package blockcache`, so unexported helpers are visible to those tests. Do not rely on that; the public tests only use the exported API.

## Internal data model

One concrete type, unexported, returned as `Cache`:

```go
type cache struct {
    capacity int
    load     Loader

    mu       sync.Mutex
    closed   bool
    root     context.Context    // cache-scoped; never a caller's ctx
    cancel   context.CancelFunc // cancelled exactly once, on the close transition, after mu is released
    entries  map[string]*entry  // resident entries only
    inflight map[string]*flight // live until the publisher finishes
    lru      *list.List         // container/list; Front = least recently used, Back = most recently used

    loads sync.WaitGroup // one Add per in-flight Loader goroutine; Close waits on it
}

type entry struct {
    key  string
    val  []byte // cache-owned copy; never the Loader's slice, never f.val, never a slice handed to a caller
    elem *list.Element
}

type flight struct {
    done  chan struct{} // closed exactly once, by whoever publishes the terminal state
    val   []byte        // private copy of the payload when state == flightOK; otherwise nil
    err   error         // loader error when state == flightErr
    state flightState
}

type flightState int

const (
    flightPending flightState = iota // not yet published
    flightOK                          // success stored; waiters copy f.val
    flightErr                         // loader error; not stored; waiters return f.err
    flightDiscard                     // result dropped because the cache closed
)
```

`flightDiscard` is a distinct terminal state. A discarded success is never published as `flightOK` with a nil slice, and never as `(nil, nil)`. Waiters that observe `flightDiscard` return `ErrClosed`. Close publishes `flightDiscard` itself, so a loader that ignores `ctx` cannot keep waiters blocked. `Close` still joins Loader goroutines with `loads.Wait()`.

Invariants under `mu`:

- `len(entries) <= capacity` at every unlock. In-flight loads are absent from `entries` and from the LRU list, so they do not consume capacity and cannot be evicted.
- `entries` and the LRU list describe the same set. Every resident key has exactly one `*entry` and one list element.
- `inflight[key] == f` iff a Loader goroutine for `f` has been started and its publisher has not yet finished the delete. After Close's detach section, `inflight` is empty. Until a publisher finishes on the non-close path, `inflight` may be non-empty while the cache is open. `closed` means no new flights.
- `Len()` returns `len(entries)` and nothing else. After the close critical section it is 0.

Recency: `touch(e)` does `lru.MoveToBack(e.elem)`. Evict does `lru.Front()` (least recently used). Insertion order among keys that have never been re-touched is the list order, so the oldest successful admit is the LRU. The spec allows any tie break among equal recency; list order is the tie break. Hits and successful admits both move to the back (rule 6). A waiter that joins an in-flight miss does not move the list. `decide` does not touch the LRU.

No second mutex. `flight.done` is the only extra synchronization, so a waiter can block without holding `mu`. The publisher sets `state` / `val` / `err` under `mu` before `done` is closed. `runLoad` closes `done` under `mu`. `Close` sets the terminal state under `mu`, unlocks, then closes `done`. The pending-state check is the double-close guard, so `done` is closed exactly once either way.

## Concurrency protocol

### `New`

1. Panic if `capacity < 1` or `load == nil`.
2. `root, cancel := context.WithCancel(context.Background())`. Not `WithTimeout`. Not a child of any caller context. `cancel` closes `root.Done()` synchronously and does not use a timer.
3. Allocate `entries`, `inflight`, and an empty `list.List`.
4. Return `*cache`.

### `Get`

Lock order is only `mu`. Never call `Loader` while holding `mu`. Never wait on `flight.done` or on `loads` while holding `mu`.

1. Lock `mu`.
2. If `closed`, unlock and return `nil, ErrClosed`. Do not call `Loader`. This is the "has not yet passed the closed check" gate (rule 8).
3. If `entries[key]` exists: copy bytes, `MoveToBack`, unlock, return the copy. No `Loader` call (rule 1).
4. If `inflight[key]` exists: take that `*flight`, unlock, go to wait (single-flight join).
5. Otherwise this caller is the leader:
   - Allocate `f := &flight{done: make(chan struct{})}`. `done` is created on the `Get` goroutine. In `TestSingleFlight` that goroutine is inside the synctest bubble, so `done` is a bubble channel.
   - `inflight[key] = f`.
   - `loads.Add(1)`.
   - Unlock.
   - Start `go c.runLoad(key, f)`.
   - Fall through to the same wait path as a joiner.

`runLoad` (leader goroutine only):

1. `defer loads.Done()` as the first action, so `Wait` is paired with `Add` even if the rest returns early. Do not `recover`. A panicking Loader is outside the contract and can stall `Close`; the spec requires the Loader to return.
2. Call `c.load(c.root, key)`. The context is the cache-scoped one, not the caller's (rule 7).
3. On return, lock `mu`.
4. Call `publish(f, loaded, loadErr)` (see below). It no-ops if Close already published.
5. Unlock. `loads.Done()` runs via defer after unlock.

`publish` (called only with `mu` held, only from `runLoad`):

```text
publish(f, loaded []byte, loadErr error):
    if f.state != flightPending:
        return                          // Close already set flightDiscard; Close closes done
    if inflight[key] == f:
        delete(inflight, key)           // runLoad owns this delete
    if closed:                          // closed flag only; not root.Err()
        f.state = flightDiscard
        f.val, f.err = nil, nil
    else if loadErr != nil:
        f.state = flightErr
        f.err = loadErr                 // bytes discarded (rule 3)
        f.val = nil
    else:
        admit(key, loaded)              // clone into entry.val; evict LRU first
        f.state = flightOK
        f.val = clone(entry.val)        // second copy; do not alias entry.val
        f.err = nil
    close(f.done)                       // exactly once on this path; state is already terminal
```

Discard is keyed only off the `closed` flag that the close section sets. `root.Err()` is a consequence of that flag: `cancel()` runs only after `closed` is set, and only on the close transition. Do not treat `root.Err() != nil` as a second reason to publish `flightDiscard`. A future cancel of `root` that is not the close transition must not discard a successful load or surface it as `ErrClosed` while the closed gate is still false. Rule 8 discards a result only after the cache-scoped context is cancelled as part of Close.

Close is the other publisher. It sets `flightDiscard` only on the transition to `closed`, under `mu`, and only for flights still in `flightPending`. It closes those `done` channels after it unlocks. After Close has set the state, `runLoad`'s `publish` sees `state != flightPending` and must not `close(done)` again.

Who deletes `inflight[key]`:

- **Close**, on the close transition, deletes every `inflight` entry in that critical section (detach). That is the owner when the cache closes before `runLoad` publishes.
- **runLoad**, inside `publish`, deletes `inflight[key]` only when it still points at `f`. That is the owner on the normal path. If Close already detached, the map no longer points at `f`, and `runLoad` does not delete a newer flight. A newer flight cannot exist: `closed` is set in the same critical section as the detach, and `Get` will not insert after that.

`admit` (called only from `publish`, `mu` held, `closed` is false):

```text
for len(entries) >= capacity:
    front := lru.Front()
    victim := front.Value.(*entry)
    lru.Remove(front)
    delete(entries, victim.key)
e := &entry{key: key, val: clone(loaded)}
e.elem = lru.PushBack(e)               // new entry is MRU; this is the rule-6 write for a successful load
entries[key] = e
```

`capacity >= 1` is guaranteed by `New`, so the loop terminates with room for one insert. Evict only residents. The key being admitted is not resident: a flight exists only for an absent key, and the single publisher is the only inserter. Do not evict `inflight` records.

### Wait path

`TestSingleFlight` derives the waiter context as `context.WithTimeout(t.Context(), 5*time.Second)`. On Go 1.25 that parent is bubbled, and a `WithTimeout` of it is also bubbled: its timer is a bubble timer, and `synctest.Wait` returns while goroutines are parked in a `select` that includes that `ctx.Done()`. The public test still must not depend on that accident. The durable park is `f.done` alone, created in `Get` inside the bubble. The only closers of `f.done` are `runLoad` (started from `Get`) and `Close` (in the public suite, from the bubble root via `t.Cleanup`).

Do not put `ctx.Done()` in the waiter `select`. Do not call `context.AfterFunc` on the caller's ctx from `Get`. `AfterFunc` runs `go f()` from `afterFuncCtx.cancel`, which is not the `Get` goroutine. When the caller ctx is not a bubble context, that goroutine is outside the bubble. Closing a per-waiter channel from it is an outside event: the `select` is not durably blocked, and closing a bubbled channel from outside the bubble panics. `TestSingleFlight`'s ctx happens to be bubbled, so an `AfterFunc` on it would not fail that one test. The same `Get` is also called with ordinary contexts. One wait path has to be correct for both. That path receives only on `f.done`.

Caller cancel is observed without a second select case and without `AfterFunc`:

```text
wait(ctx, f):
    if ctx.Err() != nil {
        // Already Done before we park. The load is still in flight unless
        // publish won the race and closed done. Do not receive yet.
        lock mu
        defer unlock
        return decide(ctx, f)
    }

    <-f.done                               // only case; bubble channel in TestSingleFlight

    lock mu
    defer unlock
    return decide(ctx, f)
```

The pre-check is not a blocking operation, so it does not affect `synctest.Wait`. In `TestSingleFlight` every waiter ctx is still active, so every waiter takes `<-f.done` and is durably blocked. `synctest.Wait` returns. The loader is still blocked on `<-release`. `calls == 1`.

A ctx that is already Done when `wait` starts returns immediately through `decide`, while the load is still in flight (unless it published in the gap). That is rule 7(b) for the non-racy case, and rule 7(a) if Close already won.

A ctx that becomes Done after the waiter has parked stays parked on `<-f.done` until the load publishes or Close closes `done`. It then runs `decide`. At that moment the flight is terminal, so the result is `ErrClosed` if the cache closed, otherwise the load result. Rule 7 lists `ctx.Err()` only while the load is still in flight. Once `done` is closed by `publish`, the load is not still in flight, so (c) is the specified result even if the caller's ctx is also Done. Closed still outranks that result.

This is the reading that keeps a single wait path, passes `TestSingleFlight` without a non-bubble channel in the `select`, and still returns `ctx.Err()` when the caller's ctx is already Done and the flight is still `flightPending`. An implementation that adds `ctx.Done()` or `AfterFunc` back into `wait` reintroduces the blocker for any caller whose `Done` channel was not created in the bubble.

### `decide` — the only rule-7/8 decision function

Every in-flight return calls this under `mu`. No other function returns a Get result for an in-flight key. The hit path and the pre-wait closed check do not consult a flight.

`decide` does not touch the LRU. Rule 6 marks a key most-recently-used on a hit and on a successful load, not on a waiter joining an in-flight miss. The admit path `PushBack`s the new entry; that is the MRU update for that success. A later editor who treats every successful Get as a hit will add a `touch` in `decide` and look up an entry that `publish` has not inserted yet, or will touch under a second lock. Keep the recency write only in the hit path and in `admit`.

```text
decide(ctx, f) (under mu) -> ([]byte, error):
    // (a) closed outranks everything, including a resolved load
    if closed || f.state == flightDiscard:
        return nil, ErrClosed
    // (c) a resolved flight beats a caller that also cancelled
    if f.state == flightOK:
        return clone(f.val), nil          // no LRU touch
    if f.state == flightErr:
        return nil, f.err
    // still in flight (flightPending): the pre-check woke us, or done was
    // closed without a terminal publish (the protocol forbids the latter)
    // (b) ctx.Err() while the load is still in flight
    return nil, ctx.Err()
```

Order is exactly the spec, evaluated at decision time under `mu`:

1. `ErrClosed` if the cache is closed, including a success that was discarded because of close (`flightDiscard`). This covers a waiter that observes `done` and would otherwise read a nil payload with a nil error.
2. Else if the flight is resolved (`flightOK` or `flightErr`), return that result. Caller cancel does not override a resolved, non-discarded load.
3. Else the load is still in flight: return `ctx.Err()`.

`flightDiscard` is checked in step 1 even when `closed` is already true, so a publish/close race cannot return `(nil, nil)`. A discarded success is never `flightOK`.

Do not return `ctx.Err()` without holding `mu` and running `decide`. The receive only waits for a terminal publish. `decide` chooses the error.

### Orphaned loads (rule 7)

The leader goroutine is not tied to any caller's context. If every waiter returns from the pre-check with `ctx.Err()` before `f.done`, `runLoad` still finishes. On success, and only when not closed, `publish` inserts into `entries`. That Get does not delete `inflight` early. A later Get for the same key, arriving while the orphan is still in `inflight`, joins the same flight. If it arrives after publish, it hits `entries`.

Caller leaving does not cancel `c.root`. There is no `AfterFunc` and no per-waiter channel.

### `Len`

Lock `mu`, return `len(entries)`, unlock. In-flight keys are not in the map, so they are excluded. The value can lag a completion by one critical section: a publisher may have the result but not yet hold `mu` to insert, or a caller may sample between insert and the next poll. It cannot exceed `capacity` because admission checks under the same `mu`.

### `Close`

Idempotent join. Close itself wakes every current waiter. It does not wait for a well-behaved Loader before that wake. It does not hold `mu` across that wake, and it does not call `cancel` or `loads.Wait` while holding `mu`.

`context.WithCancel` closes `root.Done()` synchronously, so a Loader blocked on `<-ctx.Done()` does not need the fake clock to observe cancel. The deadlock the previous plan hits under `testing/synctest` is different: `close(f.done)` while `mu` is held wakes the waiter, the waiter's next step is `mu.Lock`, and locking a mutex is not a durable block. If Close then calls `loads.Wait` before the waiter can finish `decide`, a bubble `Wait` (or the bubble clock, if any goroutine is waiting on it) cannot proceed while that waiter sits on `mu`. Unlocking before `Wait` is required. Unlocking before `cancel` is also required, so a loader that wakes on `root.Done()` and immediately locks `mu` to publish is not stuck behind Close.

```text
lock mu
if closed:
    unlock
    loads.Wait()          // second caller joins the same in-flight loaders; returns nil
    return nil
closed = true
// Snapshot, detach, and mark discard. Do not close(done) yet.
// Do not cancel yet. Waiters stay durably blocked on f.done.
toWake := pending flights
for key, f := range inflight:
    delete(inflight, key)
    if f.state == flightPending:
        f.state = flightDiscard
        f.val, f.err = nil, nil
        append f to toWake
    // state != pending: runLoad already closed done; do not close again
entries = empty map
lru.Init()
unlock                        // mu is free before any wake and before cancel

cancel()                      // cache-scoped context; Loaders are obliged to return
                              // a loader that returns now locks mu, sees state != pending, returns

for _, f := range toWake:
    close(f.done)             // wake waiters; they lock mu (not held) and decide ErrClosed

loads.Wait()                  // join Loader goroutines; they may still be inside load()
return nil
```

Why this order:

- While `mu` is held, waiters are still blocked on `f.done`. That receive is a durable block when `done` was created in the bubble. Close is not depending on the fake clock, and is not in `loads.Wait`, during this section.
- `cancel()` runs after unlock. A Loader blocked on `<-c.root.Done()` wakes, takes `mu`, and `publish` no-ops because `state != flightPending`. It does not close `done` (Close will). It does not insert.
- `close(f.done)` runs after unlock, so the woken waiter locks a free mutex, runs `decide`, sees `closed` or `flightDiscard`, and returns `ErrClosed`. The waiter is never blocked on `mu` held by Close.
- `loads.Wait()` runs last, after `cancel` and after the wake. It does not run while `mu` is held. A Loader that has already returned is joined immediately. A Loader still inside `load` is joined when it returns. Close does not return until every in-flight Loader call has returned.
- A Loader that ignores `ctx` stalls `Close` inside `Wait` (the join the spec requires) but does not stall waiters: their `done` channels are already closed.

Race between `runLoad` and `Close` on a flight that has already been published as `flightOK`:

- `publish` inserts, sets `flightOK`, closes `done`, unlocks.
- Close then sets `closed`, drops `entries` (the just-inserted entry is dropped with the map replacement), and sees `f.state != flightPending`, so it does not close `done` again and does not put `f` in `toWake`.
- A waiter that has not yet called `decide` sees `closed` and returns `ErrClosed` (priority (a) over the load result). The stored bytes are gone. This is the close window: a result that resolved in the same moment as Close is not observable as a hit after Close, and is not left resident.

If Close sets `flightDiscard` before `runLoad` enters `publish`, `runLoad` no-ops and does not close `done`. Close closes `done` after unlock. Waiters get `ErrClosed`. Bytes are not stored.

If `runLoad` has passed the `state == flightPending` check, it holds `mu` until `publish` finishes. Close cannot interleave mid-publish. Either `runLoad` publishes a non-discarded result and closes `done`, and Close then sees `state != flightPending`, or Close acquires `mu` first.

`Close` must not call `Loader` and must not hold `mu` while waiting on `loads` or while blocked on a Loader.

Second `Close` while the first is inside `loads.Wait()`: sees `closed`, unlocks, calls `loads.Wait()` on the same `WaitGroup`, returns nil. Concurrent `Wait` is safe. The only `Add` sites are in `Get` before starting a load, and those sit after the closed check under `mu`, so they are unreachable once `closed` is set. No `Add` after `Wait` has started observing zero.

`cancel` is called only on the transition to closed, so a second `Close` does not double-cancel. (`context.CancelFunc` is safe to call twice, but the plan calls it once.)

Wake vs already-finished waiters: a Get that already returned `ctx.Err()` from the pre-check has finished. It is not a current waiter. Spec priority applies at the moment a waiting Get chooses a result, which is `decide` under `mu`.

## How each rule is satisfied

1. **Hit.** `Get` finds `entries[key]` under `mu`, copies `entry.val`, `MoveToBack`, returns. `Loader` is not invoked. Public test: `TestHitReturnsCopy` (second Get, `calls == 1`).

2. **Single-flight miss.** The first `Get` that finds neither an entry nor an `inflight` record inserts one `*flight` and starts one goroutine. Later Gets for that key, until publish or Close deletes `inflight`, share that flight. All waiters copy from `f.val` after `decide` sees `flightOK`. Success inserts one resident entry. Waiters block on `<-f.done` only, a channel created in `Get` inside the bubble, so `synctest.Wait` in `TestSingleFlight` returns while the Loader is still blocked on `<-release`. Public test: 50 goroutines, `calls == 1` both before and after release, identical `block:k` bytes.

3. **Error.** Non-nil loader error skips admission, sets `flightErr` and `f.err`, closes `done`. Every current waiter returns that error and a nil slice. `inflight` is cleared, so the next `Get` is a new miss and calls `Loader` again. `(non-nil bytes, non-nil err)` is `flightErr`: bytes are not copied into `entries` or `f.val`. Public test: `TestErrorNotCached` (`errors.Is` with the sentinel `boom`, nil slice, second Get returns `"ok"`, `calls == 2`).

4. **Ownership / copy.** On successful load, `clone` into `entry.val` before publish returns; the cache does not keep the Loader's slice. `f.val` is a second `clone` of `entry.val`, so the flight result and the resident entry do not share a backing array. `Get` always clones again for the caller. Mutating a returned slice cannot change `entry.val`, `f.val`, or another waiter's slice. Public test: `TestHitReturnsCopy` mutates the first result to `'X'` and requires the second Get to still be `"payload"`.

   Nil successful payload: `clone(nil)` returns `[]byte{}` (non-nil, length 0), and that empty slice is what is stored and what each waiter receives a fresh copy of. A nil Loader success is therefore a cached empty block, distinguishable from the error path (which returns a nil slice and a non-nil error). Callers can mutate the empty slice they receive without aliasing the resident buffer, because each `clone` of an empty non-nil slice allocates. The public suite never loads a nil or empty block; the extra tests do.

5. **Capacity / LRU.** Admission runs only in `publish` under `mu`, only on success, only if not closed. While `len(entries) >= capacity`, remove `lru.Front()` from the list and delete that key from `entries`, then insert the new entry at the back. In-flight flights are not in the list, so they are not evicted and do not count. `Len` is `len(entries)` under `mu`, hence never above capacity. Public tests: `TestLRUEviction` (capacity 2: load a, b, hit a, load c evicts b; a stays at 1 load, b reloads, c loads once) and `TestLenBounded` (capacity 3, five inserts, `Len() <= 3` after each Get, final `Len() == 3`).

6. **Recency.** Hit path `MoveToBack`. Successful admit `PushBack` (already MRU). A reload after eviction is a new admit, so it becomes MRU. Failed loads and discarded loads do not touch the list. `decide` does not move the element for a joined success. The recency write lives only in the hit path and in `admit`.

7. **Caller context and orphaned loads.** `load` is invoked as `load(c.root, key)` only. Waiter cancellation never cancels `c.root`. The flight stays in `inflight` until `publish` or Close finishes, so a load with zero remaining waiters still completes and, on success, populates `entries`. Waiting `Get` result priority is only `decide`, under `mu`: (a) `closed` or `flightDiscard` → `ErrClosed`; (b) `flightPending` and own ctx done → `ctx.Err()`; (c) `flightOK` / `flightErr` → the load result, each success as its own copy. Resolved-and-not-closed beats caller cancel; closed beats a resolved result. A ctx that is already Done before the waiter parks hits (b) without waiting for the Loader, via the pre-check. A ctx that becomes Done after the park is applied at `decide` time, after the flight is terminal, so the result is (a) or (c).

8. **Close.** On the transition: set `closed`, detach every in-flight flight, publish `flightDiscard` under `mu`, drop `entries` and reset the LRU, unlock, then `cancel()` the cache context, then `close(done)` so every current waiter wakes with `ErrClosed`, then `loads.Wait()` until every `runLoad` has returned, return nil. Idempotent: a second `Close` waits on the same `WaitGroup` and returns nil. A Loader that returns after cancel does not insert (`publish` no-ops). A `Get` that acquires `mu` after `closed` is set returns `ErrClosed` and does not `Add` or start a loader. Public test: `TestClosedReturnsErrClosed` (first Close nil, Get is `ErrClosed`, second Close nil).

9. **Concurrency.** All map, list, flag, and flight-state access is under `mu`. `flight.done` is closed exactly once: by `runLoad`'s `publish` if it still owns a pending flight, or by `Close` after unlock if it set `flightDiscard` first. `WaitGroup` `Add` happens before the goroutine starts and before unlock, and never after `closed`. No lock is held across `Loader`, `Wait`, `cancel`, or channel receive. `Close` does not hold `mu` across `close(done)`. This is the race-detector contract (`go test -race`).

## Copies

```text
clone(b []byte) []byte:
    if b == nil:
        return []byte{}                 // non-nil empty; one distinct backing array per call
    return append([]byte(nil), b...)
```

Use `clone` for `entry.val`, again for `f.val`, and again for every `Get` return (hit or `flightOK`). On `flightErr` and on `ErrClosed` / `ctx.Err()` return a nil slice, not `clone`.

Sharing `f.val = e.val` is forbidden. A later edit that reslices `f.val` or forgets a clone on one return path would otherwise alias a caller mutation into the resident buffer. Two copies cost one extra allocation per successful load and keep the ownership rule local.

## Deadlock avoidance

- `mu` is not held in `Loader`, in the waiter receive, in `cancel`, or in `loads.Wait`.
- `Get` never waits for `Close`, and `Close` never calls `Get`.
- `flight.done` is closed exactly once, after `state` is terminal. The pending-state check is the double-close guard. `runLoad` closes under `mu`. `Close` closes after unlock, only for flights it itself moved to `flightDiscard`.
- `loads.Add(1)` is under `mu` before the goroutine is started, and `defer loads.Done()` is the first action in that goroutine, so `Wait` cannot return early and cannot miss the goroutine.
- `closed` is set before `Wait`, and `Get` will not `Add` once `closed` is set, so `WaitGroup` use is the legal "Add before Wait, no Add after Wait starts" pattern.
- Close does not depend on the Loader observing `ctx.Done()` in order to wake waiters. A Loader that blocks on `<-c.root.Done()` unblocks when `cancel()` runs, then takes `mu` only after Close has unlocked. A Loader that ignores cancel stalls `Close` inside `Wait` (the join the spec requires) but does not stall waiters.
- Under `testing/synctest`, a waiter woken by `close(done)` must not be left blocked on `mu` while Close is in `loads.Wait` or while Close still needs the bubble to make progress. The close sequence unlocks before `cancel`, before `close(done)`, and before `Wait`.

## Edge cases the public suite does not cover (still required)

- Several distinct cold keys in flight at once: each has its own `*flight` and its own `loads` count; single-flight is per key, not global.
- Capacity full of residents plus additional in-flight keys: `Len()` stays at capacity; admission of each success evicts LRU first, one eviction per admit.
- `Get` called with an already-cancelled ctx while a load is in flight: returns `ctx.Err()` from the pre-check without waiting for the Loader; the load continues; a later Get with a live ctx joins the orphan or hits after publish (one Loader call total).
- All waiters already cancelled before they park, load succeeds, cache not closed: key becomes resident.
- All waiters already cancelled, load fails: key not resident; later Get calls Loader again.
- A waiter that parks and is then cancelled, load then succeeds, cache not closed: that waiter receives the success bytes (decision is after resolve, so rule 7(c)); the key is resident.
- Loader returns `(bytes, err)` both non-nil: `flightErr`, nothing stored, later Get reloads.
- Successful nil or empty payload is cached as a non-nil empty slice; a later hit does not call Loader; each Get returns its own empty slice.
- `Get` after `Close` does not call Loader (a counting loader stays at 0).
- `Close` overlapping an in-flight load whose Loader blocks on `ctx.Done()`: Close unlocks, cancels, closes `done`, then waits; waiters get `ErrClosed`; the result is not stored; `Close` returns after the Loader returns. No goroutine sits on `mu` held by Close.
- Loader ignores `ctx` and returns a payload after `Close` has already published `flightDiscard`: `publish` no-ops, bytes are not stored, waiters already have `ErrClosed` once `done` is closed (which is before `loads.Wait`).
- Double `Close` concurrent with `Get`: no panic, no double `close(done)`, no double `WaitGroup` `Done`, no second loader.
- `Len` concurrent with admit and with `Close`: never `> capacity`, never negative; after Close's critical section, 0.
- Hit during an unrelated in-flight key: hit still copies and marks MRU; does not affect the flight.
- Same key requested again after a failed flight has cleared `inflight`: new flight, new Loader call.
- Leader is the only waiter and its ctx is already cancelled before the loader returns: that Get returns `ctx.Err()` from the pre-check; loader still runs on `c.root`; success still caches.
- `Close` when idle: `closed = true`, empty inflight loop, unlock, `cancel()`, `Wait` returns immediately, nil.
- Idempotent `Close` after idle close: nil, `cancel` not called again.
- Caller ctx already done at the start of `wait`, and Close has already set `flightDiscard`: `decide` returns `ErrClosed`, not `ctx.Err()`.
- `synctest` waiter: the only receive is `f.done`, created inside the bubble. `ctx.Done()` is not a select case. No `AfterFunc` runs on the caller ctx.

## Test approach

Primary bar: the shipped suite must pass, including under the race detector.

```text
go test -count=1 .
go test -race -count=1 .
go vet .
```

Public cases and the rule each locks:

| Test | Rules |
| --- | --- |
| `TestHitReturnsCopy` | 1, 4 |
| `TestSingleFlight` | 2, 4, 9 (overlap via `testing/synctest`; wait must be durable) |
| `TestErrorNotCached` | 3 |
| `TestLRUEviction` | 5, 6 |
| `TestLenBounded` | 5 |
| `TestClosedReturnsErrClosed` | 8 |
| `TestNewPanics` | `New` preconditions |

`TestSingleFlight` uses `t.Errorf` (not `Fatalf`) while the 50 goroutines are parked, then `t.Fatalf` after `wg.Wait()`. The implementation must block inside `Loader` on `<-release` without taking `mu`, and waiters must be durably blocked on `f.done` only, or `synctest.Wait` will not return if a future caller ctx is not bubbled. The loader context must not be cancelled by waiter scheduling; waiters are still blocked in `Get`, not cancelled, in that test. Do not put `ctx.Done()` in the waiter `select`. Do not register `AfterFunc` on the caller ctx.

Extra tests in `cache_test.go` (they lock the rules the public suite leaves open). Extra tests that run inside `synctest` and cancel a waiter use `context.WithCancel(t.Context())`, whose `Done` channel is created in the bubble. They do not use `WithTimeout(t.Context(), …)` as the ctx under test. The cache still must not `select` on that channel.

- `Get` with an already-cancelled ctx during an in-flight load: that Get gets `ctx.Err()` without the Loader returning; a second waiter still blocked gets the success bytes; `Loader` entered once; a following Get hits.
- Cancel every waiter before it parks (already-cancelled ctx); loader then succeeds; following Get hits (`calls == 1`).
- `Close` while a loader is blocked on `<-ctx.Done()`, inside `synctest`: `Close` returns, in-flight waiters get `ErrClosed`, a following Get does not observe the loaded bytes, and the test does not deadlock. This locks the unlock-before-cancel-before-Wait order.
- `Close` while a loader ignores `ctx` and blocks on a test channel: waiters receive `ErrClosed` without waiting for the loader; then release the loader; `Close` returns; a following Get is `ErrClosed` and the payload was not stored.
- Concurrent `Close` twice plus `Get`: both Closes return nil; Gets return `ErrClosed`; no race; no double-close panic.
- Two keys, capacity 1: second success evicts the first; an in-flight third key does not change `Len` until it succeeds.
- `([]byte("x"), err)` from Loader: waiter gets the error, nil bytes, key absent, next Get loads again.
- Loader returns `(nil, nil)`: success, cached, second Get does not call Loader, returned slice is non-nil empty, mutating it does not affect the next Get.
- Mutate the slice the Loader returned after return: a later hit still returns the pre-mutation bytes.
- 8 goroutines, 8 distinct keys, capacity 2: `Len() <= 2` always, no race, no deadlock.
- Caller ctx already cancelled and `Close` overlapping `decide`: `errors.Is(err, ErrClosed)`.

Run extra tests with `-race` as well. No `TestMain`, no `os.Exit`. Each test `Close`s via `t.Cleanup`.

## Out of scope

No metrics, no TTL, no `Delete`/`Set`, no per-key caller-context propagation into `Loader`, no byte pooling, no dependency beyond the standard library. `container/list` is the LRU. Do not start a load after close. Do not cache errors. Do not share slices with the Loader, between `entry.val` and `f.val`, or with callers. Do not add a second select case to the waiter. Do not key discard off `root.Err()`. Do not touch the LRU from `decide`.
