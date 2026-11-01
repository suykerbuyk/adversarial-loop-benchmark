# Implementation plan: `blockcache`

Package `blockcache` (module already at `worktree/go.mod`, Go 1.25). One implementation file plus the shipped public suite. No new dependencies.

This revision answers the findings in `plan/plan_findings.md`. The panic path owns `mu` once. The panic error is not part of the exported contract and is not cached. Eviction saves the victim before unlink. `Close` does not call `cancel` while holding `mu`.

## Public API

Exactly the signatures in the spec. Nothing else is exported.

```go
package blockcache

type Loader func(ctx context.Context, key string) ([]byte, error)

type Cache interface {
    Get(ctx context.Context, key string) ([]byte, error)
    Len() int
    Close() error
}

func New(capacity int, load Loader) Cache

var ErrClosed = errors.New("blockcache: closed")
```

`New` panics if `capacity < 1` or `load == nil` (public `TestNewPanics`). The concrete type is unexported (`*cache`). Callers only see `Cache`.

`Get` returns `(nil, err)` whenever `err != nil`, including `ErrClosed` and a caller-context error. Wrapping `ErrClosed` is allowed; the contract is `errors.Is`. This package returns the sentinel itself, not a wrap. `Close` always returns `nil`.

No other error value is part of the exported contract. A Loader panic is not an exported sentinel and is not documented as something callers may `errors.Is`.

## Internal data model

```go
type entry struct {
    key  string
    val  []byte // owned by the cache; never handed out
    prev *entry // neighbor toward the LRU end; nil if this node is the tail
    next *entry // neighbor toward the MRU end; nil if this node is the head
}

// flight is one in-flight Loader call for a key. It is not an entry and
// does not count toward capacity until run admits it.
type flight struct {
    key  string
    done chan struct{} // closed exactly once, by publish
    val  []byte        // owned copy, or nil; immutable after publish
    err  error         // immutable after publish
}

type cache struct {
    capacity int
    load     Loader

    mu       sync.Mutex
    resident map[string]*entry // len(resident) <= capacity, always
    head     *entry            // MRU end; nil if empty
    tail     *entry            // LRU end; nil if empty
    inflight map[string]*flight

    root   context.Context    // cancelled by Close; parent of every Loader ctx
    cancel context.CancelFunc // called once, by the Close that sets closed, after mu is released

    closed  bool               // set under mu before started is closed; never cleared
    started chan struct{}      // closed once, under mu, in the same section that sets closed
    joined  chan struct{}      // closed once, by the join owner, after loads.Wait and the resident drop

    loads sync.WaitGroup       // Add(1) under mu before the Loader goroutine starts; Done in run's defer
}
```

`flight` has no `cancel` flag. Close does not stamp per-flight state. Admission is refused by a generation check against `closed` and `root.Err()` while `mu` is held (see rule 8). Waiters return `ErrClosed` only when `closed` is true.

Two channels, two events:

- `started` is closed at the moment Close begins (same critical section as `closed = true`). The wait loop selects on it so a blocked waiter wakes without waiting for any Loader to return.
- `joined` is closed only after `loads.Wait` and the resident drop. A second `Close` waits on `joined`. It is not the waiter wake signal.

Recency is an intrusive doubly-linked list under `mu`. `container/list` is not used. `head` is the most-recently-used resident. `tail` is the least-recently-used resident. `next` points toward MRU. `prev` points toward LRU.

List invariants, maintained only while `mu` is held:

- Empty: `head == nil && tail == nil` and `len(resident) == 0`.
- One node `e`: `head == e && tail == e && e.prev == nil && e.next == nil`.
- Two or more: `head.next == nil`, `tail.prev == nil`, and following `prev` from `head` reaches `tail`. Every node's `next.prev` and `prev.next` point back at it. `len(resident)` equals the node count.
- A key is in `resident` if and only if its node is linked.

Operations:

- `linkMRU(e)` on an empty list sets `head = tail = e`. On a non-empty list sets `e.prev = head`, `e.next = nil`, `head.next = e`, `head = e`.
- `unlink(e)`: if `e.prev != nil` then `e.prev.next = e.next`, else `tail = e.next`. If `e.next != nil` then `e.next.prev = e.prev`, else `head = e.prev`. Then `e.prev, e.next = nil, nil`. Unlinking the sole node yields the empty invariant. Unlinking the tail leaves the new tail with `prev == nil`. Unlinking the head leaves the new head with `next == nil`.
- `moveToMRU(e)` is `unlink(e)` then `linkMRU(e)`. A sole node stays head and tail.
- `evictLRU()` requires a non-empty list. It saves `victim := tail` first, then `unlink(victim)`, then `delete(resident, victim.key)`. The delete uses the saved pointer's key. It does not read `tail` after unlink. Capacity 1 is the sole-node case: after unlink, `head` and `tail` are nil and `resident` no longer contains `victim.key`.

Ties cannot occur for distinct keys: every admit and every hit moves that key to `head`, so `tail` is the unique least-recent resident. The spec allows any least-recent on a tie; this structure has no ties.

`resident` holds only successfully stored entries. `inflight` holds loads that have not yet published. A key is in at most one of the two maps. `Len` returns `len(resident)` and nothing else.

Ownership:

- `copyBytes(src)` is `append([]byte(nil), src...)`.
- A nil `src` yields a nil slice. A non-nil empty `src` yields a non-nil empty slice. The two stay distinguishable. Callers that can tell nil from empty keep that distinction.
- On a successful admit the cache stores `copyBytes(loaderSlice)` and does not retain the Loader's slice.
- Every `Get` return is a fresh `copyBytes` of the stored slice. A stored nil is returned as nil. A stored non-nil empty slice is returned as a fresh non-nil empty slice.
- Waiters copy from `flight.val`, which is the same backing array as `entry.val` after admit. They never receive that array itself. After `f.done` is closed, `f.val` and `f.err` are immutable, so a later eviction of the entry does not change what waiters copy.

## Concurrency shape

One mutex, `mu`, guards `resident`, `head`, `tail`, `inflight`, `closed`, and the published fields of a `flight`. The Loader runs outside `mu`. `started` and `joined` are each closed at most once. The closer holds `mu` immediately before the close, so a waiter that observes the channel and then locks cannot miss the flag that was set in that same section.

`cancel` is not called while `mu` is held. A `Done` callback on the Loader context, or on a parent of `root`, may call `Get`, `Len`, or `Close`. Those methods take `mu`. Holding `mu` across `cancel` would deadlock that callback. `Close` sets `closed` and closes `started` under `mu`, unlocks, then calls `cancel`.

Each miss that starts a load does:

1. Under `mu`, after the closed check, insert a `flight` with `done: make(chan struct{})` and `loads.Add(1)`.
2. Unlock.
3. Start `go c.run(f)`. `run` derives the Loader context and calls `load`.
4. The starter and every joiner use the same wait loop.

`run` never holds `mu` across the Loader call. `Get` never holds `mu` across `select`. `Close` never holds `mu` across `cancel`, `loads.Wait`, or a receive on `joined`. That is the deadlock invariant.

`Len` takes `mu`, reads `len(resident)`, and returns. Admission and eviction happen in the same critical section, so a concurrent `Len` never observes a value above capacity. It may miss a completion that has not yet taken `mu`, which is the allowed one-critical-section lag.

## How each rule is satisfied

### 1. Hit

`Get` locks, sees `closed` (rule 8), then looks up `resident`. On hit it `moveToMRU`s the entry, copies `entry.val`, unlocks, and returns the copy. The Loader is not called. A hit does not consult the caller context. Rule 7's caller-context clause applies to a waiting Get while a load is in flight.

### 2. Single-flight miss

Under `mu`, if the key is absent from both maps, `Get` inserts one `flight` and is the starter. Any other `Get` that arrives before `publish` removes that flight finds the existing `flight` and becomes a waiter. Only the starter launches `run`, so concurrent Gets of one cold key produce exactly one Loader call (public `TestSingleFlight`: 50 waiters, `synctest.Wait` while the Loader is blocked on an unclosed channel).

On success, `run` stores one owned copy, publishes it on the flight, and closes `f.done`. Every waiter then copies `f.val` itself, so each receives equal bytes and its own slice (rule 4).

A waiter that arrives after `publish` has removed the flight and inserted the entry takes the hit path. Removal and insert are the same critical section on the success path, so a Get cannot observe "flight gone, entry not yet inserted."

### 3. Error

If the Loader returns a non-nil error, `run` does not copy and does not insert. It publishes `(nil, err)`, deletes the flight, and closes `f.done`. Bytes returned alongside the error are discarded. Every current waiter that is not observing `closed` receives that error and a nil slice. The key is absent, so a later `Get` starts a new flight and calls the Loader again (public `TestErrorNotCached`).

A Loader panic is the same shape as a non-cached error: current waiters of that flight receive a plain non-nil error, the key is not stored, and a later `Get` starts a new flight. The panic error is not `ErrClosed` and is not an exported value. See "run and publish."

### 4. Ownership / copy

The cache never retains the Loader slice and never returns `entry.val` or `flight.val` directly. Mutating a `Get` result cannot change a later `Get` (public `TestHitReturnsCopy`). Two waiters of the same flight do not share a backing array. Nil stays nil. Non-nil empty stays non-nil empty, as an independent slice.

### 5. Capacity / LRU

`len(resident)` is checked only when a successful load is admitted, under `mu`. If it equals `capacity`, `evictLRU` runs before `linkMRU` of the new entry. The new length is `capacity` or less. In-flight flights are not in `resident`, so they do not consume a slot and cannot be chosen as the eviction victim. They become resident, and evictable, only inside that admit section.

`evictLRU` saves the victim pointer before unlink. After unlink of the sole node, `tail` is nil. The delete uses `victim.key`, so capacity 1 does not delete `resident[""]` and does not dereference a nil tail.

Public `TestLenBounded` loads five keys into capacity 3 and asserts `Len() <= 3` after each `Get` and `Len() == 3` at the end. `TestLRUEviction` is the recency proof (rule 6): capacity 2, load `a`, load `b` (`a` is tail), hit `a` (`b` becomes tail), load `c` evicts `b`, hit `a` (no reload), `Get(b)` reloads.

A key is never in both maps, and only `run` admits, so a flight for K cannot exist while K is resident, and K cannot be the eviction victim of its own admit.

### 6. Recency

A hit moves the entry to `head` before copying out. A successful admit always inserts at `head`, after eviction if needed. Both happen under `mu`. Two concurrent hits serialize on `mu`; the later critical section wins MRU.

### 7. Caller context and orphaned loads

`run` calls the Loader with a child of `root`:

```go
ctx, cancel := context.WithCancel(c.root)
defer cancel()
b, err := c.load(ctx, f.key)
```

The child is cancelled when `run` returns (success, error, or the Close path), so a finished flight does not retain a child for the life of the process. Canceling or timing out a caller does not cancel this child. Only `Close`'s `c.cancel()`, or this defer, does.

If every waiter leaves, `run` still finishes. On success, if the admit check passes, it stores the entry. An in-flight load occupies one `inflight` slot and one `WaitGroup` count until `run` returns, then it is either resident or gone. It is not a leak.

A waiting `Get` resolves in this priority. The priority is a recheck under `mu` after every wake, because `select` case order is not priority:

1. If `closed` is set, return `(nil, ErrClosed)`.
2. Else if `f.done` is closed, return a fresh copy of `f.val` with `f.err` (nil bytes if `f.err != nil`).
3. Else if the caller's `ctx.Err() != nil`, return `(nil, ctx.Err())`.
4. Else unlock and `select` on `f.done`, `ctx.Done()`, and `started`, then loop.

Close beats a done caller context because the `closed` check is first. A resolved load beats a done caller context because the done-flight check is second. This matches rule 7: (a) `ErrClosed` if the cache closes; else (b) `ctx.Err()` if the caller's ctx is Done while the load is still in flight; else (c) the load result.

`f.done` closed and `closed` set can both be true. The closed check wins, so waiters of a load that finished during Close still get `ErrClosed`. They do not observe a discarded success as a hit.

A caller that leaves does not cancel `run` and does not call `loads.Done`.

A `Get` whose caller context is already cancelled, on a cold key, still starts the flight if it wins the miss, then takes branch 3 and returns `ctx.Err()`. The flight keeps running and a later success populates the cache. A `Get` of a resident key with an already-cancelled caller context takes the hit path and returns the copy.

### 8. Close

Waiters must be woken when Close starts, not when the Loader returns. A Loader that ignores cancel, or is slow, must not pin current waiters to `loads.Wait`. `Close` itself still waits for every in-flight Loader to return before it returns, as the spec requires.

`cancel` runs after `mu` is released. A Done callback that re-enters `Get`, `Len`, or `Close` can take `mu`. It observes `closed` already set, because that assignment happens before the unlock that precedes `cancel`.

```
Close():
    lock
    if closed:
        unlock
        <-joined          // same join the owner is running, or already finished
        return nil
    closed = true
    close(started)        // wakes every waiter select, before any Loader returns
    unlock
    cancel()              // root Done; not under mu, so a Done callback may re-enter

    loads.Wait()          // outside mu; run always Done, including on panic

    lock
    resident = empty map
    head, tail = nil, nil
    // inflight is empty: every run has published and deleted its flight
    close(joined)
    unlock
    return nil
```

Idempotent overlapping Close: the owner is the call that transitions `closed` from false to true. Every other call waits on `joined` and returns nil. A Close after `joined` is closed returns as soon as the receive completes. A re-entrant `Close` from a `cancel` callback is the non-owner path: `closed` is already true, so it waits on `joined`. The owner has not yet reached `loads.Wait` or `close(joined)`, so that callback must not be the only goroutine that can finish the join. Loader Done callbacks must not call `Close` and then block the Loader from returning. The implementation does not call `Close` from its own Loader context. Document that a user Done func may call `Get` or `Len`, and may call `Close` only if it does not need the Loader to return in order for that `Close` to finish. The safe re-entry that the unlock-before-cancel rule exists for is `Get` and `Len`.

Public `TestClosedReturnsErrClosed` covers the sequential case (first Close nil, Get is `errors.Is` ErrClosed, second Close nil).

`Get` checks `closed` under `mu` before any map lookup and before `loads.Add` or inserting a flight. A Get that has not passed that check when Close sets the flag returns `ErrClosed` and does not call the Loader. A Get that runs from a Done callback after `cancel` sees the same flag.

Waiter wake path, independent of Loader progress:

- `started` is closed in the same critical section that sets `closed`, before `cancel`.
- A waiter blocked in `select` wakes, relocks, sees `closed`, and returns `ErrClosed`.
- A waiter that is between unlock and select, or that has not yet entered the loop, takes the same closed check on its next lock.
- No waiter selects on `joined`. No waiter waits for `loads.Wait`.

Discard-on-close, including a Loader that returns before it observes cancel:

`closed` is set before `cancel()` and before any waiter can observe `started`. A Loader may already have returned and be waiting to reacquire `mu`. Admission does not trust "I did not see cancel before dropping the lock." The only admit path is:

```
lock
if c.closed || c.root.Err() != nil || err != nil:
    do not insert
    publish (nil, err)     // err may be nil on a discarded success
    delete inflight
    close(f.done)
    unlock
    return
evict if full
admit copyBytes(b) at head
publish (stored, nil)
delete inflight
close(f.done)
unlock
```

`closed` is set before `cancel()` and before `close(started)`, and it stays set. Any `run` that acquires `mu` after Close has begun sees `closed` and does not insert. Any `run` that acquired `mu` before Close began either:

- finished its critical section before Close's section, so the entry is in `resident` and Close's post-join drop removes it, or
- is still inside that section, so Close blocks on `mu` until it finishes, then proceeds to wake, unlock, cancel, and join.

There is no window in which `run` passes the admit check, drops `mu`, loses to `loads.Wait` plus the resident drop, and inserts afterward. The admit check, the insert, the flight delete, and `close(f.done)` are one critical section. `loads.Done` runs after that section (deferred, after publish returns). `Close` calls `loads.Wait` before it drops `resident`. Therefore when `Close` drops `resident`, every `run` has already either skipped insert or inserted, and no `run` will insert later. "Drops all entries" holds after `Close` returns, including the zero-waiter case and the close window.

A Loader that returns after `root` is cancelled takes the discard branch because `closed` is already true (cancel happens only after `closed` is set) or, if `closed` were observed false, because `c.root.Err() != nil`. Both checks sit in the admit section. The result is not stored. `root.Err()` is the belt; `closed` is the condition Close actually establishes first.

A discarded success publishes `(nil, nil)` and does not admit. Waiters do not turn that into a hit: they observe `closed` first and return `ErrClosed`. If the cache is not closed, `root.Err()` is nil (only Close cancels `root`), so a success is admitted. This package never cancels one flight's parent except by closing the cache. The per-flight child cancel in `run` runs after publish, so it cannot discard that flight's own result.

`Close` does not hold `mu` across `cancel` or `loads.Wait`. `run` does not need `Close` to release `mu` in order to finish. They cannot deadlock each other. A Loader that ignores `ctx.Done()` stalls `Close` on `loads.Wait`, which is the spec's join. It does not stall waiters; those already returned `ErrClosed` via `started`.

### 9. Concurrency

Every read and write of `resident`, `head`, `tail`, `inflight`, `closed`, and flight result fields happens under `mu`. `loads.Add` happens under `mu` before the goroutine starts. `loads.Done` happens in a defer in `run`, after publish. `started` is closed once, by the Close owner, in the section that sets `closed`. `joined` is closed once, by that same owner, after `loads.Wait` and the drop. `cancel` is called once, by that owner, after the unlock that follows `close(started)`. No field is published unsynchronized. This is what `-race` must accept.

Deadlock avoidance: `mu` is never held across Loader calls, `select` waits, `cancel`, or `loads.Wait`. `Get` after `Close` returns on the closed check. `Close` during `Get` either wins the flag before the flight starts, or the flight starts, waiters wake on `started`, and `run` observes `closed` when it next locks and does not admit.

## Get control flow

```
Get(ctx, key):
    lock
    if closed:
        unlock
        return nil, ErrClosed
    if e := resident[key]:
        moveToMRU(e)
        b = copyBytes(e.val)
        unlock
        return b, nil
    if f := inflight[key]:
        unlock
        goto wait
    f = &flight{key: key, done: make(chan struct{})}
    inflight[key] = f
    loads.Add(1)
    unlock
    go run(f)

wait:
    for {
        lock
        if closed:
            unlock
            return nil, ErrClosed
        select {
        case <-f.done:
            // closed is false in this section. Return the published outcome.
            err := f.err
            var b []byte
            if err == nil {
                b = copyBytes(f.val)
            }
            unlock
            return b, err
        default:
        }
        if ctx.Err() != nil:
            unlock
            return nil, ctx.Err()
        unlock
        select {
        case <-f.done:
        case <-ctx.Done():
        case <-started:
        }
    }
```

The non-blocking check of `f.done` under `mu` is the priority step. After `publish`, `f.done` is closed and `f.val` / `f.err` are immutable. A waiter that loses the race with eviction still copies `f.val`, which `run` retains on the flight for that purpose.

`ErrClosed` is returned only from a section that observed `closed`. A resolved flight on an open cache returns the load outcome. A Loader error on an open cache is returned as that error. It is not rewritten to `ErrClosed`. A Loader panic on an open cache is returned as the unpublished join-safety error described below. It is not rewritten to `ErrClosed` and it is not stored.

## run and publish

One publish path. `f.done` is closed in exactly one place. The body does not also close it. A defer does not also close it.

One owner of `mu` across the admit section and the panic publish. The body does not `defer c.mu.Unlock()`. The panic defer never locks or unlocks a mutex the body still holds. `publish` assumes the caller holds `mu` and does not lock or unlock.

```
publish(f, val []byte, err error):   // caller holds mu; f is still in inflight; does not touch mu
    f.val = val
    f.err = err
    delete(c.inflight, f.key)
    close(f.done)

run(f):
    defer c.loads.Done()

    ctx, cancel := context.WithCancel(c.root)
    defer cancel()

    var (
        b   []byte
        err error
    )
    func() {
        defer func() {
            if rec := recover(); rec != nil {
                b = nil
                err = errLoaderPanic
                panicVal = rec
            }
        }()
        b, err = c.load(ctx, f.key)
    }()

    c.mu.Lock()
    if c.closed || c.root.Err() != nil || err != nil {
        c.publish(f, nil, err)    // err may be nil; bytes discarded either way
    } else {
        if len(c.resident) == c.capacity {
            c.evictLRU()          // victim saved before unlink
        }
        stored := copyBytes(b)    // nil stays nil
        e := &entry{key: f.key, val: stored}
        c.resident[f.key] = e
        c.linkMRU(e)
        c.publish(f, stored, nil)
    }
    c.mu.Unlock()                 // explicit; not a defer; runs before any re-panic

    if panicVal != nil {
        panic(panicVal)
    }
```

`errLoaderPanic` is an unexported `errors.New("blockcache: loader panic")`. It is a join-safety signal only. It is not exported, not wrapped as `ErrClosed`, and not part of the public contract. Callers of a panicking Loader observe some non-nil error that is not `ErrClosed`. They must not depend on its text. A later `Get` of that key, while the cache is still open, calls the Loader again, because the panic path publishes with a non-nil error and does not insert. Rule 3 applies: a non-nil error is not stored.

The recover runs inside the inner function, before `mu` is acquired. If `load` returns, `panicVal` stays nil and the outer function takes `mu` once. If `load` panics, the inner recover records `errLoaderPanic`, the outer function takes `mu` once, publishes, unlocks, then re-panics. There is no second lock and no deferred unlock stacked under a recover that also locks.

If a statement after `load` returns panics (`evictLRU`, `linkMRU`, `publish`, map insert), that panic happens while this function holds `mu`. There is no recover defer that tries to lock again. That panic is a bug in the cache, not a Loader panic. It must not be swallowed. `loads.Done` still runs because it is deferred on `run`, so `Close` can finish the join after the goroutine dies. `f.done` may already be closed if `publish` ran, or still open if the panic was before `publish`. The implementation keeps the post-`load` section small and straight so that path does not panic: `evictLRU` only runs when `len(resident) == capacity >= 1`, which implies a non-nil `tail`, and it saves that pointer before unlink.

`loads.Done` is deferred on `run` and runs after the explicit unlock and after a re-panic starts unwinding. `Close` can finish the join.

The success path and the discard path both call `publish` once. They do not fall through into a second close. The inner recover does not call `publish` and does not touch `f.done`.

`defer cancel()` on the child runs when `run` returns, after publish has stored or discarded and after `mu` is unlocked. It does not affect the admit decision for this flight. It does not run while `mu` is held.

If the Loader returns `(non-nil, non-nil)`, `err != nil`, so the discard branch runs, the bytes are not copied, and nothing is inserted.

## Edge cases

- Empty key `""` is a normal key. Eviction never deletes by a zero key left over from a nil tail.
- Capacity 1: the list is a sole node after the first admit. The next successful admit saves that node, unlinks it (empty invariant), deletes it by the saved key, then `linkMRU`s the new one. `head` and `tail` both point at the survivor.
- A second load of a key cannot start while the first flight is in `inflight`. After publish, the key is either resident or absent. A new Get then hits or starts a new flight. An older flight cannot admit after a newer one: there is only one flight per key, and it admits or discards before it leaves `inflight`.
- `Get` with an already-cancelled caller context on a cold key still starts the load, then returns `ctx.Err()` from the wait loop. Success still populates the cache.
- `Get` with an already-cancelled caller context on a resident key returns the copy.
- Many concurrent `Close` calls: one owner joins, the rest receive on `joined`, all return nil.
- `Len` during `Close`: the pre-drop count or 0, never above capacity. It holds `mu`, so it does not observe a half-cleared map. After `Close` returns, `Len` is 0. A `Len` from a Done callback after `cancel` takes `mu` and returns the current resident count.
- A waiter whose caller context fires in the same window as Close observes `closed` first and returns `ErrClosed`, not `ctx.Err()`.
- A waiter whose load resolves in the same window as its caller context, with the cache still open, observes `f.done` first and returns the load result, not `ctx.Err()`.
- A Loader panic while the cache is open: current waiters get the unpublished join-safety error and a nil slice; the key is absent; a later Get calls the Loader again.
- A Loader panic after Close has set `closed`: publish still runs, with the panic error, and does not insert. Waiters that recheck see `closed` and return `ErrClosed`. `loads.Done` still runs, then the panic is re-raised.

## Files

- `worktree/blockcache.go` — `New`, `cache`, `Get`, `Len`, `Close`, `run`, `publish`, list helpers, `copyBytes`.
- `worktree/blockcache_public_test.go` — already present; do not edit.
- `worktree/go.mod` — already present; do not edit.

No other files. Helpers stay in the same file. Comments only where the closed-check priority, the `started` versus `joined` split, the admit-under-lock rule, the single `mu` owner in `run`, or unlock-before-cancel would otherwise look accidental.

## Test approach

Public suite, run from `worktree`:

```
go test -count=1 .
go test -race -count=1 .
go vet .
```

Expected coverage of the shipped tests:

| Test | Rules |
| --- | --- |
| `TestHitReturnsCopy` | 1, 4: second Get does not call Loader; mutation of the first result does not change the second |
| `TestSingleFlight` | 2: 50 overlapped Gets, exactly one Loader entry, Loader still blocked at `synctest.Wait`, all 50 get `block:k` |
| `TestErrorNotCached` | 3: first error is `errors.Is` the Loader error and nil bytes; second Get reloads and succeeds; Loader calls == 2 |
| `TestLRUEviction` | 5, 6: capacity 2, hit refreshes recency, victim is the LRU key, survivor is not reloaded |
| `TestLenBounded` | 5: `Len` never exceeds 3 and ends at 3 |
| `TestClosedReturnsErrClosed` | 8: `Close` nil, `Get` is `errors.Is(..., ErrClosed)`, second `Close` nil |
| `TestNewPanics` | `New(0, load)` and `New(1, nil)` panic |

`TestSingleFlight` uses `t.Errorf` inside the bubble before release so a failure does not skip the release and deadlock the bubble. The implementation must block inside the Loader on the test's unclosed `release` channel and must not call the Loader more than once. It must make progress when `release` is closed, with no extra synchronization the test does not provide.

Rules the public suite does not fully exercise, which the implementation still follows so a hidden grader can:

- Orphaned load: last waiter cancels, Loader later succeeds, key is resident, a new `Get` hits.
- Close while a Loader is blocked and honors cancel: `Close` cancels `root` after releasing `mu`, waiters already have `ErrClosed` before the Loader returns, `Close` then joins, the result is not stored, `Len` is 0, a later `Get` is `ErrClosed`.
- Close while a Loader ignores cancel: waiters still return `ErrClosed` as soon as `started` is closed. `Close` stays in `loads.Wait` until that Loader returns. When it does, the admit section sees `closed` and discards. After `Close` returns, `resident` is empty.
- Close versus a Loader that has already returned and is blocked on `mu`: that `run` either admits before Close's section (entry dropped after join) or sees `closed` and does not admit. It cannot admit after the drop.
- `(bytes, err)` from the Loader: error returned, nothing stored, next Get reloads.
- Each single-flight waiter gets a distinct slice.
- Caller context cancelled while waiting, cache open: that `Get` returns `ctx.Err()`, the Loader still runs, success is stored.
- `ErrClosed` wins over a cancelled caller context when Close has set `closed`.
- Concurrent `Len` never above capacity (`go test -race`).
- Idempotent overlapping `Close`.
- Capacity 1 eviction: victim pointer saved, sole-node unlink, delete by that key, new node linked.
- Nil success stays nil. Non-nil empty success is stored and returned as an independent non-nil empty slice.
- A Loader error is not rewritten to `ErrClosed` while the cache is open.
- A Loader panic does not double-lock or double-unlock `mu`, does not store the key, and a later Get calls the Loader again.
- A Done callback on the Loader context that calls `Get` or `Len` does not deadlock, because `cancel` runs without `mu` held and `closed` is already set.

No extra test file is part of the deliverable. The public file is the acceptance suite. Hidden tests are assumed to live outside this tree and to import `blockcache` against the same API.

## Implementation order

1. Types, `New` (panic checks, `root` context, maps, `started`, `joined`), `copyBytes`.
2. List operations (`linkMRU`, `unlink`, `moveToMRU`, `evictLRU` with a saved victim) and `Len`, matching the empty, one-node, and unlink cases above.
3. `Get` hit path and miss/single-flight start.
4. `run` with a single `publish`, one `mu` lock/unlock pair after the Loader returns, success path with eviction, error path without insert, inner recover that does not touch `mu`, child context cancelled when `run` returns.
5. Wait loop: `closed`, then `f.done`, then caller `ctx`, selecting on `started` rather than on the join signal.
6. `Close`: set `closed`, close `started`, unlock, then `cancel`, then wait loads outside `mu`, drop resident, close `joined`.
7. `go test`, `go test -race`, `go vet`.

## Out of scope

No metrics, no TTL, no `Delete`, no per-caller Loader context, no weight-based capacity, no second lock. Keys are strings as specified. Capacity is a count of resident entries, not bytes. The implementation does not cancel an individual flight except by closing the cache. The panic error is not exported.
