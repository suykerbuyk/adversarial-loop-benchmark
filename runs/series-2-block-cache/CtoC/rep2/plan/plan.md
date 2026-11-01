# Implementation Plan: `blockcache` (bounded, concurrent, single-flight block cache)

Round 2. This revision answers `plan/plan_findings.md` (round 1). The point-by-point response is in
`plan/plan_rebuttal.md`. Changes from round 1, in summary:
- Close now wakes waiters **at the start of Close**, through a cache-wide `closedCh`, which is closed in
  the same critical section that sets `closed`. `wait` resolves in the fixed order closed → done → ctx.
  This fixes the major finding: rule 7(a) priority during the join window.
- Entries are dropped in that same first critical section, so `Len()==0` as soon as any waiter has seen `ErrClosed`.
- A Loader panic becomes an exported `*PanicError`. It keeps the key, the panic value and the stack.
- When a Loader calls Get for its own key, this is detected through a context tag and returns `ErrSelfGet`. It does not deadlock.
  Close from inside a Loader is documented as a precondition violation in the doc comments.
- Dead code and redundant checks are removed (nits). The nil-ctx behaviour is documented.
- New synctest cases cover the close window and Get-during-join.

## 0. Files

| File | Contents |
|---|---|
| `worktree/blockcache.go` | Public API and the implementation. One file of about 250 lines. |
| `worktree/blockcache_public_test.go` | The shipped public suite, unchanged. |
| `worktree/blockcache_extra_test.go` | Extra tests (§6). |

Module `blockcache`, `go 1.25`, so `testing/synctest.Test` is available. Only the standard library is used:
`container/list`, `context`, `errors`, `fmt`, `runtime/debug`, `sync`.

## 1. Public API

Verbatim from the spec:
```go
type Loader func(ctx context.Context, key string) ([]byte, error)
type Cache interface {
    Get(ctx context.Context, key string) ([]byte, error)
    Len() int
    Close() error
}
func New(capacity int, load Loader) Cache   // panics if capacity < 1 || load == nil
var ErrClosed = errors.New("blockcache: closed")
```
Two documented additions. Both are additive and the spec API is unchanged:
```go
// ErrSelfGet is returned by Get when it is called, with the Loader's ctx (or a ctx
// derived from it), from inside the Loader call that is loading that same key.
// Waiting would deadlock, because the load waits on itself.
var ErrSelfGet = errors.New("blockcache: Loader called Get for the key it is loading")

// PanicError is the error waiters receive when the Loader panics. Like any Loader
// error it is not cached (rule 3). It keeps the panic value and the stack, so a
// programming bug can be told apart from a transient load failure.
type PanicError struct {
    Key   string
    Value any    // the value passed to panic
    Stack []byte // debug.Stack() captured in the recovering goroutine
}
func (e *PanicError) Error() string  // "blockcache: loader panicked for key %q: %v\n\n<stack>"
func (e *PanicError) Unwrap() error  // returns Value if it is an error, else nil
```
`ErrClosed` is returned unwrapped, so both `==` and `errors.Is` work. The concrete type `*cache` is unexported.

Doc-comment obligations, written into the code:
- **Get:** `ctx` must be non-nil. A nil ctx panics, as in the stdlib. A hit is served even when ctx is Done.
- **Loader / Close:** a Loader must not call `Close` on its own cache. Close waits for every in-flight Loader call to return (rule 8), so a Loader that calls it deadlocks by construction.
  A Loader may call `Len`, and `Get` for other keys. If it calls `Get` for its own key with its ctx, it gets `ErrSelfGet`.
  A key cycle between Loaders (A's Loader calls Get(B), and B's Loader calls Get(A)) cannot be detected. It ends only through the callers' ctx or through Close.

## 2. Internal data model

```go
type cache struct {
    capacity int
    load     Loader

    ctx    context.Context    // cache-scoped; parent of every Loader ctx
    cancel context.CancelFunc

    mu       sync.Mutex                // guards the fields below, up to loaders
    closed   bool                      // set once, by the first Close
    entries  map[string]*list.Element  // key -> element of lru; Value is *entry
    lru      *list.List                // front = MRU, back = LRU; resident entries only
    inflight map[string]*call          // unresolved loads; NOT in lru, not counted toward capacity

    closedCh  chan struct{}  // closed (under mu) in the same critical section that sets closed
    loaders   sync.WaitGroup // one Add per started Loader goroutine (Add only under mu while !closed)
    closeDone chan struct{}  // closed when the first Close has finished its join
}

type entry struct { key string; val []byte }               // val is cache-owned and never mutated

type call struct {
    done chan struct{} // closed exactly once, under mu, when the load resolves
    val  []byte        // cache-owned copy (nil on error); written before close(done)
    err  error         // written before close(done)
}

type loadTagKey struct{}                  // context key
type loadTag struct { c *cache; key string } // value attached to each Loader's ctx
```

Invariants (all hold whenever `mu` is not held):
- **I1:** `lru.Len() == len(entries) <= capacity`.
- **I2:** a key is never in both `entries` and `inflight`. A key leaves `inflight` in the same critical section that may add it to `entries`.
  So for any key there is at most one `call`, and runLoad may `delete(inflight, key)` unconditionally.
- **I3:** `entry.val` and `call.val` are never written after publication, and readers only copy them out.
- **I4:** once `closed` is set, nothing is added to `entries` or `inflight`, and `loaders.Add` is never called again.
- **I5:** `call.val` and `call.err` are written before `close(call.done)`. That close gives the happens-before edge to waiters.
- **I6:** `closed == true` ⇔ `closedCh` is closed (as observed under mu). Both change in one critical section, so anyone who sees `closedCh` closed would also see `closed`.
- **I7:** after `loaders.Wait()` returns inside Close, `inflight` is empty. Every runLoad deletes its own key before calling `Done`, and by I4 nothing new is added.

## 3. Algorithms

### New
Panic with a descriptive message if `capacity < 1` or `load == nil`. Set up `ctx, cancel = context.WithCancel(context.Background())`.
Allocate the maps, the list, `closedCh` and `closeDone`. Start no goroutines.

### Get(ctx, key)
```
if t, ok := ctx.Value(loadTagKey{}).(*loadTag); ok && t.c == c && t.key == key {
    return nil, ErrSelfGet                 // re-entrant own-key Get; would wait on itself forever
}
lock
if closed                 { unlock; return nil, ErrClosed }                   // rule 8: no Loader call
if el, ok := entries[key] { lru.MoveToFront(el); v := el.Value.(*entry).val; unlock; return clone(v), nil } // rules 1, 6
if err := ctx.Err(); err != nil { unlock; return nil, err }                   // dead caller neither starts nor joins a load
cl, ok := inflight[key]
if !ok {
    cl = &call{done: make(chan struct{})}
    inflight[key] = cl
    loaders.Add(1)                         // under mu while !closed (I4), so Close's Wait cannot miss it
    go c.runLoad(key, cl)
}
unlock
return c.wait(ctx, cl)
```
- `clone(v)` is `b := make([]byte, len(v)); copy(b, v)`. It always returns a fresh, non-nil slice, and it runs outside the lock (safe by I3).
- The self-Get check comes before the closed check, so it is deterministic. It only fires when the ctx carries *this* cache's tag for *this* key. An unrelated cache, or another key, is unaffected.
- A hit is served even when `ctx` is Done. Rule 7(b) applies only to a *waiting* Get.

### wait(ctx, cl): the fixed resolution order is rule 7
```
select {
case <-cl.done:
case <-ctx.Done():
case <-c.closedCh:
}
select { case <-c.closedCh: return nil, ErrClosed; default: }    // (a) the cache has closed
select { case <-cl.done:   return result(cl);      default: }    // (c) resolved load
return nil, ctx.Err()                                            // (b) ctx Done and the load is still in flight

result(cl): if cl.err != nil { return nil, cl.err }; return clone(cl.val), nil
```
- **(a) beats (b) in every window.** When Close begins, `closedCh` is closed at once. So a waiter that is woken by its own ctx during the join window still checks `closedCh` first and returns `ErrClosed`.
  This is exactly the finding's scenario: the Loader does `<-ctx.Done(); sleep 1s`, the waiter has a 100 ms deadline, and Close runs at t=0. The waiter gets `ErrClosed` (at t=0, in fact).
- **(a) beats (c).** Suppose a waiter is still inside Get when Close has begun and has not yet passed the post-wake check. It returns `ErrClosed` even if its load had already succeeded.
  This matches rule 8, "wakes every current waiter with ErrClosed". It also gives a simple rule: once Close has begun, no Get that is still pending returns bytes.
  A waiter that passed the check before Close began is linearized before the Close.
- **(b)** happens only when the cache is not closed and `cl.done` is still open, so the load really is in flight.
- **(c)** each waiter gets its own copy (rule 4). An error is passed through unchanged (rule 3).
- **Why the wait does not wait for the join.** Close's own return is still ordered after the join, and its other guarantees stand: every Loader has returned, every waiter has been woken with `ErrClosed`, and entries have been dropped.
  Waking waiters immediately is required for 7(a) to win over 7(b) in the join window. It also means a Loader's re-entrant Get ends on Close.
- All waiting is a `select` on channels only. synctest treats that as durably blocked, and waiters never block on `mu`.

### runLoad(key, cl): one goroutine per miss
```
defer loaders.Done()
val, err := c.callLoader(key)        // see below
var owned []byte
if err == nil { owned = clone(val) } // rule 4: copy outside the lock; the Loader's slice is never kept
lock
delete(inflight, key)                // unconditional: by I2 this key's only call is cl
switch {
case closed:     cl.err = ErrClosed  // rule 8: result discarded, nothing stored (the zero-waiter and close-window cases)
case err != nil: cl.err = err        // rule 3: the error wins; bytes are dropped; nothing stored
default:
    if lru.Len() >= capacity { evict lru.Back() from list and map }   // rule 5: evict before admitting
    entries[key] = lru.PushFront(&entry{key, owned})                  // rule 6: MRU
    cl.val = owned
}
close(cl.done)
unlock
```
- **Only `closed` is checked.** `cancel()` is only ever called after `closed=true` is set under mu, so `closed` covers the `ctx.Err()` case. A comment in the code states this.
- Eviction and admission share one critical section, so `Len()` can never see `capacity+1`.
- The load ignores waiters' contexts. It runs to completion and fills the cache even when every waiter has left (rule 7, orphaned loads).

**callLoader(key):**
```
lctx := context.WithValue(c.ctx, loadTagKey{}, &loadTag{c, key})
defer func() {
    if r := recover(); r != nil { val, err = nil, &PanicError{Key: key, Value: r, Stack: debug.Stack()} }
}()
return c.load(lctx, key)
```
The tag value lets Get detect a self-call. `lctx` is still cancelled when `c.ctx` is cancelled.

### Len()
`lock; n := lru.Len(); unlock; return n`. It counts resident entries only, and by I1 it is never above capacity.

### Close()
```
lock
if closed { unlock; <-closeDone; return nil }   // idempotent; a concurrent second Close waits for the same join
closed = true
close(closedCh)                                 // wake every current waiter with ErrClosed (rule 7a / 8)
entries = make(map...); lru.Init()              // drop all entries now; nothing can be re-added (I4)
unlock
cancel()                                        // cancel the cache-scoped ctx
loaders.Wait()                                  // wait for every in-flight Loader call to return
// I7: inflight is empty here. Every runLoad resolved its call with ErrClosed (it saw closed)
// and deleted its key, so no further clean-up is needed.
close(closeDone)
return nil
```
- `mu` is never held while blocking, so runLoad can always take it. No deadlock.
- After `closed=true`, no `Add` can happen (I4), so `Wait` cannot race with `Add`.
- When Close returns: every Loader has returned, every waiter has been woken with `ErrClosed`, `Len()==0`, and results that arrived late were discarded. Later Gets return `ErrClosed` without calling the Loader.
- A Get that is concurrent with Close and has not yet taken `mu` (that is, has not passed the closed check) sees `closed` and returns `ErrClosed` with no Loader call.

## 4. Robustness decisions

- **`(nil, nil)` from a Loader** is a successful empty block. Get returns a non-nil, zero-length slice.
- **`(bytes, err)`**: the error wins. The bytes are never copied or stored.
- **Loader panic** → `*PanicError` for every waiter of that load, not cached. The program survives and waiters are not stranded (rule 9), and callers can detect the condition with `errors.As`.
- **Re-entrancy:** an own-key Get with the Loader ctx returns `ErrSelfGet`. An own-key Get with an unrelated ctx is still woken by Close through `closedCh`.
  A Loader calling Close is a documented precondition violation. A cycle across keys is documented as undetectable.

## 5. Traceability

| Rule | Mechanism | Tests |
|---|---|---|
| 1 Hit | `entries` lookup, `MoveToFront`, `clone`, no Loader call | `TestHitReturnsCopy`, `TestLRUEviction` |
| 2 Single-flight | `inflight` checked and filled under mu; one runLoad per call; shared `call` | `TestSingleFlight` |
| 3 Error | no store on err; `inflight` entry deleted; bytes dropped | `TestErrorNotCached`, `TestErrorWinsOverBytes`, `TestErrorFanOut`, `TestLoaderPanic` |
| 4 Copy | clone on admission and on every return | `TestHitReturnsCopy`, `TestLoaderSliceNotRetained`, `TestWaitersGetDistinctCopies` |
| 5 Capacity/LRU | evict Back, then PushFront, in one critical section; inflight loads are not in lru | `TestLRUEviction`, `TestLenBounded`, `TestInflightNotCounted`, `TestLenNeverAboveCapRace` |
| 6 Recency | MoveToFront on a hit, PushFront on a load | `TestLRUEviction` |
| 7 Ctx/orphans | Loader ctx derives from `c.ctx`; wait order closed → done → ctx | `TestWaiterCtxCancel`, `TestOrphanedLoadPopulates`, `TestDeadCtxMiss`, `TestCloseWindowPriority` |
| 8 Close | `closed` + `closedCh` + drop, then cancel, then Wait; late results discarded; `closeDone` | `TestClosedReturnsErrClosed`, `TestCloseWaitsForLoader`, `TestCloseWakesWaiters`, `TestCloseDiscardsLateResult`, `TestGetDuringCloseJoin`, `TestConcurrentClose` |
| 9 Concurrency | one mutex, never held while blocking; channel-only waits | `-race` over everything, `TestStress`, `TestSelfGet` |

## 6. Test approach

Commands, run in `worktree/`: `gofmt -l .` (expect empty output), `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race -count=20 ./...`.

The extra tests live in package `blockcache`. Overlap is forced with `synctest.Test` and `synctest.Wait()`, and every wait has a 5s deadline. No `TestMain`. Every test calls Close in cleanup. In-bubble assertions use `t.Errorf` before goroutines are drained.
1. **TestErrorWinsOverBytes:** the Loader returns `([]byte("x"), boom)`. Get gives `(nil, boom)`, `Len()==0`, and the next Get loads again.
2. **TestErrorFanOut (synctest):** 10 waiters on one failing load. All get `boom`, with one Loader call.
3. **TestLoaderSliceNotRetained:** the Loader mutates its slice after returning. Later Gets are unaffected.
4. **TestWaitersGetDistinctCopies (synctest):** waiters mutate their results. There is no cross-talk, and a later hit still returns the original bytes.
5. **TestInflightNotCounted (synctest):** capacity 1, `a` resident. While `b` is blocked, `Len()==1` and `a` hits. After `b` resolves, `a` is evicted and `Len()==1`.
6. **TestLenNeverAboveCapRace:** concurrent Gets of distinct keys while a poller asserts `Len() <= cap`.
7. **TestWaiterCtxCancel (synctest):** a cancelled waiter gets `context.Canceled`. A live waiter still gets the bytes.
8. **TestOrphanedLoadPopulates (synctest):** the only waiter leaves. After release and `synctest.Wait()`, `Len()==1`, and the next Get hits with 1 Loader call.
9. **TestDeadCtxMiss:** a miss with a cancelled ctx returns `ctx.Err()` with no Loader call. A hit with a cancelled ctx returns the bytes.
10. **TestCloseWaitsForLoader (synctest):** the Loader does `<-ctx.Done(); time.Sleep(1s); flag=true`. Close returns nil, and only after the flag is set.
11. **TestCloseWakesWaiters (synctest):** N waiters on a blocked load. All get `errors.Is(err, ErrClosed)`, and `Len()==0`.
12. **TestCloseDiscardsLateResult (synctest):** the Loader ignores cancellation briefly and then returns success. After Close, `Len()==0`. A later Get returns ErrClosed with no new Loader call. Covers zero waiters too.
13. **TestCloseWindowPriority (synctest, finding 1):** the Loader does `<-ctx.Done(); time.Sleep(time.Second)`. The waiter's ctx has a 100 ms timeout, and Close is started in a goroutine at t=0.
    The waiter must get `ErrClosed`, not `DeadlineExceeded`. Variant: Close starts at t=50 ms and the waiter's deadline is 100 ms. Again `ErrClosed`.
14. **TestGetDuringCloseJoin (synctest, finding 2):** the same slow-to-cancel Loader. Start Close in a goroutine, then `synctest.Wait()`. Close is now parked in `loaders.Wait()`/the Loader's `Sleep`.
    Then Get for a new key and Get for the in-flight key both return `ErrClosed`, and the Loader call count is unchanged. Also assert that Close has not returned yet, so the Gets really did happen during the join.
15. **TestConcurrentClose (synctest):** two Closes while a Loader is being joined. Both return nil, and neither returns before the Loader has returned.
16. **TestLoaderPanic:** the Loader panics with a value. Get's error satisfies `errors.As(err, &pe)`, `pe.Value` and `pe.Key` match, `len(pe.Stack) > 0`, nothing is cached, and the next Get succeeds.
17. **TestSelfGet (synctest):** the Loader for `a` calls `c.Get(ctx, "a")` with its own ctx and gets `ErrSelfGet`. It calls `c.Get(ctx, "b")` and gets b's bytes. The outer Get returns normally, with no deadlock.
    Variant: the own-key Get uses `context.Background()`, and Close ends it with `ErrClosed`, so Close returns.
18. **TestStress:** mixed Get, Len and a final Close over a small key space and capacity, under `-race`, with no deadlock within the deadline.
