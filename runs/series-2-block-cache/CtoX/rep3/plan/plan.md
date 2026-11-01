# Implementation Plan — `blockcache` (bounded, concurrent, single-flight block cache)

Status: round 1. No `plan_findings.md` existed when this was written, so there is no rebuttal file.

## 0. Deliverable layout

- `worktree/blockcache.go`: the whole package (about 200 lines). Standard library only:
  `bytes`, `container/list`, `context`, `errors`, `sync`.
- `worktree/blockcache_public_test.go`: the shipped suite, left unchanged.
- `worktree/blockcache_internal_test.go`: extra tests (see §6) that cover the rules the public
  suite does not test.
- `go.mod` stays as it is (`module blockcache`, `go 1.25`; `testing/synctest.Test` is GA in 1.25).

## 1. Public API (exactly as in the spec)

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
`New` returns `*cache`, an unexported type. The package exports nothing beyond the spec.
Panic messages: `"blockcache: capacity must be >= 1"` and `"blockcache: nil Loader"`.

## 2. Internal data model

```go
type entry struct {
    key string
    val []byte          // cache-owned, never mutated after creation (immutable)
}

type call struct {      // one in-flight load (single-flight unit)
    done chan struct{}  // closed exactly once when the load resolves
    val  []byte         // written before close(done); read only after <-done
    err  error
}

type cache struct {
    capacity int
    load     Loader

    ctx    context.Context    // cache-scoped; passed to every Loader call
    cancel context.CancelFunc

    mu       sync.Mutex                // guards everything below
    ll       *list.List                // resident entries, front = MRU, back = LRU
    entries  map[string]*list.Element  // key -> element (Value is *entry)
    inflight map[string]*call          // key -> unresolved load
    closed   bool

    closedCh  chan struct{}   // closed (under mu) when Close starts; wakes all waiters
    loaders   sync.WaitGroup  // one count per running Loader goroutine
    closeDone chan struct{}   // closed when the first Close has finished joining
}
```

Invariants (hold whenever `mu` is not held):
- I1 `len(entries) == ll.Len() <= capacity`.
- I2 A key is never in both `entries` and `inflight`.
- I3 `closed == true` implies `entries` and `inflight` are empty, no new call is created,
  and nothing is ever stored again.
- I4 Every `val` reachable from `entries` or from a resolved `call` is cache-owned and never
  written after it is published. That is why copying it out after releasing `mu` is race-free.
- I5 `loaders.Add(1)` runs only under `mu` with `closed == false`. Close sets `closed` under `mu`
  before it calls `loaders.Wait()`, so no `Add` can race with the `Wait` (the WaitGroup rule).

`mu` is never held while calling the Loader, while blocking on a channel, or while copying a
large slice. No method acquires any other lock, so there is no lock ordering to get wrong.

## 3. Algorithms

### New
Validate arguments (panic otherwise). Then `ctx, cancel := context.WithCancel(context.Background())`,
initialise the maps, list and channels. No background goroutine is started. This matters
for `synctest`: a long-lived goroutine would keep the bubble from exiting.

### Get(ctx, key)
```
mu.Lock()
if closed { mu.Unlock(); return nil, ErrClosed }                  // rule 8: no Loader call
if el, ok := entries[key]; ok {                                    // rule 1 hit
    ll.MoveToFront(el); v := el.Value.(*entry).val
    mu.Unlock(); return clone(v), nil                              // rules 4, 6
}
cl, ok := inflight[key]
if !ok {                                                           // first miss: start the load
    cl = &call{done: make(chan struct{})}
    inflight[key] = cl
    loaders.Add(1)                                                 // I5
    go c.run(key, cl)
}
mu.Unlock()
return c.wait(ctx, cl)
```

### wait(ctx, cl): implements the rule 7 priority
```
select { case <-closedCh: case <-ctx.Done(): case <-cl.done: }
// Decide by priority, not by which select case won (select picks randomly among ready cases):
if isClosed(closedCh)  { return nil, ErrClosed }                   // (a)
if isClosed(cl.done)   { if cl.err != nil { return nil, cl.err }   // (c) resolved
                         return clone(cl.val), nil }
return nil, ctx.Err()                                              // (b) still in flight
```
`isClosed` is a non-blocking `select { case <-ch: return true; default: return false }`.
"Resolved" is checked before `ctx.Err()`. Rule 7(b) applies only while the load is still in flight,
so a waiter whose load has already resolved gets the result even if its ctx is also Done.
A waiter that leaves through (b) does not affect the load (rule 7, orphaned loads).

### run(key, cl): the Loader goroutine (exactly one per in-flight key)
```
defer loaders.Done()
v, err := load(c.ctx, key)              // cache-scoped ctx, never a caller's (rule 7)
var own []byte
if err == nil { own = clone(v) }        // copy outside the lock; Loader's slice not retained (rule 4)
                                        // (bytes, err) -> error wins, bytes dropped (rule 3)
mu.Lock()
delete(inflight, key)  // no-op if Close already cleared the map
switch {
case closed || c.ctx.Err() != nil:      // returned after cancel / in close window -> discard (rule 8)
    cl.err = ErrClosed
case err != nil:                        // rule 3: not stored; next Get re-loads
    cl.err = err
default:                                // rules 2, 5, 6
    c.storeLocked(key, own); cl.val = own
}
close(cl.done)                          // publishes val/err to waiters (happens-before)
mu.Unlock()
```
- Waiters that remain after a discard get `ErrClosed` from `closedCh` anyway. `cl.err = ErrClosed`
  is a second safeguard.
- Removing the key from `inflight` and inserting it into `entries` happen in one critical section,
  so no Get can see the key as both absent and not in flight (this keeps single-flight exact).
- `closed` is checked as well as `ctx.Err()`. That covers the short window between `closed = true`
  and `cancel()`. Results from that window are also discarded, which keeps "drops all entries" true.
- Loader panics are not recovered. The deferred `loaders.Done()` still runs and the program crashes
  as Go normally does. Turning a panic into a cached error would hide bugs.

### storeLocked(key, own): rule 5 (capacity / LRU), with mu held
```
if el, ok := entries[key]; ok { el.Value.(*entry).val = own; ll.MoveToFront(el); return } // defensive; unreachable by I2
if ll.Len() >= capacity {                 // evict BEFORE insert -> never above capacity
    back := ll.Back(); ll.Remove(back); delete(entries, back.Value.(*entry).key)
}
entries[key] = ll.PushFront(&entry{key, own})   // successful load = MRU (rule 6)
```
In-flight loads live only in `inflight`, so they never count toward capacity and can never be
evicted. A load becomes resident (and evictable) only here.

### Len()
`mu.Lock(); n := ll.Len(); mu.Unlock(); return n`. Eviction and insertion happen in the same
critical section, so a concurrent poll never sees more than `capacity` entries.

### Close()
```
mu.Lock()
if closed { mu.Unlock(); <-closeDone; return nil }   // idempotent; a concurrent second Close joins the first
closed = true
close(closedCh)                  // wake every current waiter -> ErrClosed (rule 7a / 8)
cancel()                         // cancel cache-scoped ctx -> Loaders must return promptly
entries = map{}; ll.Init()       // drop all entries (Len()==0 immediately)
inflight = map{}                 // forget unresolved calls; their run() will discard
mu.Unlock()
loaders.Wait()                   // wait for every in-flight Loader call to return
close(closeDone)
return nil
```
- Calling `cancel()` while holding `mu` is safe. It only runs the context's own propagation and
  takes no cache lock. It could also be moved after `Unlock`.
- A Get that takes `mu` after `closed = true` returns `ErrClosed` without calling the Loader.
  A Get that took `mu` earlier is either a hit that already returned, or a waiter that `closedCh`
  wakes.
- Waiters are woken immediately, before the join. They do not wait for slow Loaders.
- Deadlock freedom: Close holds `mu` only for O(1)/O(n) bookkeeping. `loaders.Wait()` runs without
  `mu`, and every `run` needs `mu` only briefly to finish. Each Loader is required to return once
  `ctx` is Done, so `Wait` terminates.
- Documented caller contract (spec-consistent): a Loader must not call `Close` on its own cache,
  because it would wait for itself. A Loader must not `Get` its own key either: that is
  self-single-flight, the usual singleflight caveat. A Loader may `Get` other keys, since `mu` is
  not held during Loader calls.

### clone(v)
`out := make([]byte, len(v)); copy(out, v); return out`. On success the result is always a fresh,
non-nil slice (a successful empty block is `[]byte{}`). On error the result is `nil` (the public
test requires `nil` bytes on error).

## 4. Rule-by-rule satisfaction

| Rule | Where satisfied |
|---|---|
| 1 Hit | Get hit branch: `MoveToFront`, `clone`, no Loader call. |
| 2 Single-flight | `inflight` map checked and filled under `mu`. Only the creator starts `run`. Joining waiters share `cl`. In-flight → resident handoff is atomic. Every waiter gets its own `clone(cl.val)`. |
| 3 Error | `run`: an error is stored on `cl` only and never reaches `storeLocked`. The key leaves `inflight`, so the next Get re-loads. `(bytes, err)`: `own` stays nil, so the error wins. |
| 4 Ownership | `run` clones the Loader's slice before storing it. Every Get return (hit or waiter) is a new `clone`. Stored slices are immutable (I4). |
| 5 Capacity/LRU | `storeLocked` evicts `ll.Back()` before pushing, in the same critical section. In-flight calls are outside `ll`/`entries`. `Len` reads `ll.Len()` under `mu`. |
| 6 Recency | `MoveToFront` on hit; `PushFront` on successful load. |
| 7 Ctx/orphans | Loader gets `c.ctx`. `run` is independent of waiters and stores on success even with zero waiters. `wait` applies priority (a) closed > (c) resolved-before-check > (b) ctx.Err while still in flight. |
| 8 Close | `closed` + `closedCh` + `cancel` + clear maps under `mu`, then `loaders.Wait()`, then `closeDone`. Second Close waits on `closeDone`. `run` discards results when `closed`/ctx cancelled. Get checks `closed` first. |
| 9 Concurrency | All shared state is under one mutex. Results are published through channel close. Stored bytes are immutable. No lock is held across blocking. WaitGroup Add/Wait are ordered (I5). |

## 5. Edge cases considered
- Get with an already-Done ctx on a resident key: returns the hit. Rule 7(b) applies only to waiting.
- Get with an already-Done ctx on a miss: registers or joins the load, which keeps running and
  populating (rule 7). The caller gets `ctx.Err()` unless the load has already resolved by the
  priority check.
- The Loader returns `(nil, nil)`: stored as an empty block. Gets return `[]byte{}` with a nil error.
- Capacity 1: every new admission evicts the single resident entry.
- Close racing with a completing load: whichever takes `mu` first wins. If `run` goes first, the entry
  is stored, then cleared by Close. If Close goes first, `run` discards. Either way no entry survives
  Close, and waiters get `ErrClosed` through `closedCh`.
- Close inside a `synctest` bubble (public `t.Cleanup`): the cache has no timers and no persistent
  goroutines, so the bubble can drain.

## 6. Test approach
Commands: `go vet ./...`, `go test ./...`, `go test -race -count=20 ./...` (repeated runs to
shake out flakiness), and `gofmt -l .` must print nothing.

1. **Public suite**, unchanged: copy semantics, SingleFlight (synctest), ErrorNotCached, LRUEviction,
   LenBounded, ClosedReturnsErrClosed, NewPanics.
2. **Internal tests** (`blockcache_internal_test.go`). Overlap is made deterministic with
   `synctest.Test` + `synctest.Wait`, and every wait outside a bubble is bounded by a 5s ctx.
   - *ErrorWinsOverBytes*: the Loader returns `([]byte("x"), err)`. The Get gets `err` and nil bytes,
     `Len()==0`, and the next Get reloads.
   - *SingleFlightError*: N blocked waiters, the Loader errors, and all N get the same error.
   - *PerWaiterCopies*: two waiters on one load. Mutating one result changes neither the other result
     nor a later hit. The Loader then mutates its own returned slice after return, and the cache is
     unaffected.
   - *LoaderGetsCacheCtx*: the Loader's ctx is not the caller's (cancel the caller, check the Loader's
     ctx is not Done). Close then cancels it.
   - *CallerCancelOrphanedLoad*: a blocked Loader. Cancel the only waiter's ctx, and the Get returns
     `context.Canceled`. Release the Loader, then the next Get is a hit (Loader count is 1).
   - *ClosePriorityOverCtx*: a waiter with a cancellable ctx and a blocked Loader. Close (Loader
     honours ctx) gives `ErrClosed`, not ctx error.
   - *CloseWakesWaitersAndJoinsLoader*: the Loader ignores ctx for a while (blocks on `release`). Start
     Close in a goroutine and `synctest.Wait()`: waiters already returned `ErrClosed`, Close has not
     returned yet. Release, and Close returns nil. The Loader's result is discarded (`Len()==0`).
   - *ConcurrentSecondClose*: while the first Close is joining a blocked Loader, a second Close is
     also blocked (`synctest.Wait` shows neither has returned). Release, and both return nil.
   - *GetAfterCloseNoLoad*: the Loader call count stays 0, and `errors.Is(err, ErrClosed)`.
   - *InflightNotCountedNotEvicted*: capacity 1, key `a` resident, `b` loading (blocked). `Len()==1`.
     A hit on `a` still works. Releasing `b` evicts `a`.
   - *LenNeverAboveCapacityUnderPoll* (with -race): many goroutines Get distinct keys while a poller
     asserts `Len() <= capacity` in a loop.
   - *ConcurrentStress*: random keys/hits/errors/caller cancels, then Close mid-flight. It checks
     there is no race, no deadlock (5s deadline), and every Get result is either valid bytes or one
     of {loader err, ctx err, ErrClosed}.
