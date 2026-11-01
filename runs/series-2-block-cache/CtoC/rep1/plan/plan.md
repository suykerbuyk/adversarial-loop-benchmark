# Implementation plan: `blockcache` (round 1)

No `plan_findings.md` exists yet, so this is the first plan and there is no rebuttal file.

## 0. Deliverable layout

- `worktree/blockcache.go`: the whole package (about 200 lines). It uses only the standard
  library: `context`, `errors`, `fmt`, `sync`, `container/list`.
- `worktree/blockcache_internal_test.go`: the extra tests from §5. The public test file stays as
  it is.
- Module `blockcache`, `go 1.25` (this is already in `go.mod`; `testing/synctest` needs 1.25).

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

`New` returns `*cache`, which is unexported, as a `Cache`. There are no other exported
identifiers.

## 2. Internal data model

```go
type cache struct {
    capacity int
    load     Loader

    ctx    context.Context    // cache-scoped; every Loader call gets this
    cancel context.CancelFunc

    mu       sync.Mutex        // guards every field below
    ll       *list.List        // resident entries; Front = MRU, Back = LRU; Value is *entry
    items    map[string]*list.Element
    inflight map[string]*call  // unresolved loads; NOT counted in capacity, never evictable
    closed   bool              // set once, under mu, at the same time as cancel()

    closedCh  chan struct{}    // closed (under mu) when Close begins; wakes waiters with ErrClosed
    closeDone chan struct{}    // closed when the first Close has finished joining loaders
    loads     sync.WaitGroup   // one count per running Loader goroutine
}

type entry struct {
    key string
    val []byte                 // cache-owned copy; never mutated after creation
}

type call struct {
    done chan struct{}         // closed exactly once when the load resolves
    val  []byte                // cache-owned copy (nil on error); written before done is closed
    err  error                 // written before done is closed
}
```

Invariants (all fields are guarded by `mu`):

- **I1.** `ll.Len() == len(items) <= capacity` at the end of every critical section.
- **I2.** A key is never in `items` and `inflight` at once. A miss is the only thing that creates
  an inflight entry, and only the completion of that same call writes to `items`. There is also a
  defensive branch: if completion finds the key already resident, it overwrites the value and
  moves the entry to the front instead of inserting a second copy.
- **I3.** `closed == true` ⇔ `cancel()` has been called ⇔ `closedCh` is closed. All three
  change together in one critical section.
- **I4.** `loads.Add(1)` is called only while holding `mu` and only when `!closed`. This means no
  `Add` can happen concurrently with or after `Close`'s `loads.Wait()`, so the WaitGroup is used
  correctly and the race detector stays clean.
- **I5.** `call.val` and `call.err` are written once, before `close(done)`. Waiters read them only
  after seeing `done` closed. This is a happens-before edge, so there is no data race.
- **I6.** No byte slice handed to the caller or received from the Loader is ever aliased by
  `entry.val` or `call.val`.

## 3. Algorithms

### `New(capacity, load)`
Panic with a descriptive message if `capacity < 1` or `load == nil`. Create
`ctx, cancel := context.WithCancel(context.Background())`, the maps, the list, and the two
channels.

### `Get(ctx, key)`
```
mu.Lock()
if closed            { mu.Unlock(); return nil, ErrClosed }            // rule 8: no Loader call
if el, ok := items[key] { ll.MoveToFront(el); out := clone(el.val); mu.Unlock(); return out, nil } // rules 1, 6
cl, ok := inflight[key]
if !ok {                                                                   // rule 2: start one load
    cl = &call{done: make(chan struct{})}
    inflight[key] = cl
    loads.Add(1)                                                           // I4
    go c.run(key, cl)
}
mu.Unlock()
return c.wait(ctx, cl)
```
The copy on a hit is made under `mu`. That is cheap and keeps the code simple. It would also be
correct outside the lock, because `entry.val` is immutable.

A caller whose ctx is already Done still joins or starts the load. It then gets `ctx.Err()` from
`wait` (unless the load has already resolved). This keeps a single code path and matches rule 7:
the orphaned load goes on and populates the cache. A hit is returned even if ctx is Done, because
rule 7's priority list applies only to a *waiting* Get.

### `wait(ctx, cl)`: rule 7's priority, implemented exactly
```
select {
case <-c.closedCh:  return nil, ErrClosed                                  // (a)
case <-cl.done:
case <-ctx.Done():
    if isClosed(c.closedCh) { return nil, ErrClosed }                      // (a) beats (b)
    if !isDone(cl.done)     { return nil, ctx.Err() }                      // (b): load still in flight
    // otherwise the load already resolved, so fall through to (c)
}
if isClosed(c.closedCh) { return nil, ErrClosed }                          // (a) beats (c)
if cl.err != nil        { return nil, cl.err }                             // rule 3
return clone(cl.val), nil                                                  // rule 4: own copy per waiter
```
`isClosed` and `isDone` are non-blocking `select`/`default` probes. Go's `select` picks randomly
among ready cases, so the re-checks after wake-up are what enforce the priority order, whichever
case actually fired.

### `run(key, cl)`: the Loader goroutine
```
defer loads.Done()
val, err := c.safeLoad(key)           // calls c.load(c.ctx, key); recovers a panic into an error
mu.Lock()
delete(inflight, key)
switch {
case closed:     cl.err = ErrClosed   // rule 8: discarded, not stored (zero-waiter and close-window cases)
case err != nil: cl.err = err         // rule 3: error wins; bytes discarded; key not stored
default:
    own := clone(val)                 // rule 4: never retain the Loader's slice
    cl.val = own
    c.admit(key, own)                 // rules 5, 6
}
close(cl.done)
mu.Unlock()
```
The cache is closed if and only if the cache context is cancelled (I3). So checking `closed`
under `mu` is the same test as "the Loader returned after the cache-scoped context was
cancelled", and there is no window where a result could slip in after `Close` has dropped the
entries.

`safeLoad` uses `defer`/`recover` to turn a Loader panic into
`fmt.Errorf("blockcache: loader panic for key %q: %v", key, r)`. Without this, a panicking Loader
would skip `loads.Done()` and the inflight cleanup. The process would usually crash anyway, but
with a recover in place the cache stays consistent and waiters are never stranded. This is a
documented choice; the spec does not cover panics.

### `admit(key, val)` (called with `mu` held)
If the key is already resident (defensive branch, see I2), replace `val` and `MoveToFront`.
Otherwise, if `ll.Len() >= capacity`, remove `ll.Back()` from both the list and the map
(LRU eviction), then `PushFront` the new entry and record it in `items`. Eviction and insertion
happen in the same critical section that `Len` reads under, so `Len()` can never go above
capacity, even if another goroutine is polling it.

### `Len()`
`mu.Lock(); n := ll.Len(); mu.Unlock(); return n`. Only resident entries count; the inflight map is
ignored.

### `Close()`
```
mu.Lock()
if closed { mu.Unlock(); <-closeDone; return nil }     // idempotent; a 2nd Close waits for the same join
closed = true
cancel()                                                // cancel cache-scoped ctx
close(closedCh)                                         // wake every current waiter with ErrClosed
items = map[string]*list.Element{}; ll.Init()           // drop all entries
mu.Unlock()
loads.Wait()                                            // wait for every in-flight Loader call to return
close(closeDone)
return nil
```
- Waiters are woken at the start of `Close`, not after the join. Rule 7(a) only requires that
  they get `ErrClosed`, and waking them early means a slow Loader cannot hold them up. `Close`
  itself still does not return until every Loader call has returned.
- Entries are dropped in the same critical section that sets `closed`. No store can happen after
  that point (see `run`), so the cache stays empty and `Len()` reports 0 from then on.
- The join happens outside `mu`. Finishing Loader goroutines must take `mu` in `run`, so holding
  it during the join would deadlock.
- The inflight map is left alone. Each `run` removes its own entry, and nobody can look up the
  map once `closed` is set.

Each Loader goroutine waits only on its Loader and then on a short `mu` section. `Close` waits on
`loads` without holding `mu`. Waiters block only in a `select` that includes `closedCh` and their
own ctx. So no cycle of waits exists, as long as the Loader respects the contract and returns
promptly once its ctx is Done.

Unsupported re-entrancy, documented in the doc comment: a Loader that calls `Close` on its own
cache would wait on itself. A Loader that calls `Get` for its own key would wait on its own call,
until its caller's ctx or `Close` ends that wait. These are contract violations, in the same class
as "a Loader MUST return promptly".

### `clone(b)`
`out := make([]byte, len(b)); copy(out, b); return out`. Every successful Get gets a distinct,
non-nil slice. A zero-length success gives `[]byte{}`. Error results are always `nil` bytes; the
public `TestErrorNotCached` asserts `b == nil`.

## 4. Rule-by-rule satisfaction

| Rule | Where it is satisfied |
|---|---|
| 1 Hit | Get: closed check → `items` hit → `MoveToFront` + `clone`; no Loader |
| 2 Single-flight | `inflight` map checked/inserted under `mu`; one `run` per call; all waiters share `cl` |
| 3 Error | `run`: `err != nil` → not admitted, `cl.val` nil, `inflight` deleted so next Get reloads; error wins over bytes |
| 4 Copy | `clone` on admit (Loader slice not retained) and `clone` per Get, for both hits and waiters |
| 5 Capacity/LRU | `admit` evicts `Back` before `PushFront` in one critical section; inflight is in a separate map, so it is neither counted nor evictable |
| 6 Recency | `MoveToFront` on hit; `PushFront` on successful load |
| 7 Ctx/orphans | Loader gets `c.ctx`; `run` does not depend on waiters; `wait` enforces the (a)>(b)>(c) priority with re-checks |
| 8 Close | `closed` + `cancel` + `closedCh` + drop in one section; `loads.Wait`; `closeDone` for idempotent joins; `run` discards results when `closed`; Get rejects new calls before any Loader call |
| 9 Concurrency | All shared state is under `mu`; channel happens-before edges for `call` fields; WaitGroup Add only under `mu` while open (I4); no lock is held across a blocking wait |

## 5. Test approach

Run `go build ./...`, `go vet ./...`, `go test -race -count=1 ./...`, and also run with
`-count=20` to shake out flakes. The public suite must pass unchanged, and that includes
`TestSingleFlight` under synctest. Waiters block in `select` on channels, which synctest counts
as durably blocked. `New` is called inside the bubble, so its context and goroutines belong to
the bubble.

Extra internal tests. These are deterministic and use synctest or channels; there are no sleeps
used for correctness.

1. **Error wins:** the Loader returns `([]byte("x"), err)`. Get returns `(nil, err)`, `Len()==0`,
   and the next Get reloads.
2. **Loader slice not retained:** the Loader keeps its returned slice and mutates it after
   returning. A later hit still shows the original bytes.
3. **Distinct copies per waiter:** N waiters on one load each get a slice. Mutating one leaves the
   others and the cache unchanged.
4. **Caller cancel while in flight:** the Loader blocks; the waiter's ctx is cancelled and the
   waiter gets `context.Canceled`. Then release the Loader. The orphaned load populates the
   cache: `Len()==1` and the next Get hits with no second Loader call.
5. **Resolved beats ctx (c over b):** under synctest, resolve the load, then cancel the ctx before
   the waiter is scheduled. The waiter gets the value. Best-effort check of the re-check path.
6. **Close wakes waiters:** waiters blocked on a load all get `ErrClosed`. `Close` returns only
   after the Loader (which honors ctx) has returned (recorded with an atomic flag). `Len()==0`
   afterwards.
7. **Close-window discard:** the Loader ignores cancellation until it is released, then returns
   success. Close blocks until release. Afterwards `Len()==0` and the value is not stored.
8. **Concurrent Close:** two Closes run while a Loader is blocked. Neither returns before the
   Loader returns, and both return nil.
9. **Get after Close:** returns `ErrClosed` and the Loader call count stays unchanged.
10. **Len bound under concurrent poll:** with capacity 2, 64 goroutines Get distinct keys while a
    poller asserts `Len() <= 2`, all under `-race`.
11. **LRU with a pending load:** with capacity 1, `a` is resident and `b` is in flight; a Get of
    `a` still hits (the pending load did not evict it). Once `b` resolves, `a` is evicted.
12. **Loader panic:** waiters get an error, nothing hangs, a later Get reloads, and Close returns.
13. **New panics:** negative capacity, in addition to the public cases.
14. **Stress under -race:** random Gets, Lens, and a final Close from many goroutines, with a
    Loader that honors ctx. Check that there is no deadlock (bounded by a 5s ctx) and no race
    report.
