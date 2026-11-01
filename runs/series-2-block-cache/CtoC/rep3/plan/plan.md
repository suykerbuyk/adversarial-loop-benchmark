# Implementation Plan — `blockcache` (bounded, concurrent, single-flight block cache)

Round 2. This revision addresses every item in `plan_findings.md`. The point-by-point response is in `plan_rebuttal.md`. Changes since round 1:
- the Close order is now cancel, then join, then wake/drop (§3.7);
- `wait` settles on the `closed` flag (§3.3);
- `run` resolves its call in a defer (§3.4);
- `sync.Once` is replaced by a channel-based shared join (§3.7);
- new and deterministic tests have been added (§6).

## 0. Files

| File | Purpose |
|---|---|
| `worktree/blockcache.go` | The whole package: the public API and the private implementation. |
| `worktree/blockcache_internal_test.go` | Extra tests (§6), in package `blockcache` so they can call `wait` directly for the readiness-tie tests. |
| `worktree/blockcache_public_test.go` | The public suite, left as shipped. |

Only the standard library is used: `container/list`, `context`, `errors`, `fmt`, `sync`. `go.mod` stays as it is (`module blockcache`, `go 1.25`). `testing/synctest` with `synctest.Test` is available.

## 1. Public API (exactly as in the spec, with no extra exported identifiers)

```go
// Package blockcache provides a bounded, concurrent, single-flight LRU cache of
// byte blocks in front of a slow backing store.
package blockcache

type Loader func(ctx context.Context, key string) ([]byte, error)

type Cache interface {
    Get(ctx context.Context, key string) ([]byte, error)
    Len() int
    Close() error
}

var ErrClosed = errors.New("blockcache: closed")

func New(capacity int, load Loader) Cache
```

`New` panics with `"blockcache: capacity must be >= 1"` when `capacity < 1`, and with `"blockcache: nil Loader"` when `load == nil`. It returns an unexported `*cache`.

Godoc must state these points (the findings asked for them to be pinned):
- **Get, pre-cancelled ctx on a miss:** the single-flight load is still started (or joined), so it warms the cache as an orphan load (rule 7). The call itself returns `ctx.Err()` unless the cache is closing or the load has already resolved. This is deliberate; do not short-circuit it.
- **Get, nil ctx:** a nil ctx is a caller bug. Get panics with `"blockcache: nil Context"` at entry, on every path (hit, miss, closed), so the behaviour is consistent.
- **Get, ctx on a hit:** a hit does not block, so the ctx is not consulted (rule 7's priority applies to a *waiting* Get).
- **Get, (nil, nil) from the Loader:** this stores an empty block. Get then returns a non-nil, zero-length `[]byte{}` with a nil error.
- **Get after Close starts:** once Close has begun, a Get that reaches the closed check returns `ErrClosed` at once (it does not wait for Close's join).
- **Loader:** it must return promptly when its ctx is Done, because Close (and any waiter caught by a closing cache) depends on that. A Loader must not call `Close` on its own cache. A Loader that panics or calls `runtime.Goexit` resolves its flight with an error (§3.4).

## 2. Internal data model

```go
type cache struct {
    capacity int
    load     Loader

    ctx    context.Context    // cache-scoped; every Loader call gets this
    cancel context.CancelFunc

    loads    sync.WaitGroup   // one count per running loader goroutine
    closedCh chan struct{}    // closed exactly once, AFTER the join; wakes waiters and later Closes

    mu       sync.Mutex       // guards every field below
    closed   bool             // set at the start of Close ("closing has begun")
    lru      *list.List                 // front = MRU, back = LRU; Element.Value is *entry
    items    map[string]*list.Element   // resident entries only
    inflight map[string]*call           // unresolved loads only (not resident, not counted)
}

type entry struct {
    key string
    val []byte // cache-owned; never written after creation
}

type call struct {
    done chan struct{} // closed exactly once, when the load resolves
    val  []byte        // cache-owned copy; written before close(done), read-only afterwards
    err  error         // written before close(done)
}

var errLoaderExited = errors.New("blockcache: loader exited without returning")
```

Invariants (written as comments in the code):
- **I1** `len(items) == lru.Len() <= capacity` at the end of every critical section.
- **I2** A key is never in both `items` and `inflight`.
- **I3** `entry.val` and `call.val` are never written after they are published, so copying out of them without `mu` is race-free. The happens-before edge comes from the mutex release, or for `call` from `close(done)`.
- **I4** `closed = true` and `cancel()` happen in the same critical section. After that, no `loads.Add` runs and no entry is inserted. So "Loader returned after the cache ctx was cancelled" is the same as "`run` observes `closed` under `mu`".
- **I5** `loads.Add(1)` only runs under `mu` while `closed == false`. So every `Add` happens-before Close's `loads.Wait()`.
- **I6** `closedCh` is closed only after `loads.Wait()` has returned. So any goroutine that observes `closedCh` closed knows that every Loader call has returned.

## 3. Algorithms

### 3.1 `New`
Validate arguments and panic if they are bad. Then `ctx, cancel := context.WithCancel(context.Background())`, and make `closedCh`, `lru` and both maps. `New` starts no goroutines.

### 3.2 `Get(ctx, key)`
```
if ctx == nil { panic("blockcache: nil Context") }
mu.Lock()
if closed            { mu.Unlock(); return nil, ErrClosed }   // rule 8: no Loader call
if el, ok := items[key]; ok {                                  // rules 1, 6, 4
    lru.MoveToFront(el); v := el.Value.(*entry).val; mu.Unlock(); return clone(v), nil
}
cl := inflight[key]
if cl == nil {                                                 // first miss starts the flight (rule 2)
    cl = &call{done: make(chan struct{})}
    inflight[key] = cl
    loads.Add(1)                                               // under mu with closed==false (I5)
    go c.run(key, cl)
}
mu.Unlock()
return c.wait(ctx, cl)
```
`clone(b)` is `out := make([]byte, len(b)); copy(out, b); return out`. It always returns a fresh, non-nil slice.

A miss with a pre-cancelled ctx still starts or joins the load. This is deliberate and documented (see §1).

### 3.3 `wait(ctx, cl)` — rule 7 priority, and decisions about Close timing
**Decision:** "the cache closes" in rule 7(a) starts when Close sets `closed = true`. From that point `ErrClosed` beats both ctx expiry and a resolved load result. But per rule 8 and I6, a waiter is only handed `ErrClosed` *after* the join, when `closedCh` closes. So no waiter can see `ErrClosed` while some Loader is still running.

```
select { case <-closedCh: case <-cl.done: case <-ctx.Done(): }  // block until something fires
// Settle by priority, not by select's random choice:
mu.Lock(); closing := closed; mu.Unlock()
if closing {                       // (a) cache is closing or closed
    <-closedCh                     // wait for the join (bounded: Loaders must return promptly)
    return nil, ErrClosed
}
select {
case <-cl.done:                    // (c) resolved, and the cache was not closing
    if cl.err != nil { return nil, cl.err }
    return clone(cl.val), nil
default:
}
return nil, ctx.Err()              // (b) ctx Done while the load is still in flight
```
- If `closedCh` fired, `closed` is necessarily true (it was set first), so the result is (a).
- If `cl.done` fired from `run`'s closed branch, `closed` is true, so the result is (a): the waiter waits for the join and returns `ErrClosed`. The fallback `cl.err` is never handed out early.
- If ctx fired while Close is joining, `closed` is true. The waiter blocks on `closedCh` until the join ends and then returns `ErrClosed`. This briefly outlives the caller's deadline, but it is bounded by the Loader contract ("return promptly when ctx is Done"; the cache ctx is already cancelled). The trade-off is documented in Get's godoc: it is what lets the (a)-over-(b) priority and the "join before wake" order both hold.
- (b) vs (c): rule (b) only applies "while the load is still in flight", so an already-closed `cl.done` returns its result even when ctx is also Done.
- Errors are returned unwrapped, so `errors.Is(err, loaderErr)` holds. `ErrClosed` is the sentinel itself. On error the bytes are `nil`.
- Every blocking operation is a channel receive, so a blocked Get is durably blocked under synctest.

### 3.4 `run(key, cl)` — the one Loader call per flight, always resolved exactly once
```
func (c *cache) run(key string, cl *call) {
    defer c.loads.Done()                        // runs last: the join covers resolve()
    var (b []byte; err error; returned bool)
    defer func() {
        if !returned {                          // panic or runtime.Goexit inside the Loader
            if r := recover(); r != nil {
                err = fmt.Errorf("blockcache: loader panicked: %v", r)
            } else {
                err = errLoaderExited           // Goexit (e.g. t.FailNow inside a Loader)
            }
            b = nil
        }
        c.resolve(key, cl, b, err)
    }()
    b, err = c.load(c.ctx, key)                 // cache-scoped ctx, never a caller's (rule 7)
    returned = true
}

func (c *cache) resolve(key string, cl *call, b []byte, err error) {
    c.mu.Lock()
    defer c.mu.Unlock()
    delete(c.inflight, key)
    switch {
    case c.closed:            // returned after the cache ctx was cancelled: discard (rule 8, I4)
        cl.err = ErrClosed    // fallback only; wait() reports (a) via closedCh after the join
    case err != nil:          // error wins; bytes discarded; not stored (rule 3)
        cl.err = err
    default:
        owned := clone(b)     // never keep the Loader's slice (rule 4)
        cl.val = owned
        c.admit(key, owned)   // rules 5, 6
    }
    close(cl.done)
}
```
- **Policy:** a panic in a Loader is recovered into an error and delivered to every waiter of that flight. The key is not stored, and the next Get re-loads. Without this, a panic in the cache-owned goroutine would kill the process, and the caller could not recover it. `runtime.Goexit` is turned into `errLoaderExited`. Both cases ensure that a flight always resolves, `inflight` never holds a dead key, and `loads.Done` always runs (rule 9: no hangs). Both are documented on `Loader`.
- `recover()` is called directly in the deferred function, so it works. During Goexit it returns nil, which selects the `errLoaderExited` branch.
- Inflight deletion, admission and `close(done)` happen in one critical section. So a later Get sees either the resident entry or nothing (after an error), and no Get can join a call that has already resolved.

### 3.5 `admit(key, val)` (caller holds `mu`)
```
if _, ok := items[key]; ok { panic("blockcache: invariant violated: admitting a resident key") } // I2
for lru.Len() >= capacity {                       // evict LRU first (rule 5)
    back := lru.Back(); lru.Remove(back); delete(items, back.Value.(*entry).key)
}
items[key] = lru.PushFront(&entry{key: key, val: val}) // new entry is MRU (rule 6)
```
Eviction and insertion share one critical section, so `Len()` never sees `capacity+1`. An unresolved load lives only in `inflight`, so it never counts toward capacity and is never evicted. The silent-overwrite branch from round 1 is now an invariant panic.

### 3.6 `Len()`
`mu.Lock(); n := lru.Len(); mu.Unlock(); return n`. It counts resident entries only, and by I1 it never exceeds capacity.

### 3.7 `Close()` — spec order: cancel → join → wake → drop
```
mu.Lock()
if closed {                       // a second or later Close
    mu.Unlock()
    <-closedCh                    // share the first Close's join; nil once it has finished
    return nil
}
closed = true                     // from here, Gets return ErrClosed and no load can start (I4, I5)
cancel()                          // 1. cancel the cache-scoped ctx
mu.Unlock()

loads.Wait()                      // 2. wait for every in-flight Loader call to return (outside mu)

mu.Lock()
lru.Init(); clear(items)          // 4. drop all entries
close(closedCh)                   // 3. wake every waiter (they return ErrClosed) and later Closes
mu.Unlock()
return nil
```
- **Order:** this follows rule 8 and the godoc literally. Waiters only get `ErrClosed` once every Loader has returned (I6). Wake and drop happen in the same critical section, so their relative order cannot be observed. During the join, `Len()` may still report the old entries. That is a stale view but stays within capacity, and no entry can be added after `closed` (I4). Loaders that return during the join see `closed` and are discarded (§3.4), which covers both the zero-waiter case and the close-window case. So once Close returns, `Len()==0` and stays 0.
- **Idempotent, with a shared join:** a concurrent second Close blocks on `closedCh` until the first Close's join has finished, then returns nil. Later Closes return nil at once. This replaces round 1's `sync.Once`. A goroutine blocked inside `Once.Do` waits on a mutex, which synctest does *not* treat as durably blocked, so a deterministic concurrent-Close test would hang in `synctest.Wait`. A channel receive is durably blocking.
- **WaitGroup under synctest:** every `loads.Add` runs inside the bubble's goroutines, so `loads.Wait()` is durably blocking (Go ≥1.25 semantics). A Close parked in the join can therefore be detected with `synctest.Wait()`.
- **No deadlock:** `mu` is never held while calling the Loader, in `loads.Wait()`, or while blocking in `wait`. `resolve` only takes `mu` briefly.
- Each `run` removes its own `inflight` key, so `inflight` is empty once `loads.Wait()` returns.

## 4. Rule-by-rule traceability

| Rule | Where satisfied |
|---|---|
| 1 Hit | §3.2 hit branch: `MoveToFront` and `clone`, no Loader call. |
| 2 Single-flight | §3.2: `inflight` is checked and set under one lock, so exactly one `run` per flight. Every waiter clones `cl.val` (§3.3). On success `admit` (§3.4). |
| 3 Error | §3.4 `resolve`: `err != nil` wins over the bytes, nothing is stored, and `inflight` is deleted, so the next Get re-loads. Panic and Goexit also produce errors. |
| 4 Ownership/copy | `clone` when storing and `clone` on every return. Stored slices are immutable (I3). |
| 5 Capacity/LRU | §3.5: evict and insert in one critical section. In-flight loads stay in `inflight`. `Len` = `lru.Len()`. |
| 6 Recency | `MoveToFront` on a hit; `PushFront` on admit. |
| 7 Ctx/orphans | The Loader gets `c.ctx`. `run` does not depend on waiters, so an orphan load still admits. `wait` priority is (a) closing > (b, only if unresolved) > (c), settled deterministically (§3.3). |
| 8 Close | §3.7: cancel → join → wake/drop. A shared join via `closedCh`. Results arriving after cancel are discarded (§3.4). Gets check `closed` under `mu` first (§3.2). |
| 9 Concurrency | All mutable state is under one mutex. Published bytes are immutable. Every flight resolves exactly once, even on panic or Goexit. There is no lock-order issue (only one lock) and no lock is held across blocking. |

## 5. Misuse notes (documented)
- A Loader that calls `Close` on its own cache deadlocks: Close would join itself. A Loader that calls `Get` for its own key waits for itself until the inner ctx expires. Both are contract violations and are documented on `Loader`.
- A Loader that ignores ctx cancellation makes Close, and any waiter caught by a closing cache, wait for it. Rule 8 requires this ("waits for every in-flight Loader call to return").

## 6. Test approach

Commands, all of which must pass:
- `gofmt -l .` must print nothing.
- `go vet ./...`
- `go test ./...`
- `go test -race -count=50 ./...`

The public suite is used unchanged. Each internal test bounds its waits with a 5s ctx. Overlap-sensitive tests use `synctest.Test` plus `synctest.Wait()`. Inside bubbles, assertions made while goroutines are parked use `t.Errorf` (not `t.Fatalf`), as the public suite does.

**Copy / ownership / errors**
1. **Loader slice not retained:** the Loader returns a slice it keeps and mutates after returning. A later hit returns the original bytes.
2. **Per-waiter copies under single flight (synctest):** N waiters each get a distinct backing array (`&r[0]` differs). Mutating one does not affect the others or a later hit.
3. **(bytes, err): error wins:** Get returns `nil, err`, `Len()==0`, and the next Get calls the Loader again.
4. **Error fan-out (synctest):** N waiters on one failing load all get `errors.Is(err, boom)`, with exactly 1 Loader call.
5. **(nil, nil) pinned:** Get returns a non-nil `[]byte{}` with a nil error, `Len()==1`, and a second Get is a hit.
6. **Loader panic:** Get returns a non-nil error containing "panicked", and the process survives. The next Get re-loads (count 2), and `Close` returns nil.
7. **Loader Goexit:** the Loader calls `runtime.Goexit()`. Get returns `errLoaderExited` within the deadline (no hang), and the next Get re-loads.

**Ctx and priority**
8. **Orphaned load populates (synctest):** the only waiter's ctx is cancelled while the Loader blocks, and the waiter gets `context.Canceled`. Then the Loader is released. After `synctest.Wait()`, `Len()==1` and the next Get is a hit (count still 1).
9. **Pre-cancelled miss still loads exactly once and warms the cache:** Get with an already-cancelled ctx returns `context.Canceled`. After the load finishes, Get with a live ctx hits, and the Loader count is 1. This pins the documented policy.
10. **(b) vs (c):** a ctx cancelled while the load is in flight gives `ctx.Err()`. A resolved load wins over a ctx that is also Done: call `wait` directly with `cl.done` closed and ctx cancelled, and expect the result.
11. **Readiness ties in `wait`, called directly, with every combination ready at once:**
    - `closedCh` closed, `cl.done` closed (success) and ctx cancelled, with `closed=true`: expect `ErrClosed`;
    - `cl.done` closed (success) and ctx cancelled, not closing: expect the bytes;
    - `closedCh` and ctx only: expect `ErrClosed`.

    Each case runs in a loop of 1000 iterations, so `select`'s random choice is fully exercised.
12. **(a) over (c), end to end (synctest):** the Loader ignores ctx and blocks on `release`. One waiter is parked. Start Close in a goroutine and call `synctest.Wait()` (Close is parked in the join). Release, so the Loader returns *success*. Expect: the waiter gets `ErrClosed`, Close returns nil, `Len()==0`, and a later Get gets `ErrClosed` with no new Loader call.
13. **(a) over (b), end to end (synctest):** as in test 12, but after `synctest.Wait()` (Close parked in the join) cancel the waiter's ctx. Then `synctest.Wait()` again. The waiter must still be parked (it waits for the join; check with an atomic "returned" flag using `t.Errorf`). Release. The waiter gets `ErrClosed`, not `context.Canceled`.

**Close**
14. **Join before wake (the finding's scenario, synctest):** the Loader does `<-ctx.Done()`, then `time.Sleep(10ms)` (fake time), then `flag.Store(true)`, and returns. One waiter is parked, then Close runs. When the waiter gets `ErrClosed` it reads `flag`, which must be true. After Close returns, `flag` is true and the Loader saw `ctx.Err()==context.Canceled`.
15. **Close wakes all waiters (synctest):** with N waiters across 3 keys, all get `errors.Is(err, ErrClosed)`.
16. **Close-window discard, deterministic (synctest):** the Loader ignores ctx and blocks on `release`. One waiter is parked. Start Close in a goroutine and call `synctest.Wait()`. Release, then `synctest.Wait()`. Assert: Close returned nil, the waiter got `ErrClosed`, `Len()==0`, and Get gives `ErrClosed` with the Loader count unchanged.
17. **Zero-waiter discard variant (synctest):** as in test 16, but the only waiter's ctx is cancelled before Close, so it returns `context.Canceled` and the load is orphaned. Close is parked in the join, then release. Expect `Len()==0` after Close.
18. **Concurrent Close shares the join (synctest):** the Loader is blocked, and two Closes run in goroutines. After `synctest.Wait()`, neither has returned (atomic flags). Release. Both return nil. A third Close returns nil at once.
19. **Get after Close calls no Loader:** the count stays 0. A Get during the join (between `closed=true` and the end of the join, synctest) also returns `ErrClosed` without calling the Loader.

**Capacity / LRU / concurrency**
20. **In-flight not counted or evictable (synctest):** capacity 1, `a` resident, the load of `b` blocked. `Len()==1` and `a` hits. After release, `b` is resident and `a` is evicted.
21. **Recency on load:** capacity 2. Load a, then b, then c: a is evicted and b survives.
22. **Len under concurrent poll:** 32 goroutines Get random keys out of 100 with capacity 8, while a poller asserts `Len() <= 8`, under `-race`.
23. **Stress with Close:** many Gets with a Close in the middle. Every result is valid bytes, `ErrClosed`, or a ctx error. Everything returns before the deadline.
24. **Panics in New and Get:** `New(-1, load)` panics, and `Get(nil, k)` panics on both the hit and the miss path.

## 7. Done criteria
- Builds; `gofmt`/`go vet` clean.
- The public suite and the internal suite pass, including `-race -count=50`.
- Godoc on every exported identifier, covering the policies in §1. Code comments cover I1–I6, the `wait` settle logic, and the Close order.
