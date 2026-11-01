# Implementation Plan: `blockcache` (bounded, concurrent, single-flight block cache)

Round 3. This revision responds to the round-2 `plan_findings.md`. The point-by-point response is in
`plan_rebuttal.md`. Changes from round 2 are marked **[R3]**, and round-2 changes keep their **[R2]** marks.
The round-2 adversary review found **no blocker or major issue**. Every R3 change addresses a minor or nit
finding: it closes a race window, hardens a code path, adds documentation, or tightens a test.

## 0. Layout and toolchain

- **Module.** `blockcache` (`worktree/go.mod`, `go 1.25`). `testing/synctest`'s `synctest.Test` is
  available (the public suite already uses it).
- **Package code.** `worktree/blockcache.go` holds the whole package: public API, cache type, LRU and load
  goroutine. It is about 220 lines, so one file stays readable.
- **Public tests.** `worktree/blockcache_public_test.go` is the shipped suite. It stays **byte-identical** to
  `instrument/resolve_public_test.go` (confirmed with `diff`).
- **Extra tests.** `worktree/blockcache_internal_test.go` holds the additional tests (§5).
- **Imports.** Standard library only: `bytes`, `container/list`, `context`, `errors`, `fmt`, `sync`.

## 1. Public API (exactly as specified)

```go
package blockcache

type Loader func(ctx context.Context, key string) ([]byte, error)

type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Len() int
	Close() error
}

func New(capacity int, load Loader) Cache   // panics if capacity < 1 || load == nil

var ErrClosed = errors.New("blockcache: closed")
```

**Constructor and errors**
- `New` returns the unexported `*cache` as a `Cache`.
- Panic messages: `"blockcache: capacity must be >= 1"` and `"blockcache: nil Loader"`.
- `ErrClosed` is returned unwrapped, so both `==` and `errors.Is` work.

**Doc comments.** Every exported identifier gets one that restates its contract. On top of the spec's
wording:
- **`Loader`:** the Loader receives a cache-scoped ctx. It must return promptly once that ctx is Done,
  otherwise Close blocks. A panic in the Loader is recovered and reported to that load's waiters as an error.
- **`Close`** **[R3]:** Close blocks until every in-flight Loader call has returned. A Loader that ignores
  ctx cancellation therefore blocks Close, including any concurrent second Close. Callers must call Close when
  they are done with the cache, because it releases the cache-scoped context and joins the load goroutines.
- **`Get`:** a hit returns the bytes even if ctx is already Done; ctx only bounds *waiting* on an in-flight
  load. A miss always starts, or joins, a load that runs under the cache ctx. That load completes and
  populates the cache even if this caller leaves.

## 2. Internal data model

```go
type cache struct {
	capacity int
	load     Loader

	ctx    context.Context    // cache-scoped; every Loader call receives it
	cancel context.CancelFunc // called ONLY by the first Close, inside its mu critical section

	mu       sync.Mutex               // guards everything below
	closed   bool                     // set once, by the first Close, under mu
	lru      *list.List               // resident entries; Front = MRU, Back = LRU; Value is *entry
	items    map[string]*list.Element // resident key -> element
	inflight map[string]*call         // keys with a load in flight (not resident, not counted)

	loads     sync.WaitGroup // one count per running load goroutine
	closing   chan struct{}  // closed by the first Close, under mu; wakes in-flight waiters
	closeDone chan struct{}  // closed by the first Close after join + drop
}

type entry struct {
	key string
	val []byte // cache-owned; never written after publication; may alias call.val (I5)
}

type call struct {
	done chan struct{} // closed exactly once, under mu, after val/err are set
	val  []byte        // cache-owned copy on success; read-only after done; same array as entry.val
	err  error         // Loader error, or ErrClosed if the result was discarded because of Close
}
```

Invariants (they hold whenever `mu` is free):
- **I1** `len(items) == lru.Len() <= capacity`.
- **I2** A key is never in both `items` and `inflight`. A load starts only when the key is in neither, and
  only that load's goroutine can make the key resident. Because this holds by construction, `admit` has no
  "already resident" branch.
- **I3** Each key in `inflight` has exactly one live load goroutine, and `loads` counts it.
- **I4** Once `closed` is true, nothing new enters `inflight`, `items` or `loads`.
- **I5** `entry.val` and `call.val` are never written after publication. On a successful load they **alias the
  same backing array** (one clone of the Loader's bytes). Readers only copy them, so copying can happen
  outside `mu`. **[R3]** In the code, this rule is stated in a comment at the single place where the two
  fields are assigned (`admit(key, own); cl.val = own`) and on the struct fields. Anyone who wants to mutate
  either field must first give each its own copy.
- **I6** If `c.ctx` is cancelled, then `c.closed` is true. The only `cancel()` call is in Close, in the same
  critical section that sets `closed`.
- **I7** **[R3]** `closing` is closed exactly when `closed` becomes true, in the same critical section, at the
  single site in Close. `closing` is just the channel form of `closed`, used by waiters that are blocked in
  `select`. A comment at that site says the two must change together.

**Unbounded in-flight loads, by design** **[R3]:** a code comment on `inflight` records this. The spec says
in-flight loads do not count toward capacity and cannot be evicted (rule 5), and that orphaned loads are not
leaks (rule 7). So the number of concurrent load goroutines is bounded by the number of distinct missing keys,
not by `capacity`. Maintainers must not "fix" this by capping it.

## 3. Algorithms

### New
1. Check the preconditions and panic if either fails.
2. Create the cache context: `ctx, cancel := context.WithCancel(context.Background())`.
3. Build the maps, the list and the two channels.

### Get(ctx, key)
```
c.mu.Lock()
if c.closed { c.mu.Unlock(); return nil, ErrClosed }     // rule 8: no Loader call
if el, ok := c.items[key]; ok {                          // rule 1: hit; caller ctx deliberately NOT consulted
    c.lru.MoveToFront(el)                                // rule 6
    v := el.Value.(*entry).val                           // grab the slice header under mu
    c.mu.Unlock()
    return bytes.Clone(v), nil                           // rule 4: fresh copy, made outside the lock (I5)
}
cl, ok := c.inflight[key]
if !ok {                                                 // rule 2: the first miss starts the only load
    cl = &call{done: make(chan struct{})}
    c.inflight[key] = cl
    c.loads.Add(1)                                       // under mu, after seeing closed == false
    go c.runLoad(key, cl)
}
c.mu.Unlock()
return c.wait(ctx, cl)
```
- **Hits ignore the caller's ctx.** Rule 7(b) applies only "while the load is still in flight", and rule 1
  places no ctx condition on a hit.
- **An already-cancelled ctx on a miss still starts or joins the load.** The load runs under the cache ctx and
  completes on its own (rule 7). This also means a cancelled caller cannot split single-flight or stop the
  value from being cached. `wait` then returns `ctx.Err()` right away, unless the load has already resolved.

### wait(ctx, cl): rule 7 priority **[R3: the decision is made atomically under mu]**
```
select {                       // block until something happens
case <-cl.done:
    return cl.result()         // (c) resolved: the load's outcome (or ErrClosed if Close discarded it)
case <-c.closing:
case <-ctx.Done():
}
// Woken by closing or by our own ctx. Decide under mu: runLoad closes cl.done under mu, and Close sets
// closed (and closes closing) under mu, so the snapshot below is consistent with both.
c.mu.Lock()
var resolved, closed bool
select { case <-cl.done: resolved = true; default: }
closed = c.closed
c.mu.Unlock()
switch {
case resolved:  return cl.result()         // (c) beats (b): the load is no longer in flight
case closed:    return nil, ErrClosed      // (a) beats (b)
default:        return nil, ctx.Err()      // (b) own ctx done while the load is genuinely still in flight
}
```
where `cl.result()` is `if cl.err != nil { return nil, cl.err }; return bytes.Clone(cl.val), nil`. That gives
each waiter its own copy (rules 2 and 4), made outside the lock.

**[R3] Why the change.** The round-2 version used two non-blocking selects one after the other. The adversary
noted that `done` could close after the `done` check but before `return ctx.Err()`. In that case a waiter
would report (b) even though (c) had already been published. With this version, at the moment `mu` is held:
- If `done` is open, runLoad has not yet published, because it closes `done` under `mu`.
- If `closed` is false, Close has not started, because it sets `closed` under `mu`.

The decision is therefore linearizable. Whatever happens after `Unlock` is ordered after this Get's decision.
`ctx.Err()` is non-nil whenever we reach the `default` branch: we only take the mu path after waking on
`closing` or `ctx.Done()`, and if we woke on `closing` then `closed` is already true.

**Maintainer warning, written as a comment in the code** **[R3]:** collapsing this into one `select` that
returns on whichever channel fires is **incorrect**. Go picks at random among ready cases, so:
- (b) could beat (a);
- (a) or (b) could beat an already-resolved (c).

The order of the `switch` *is* the rule-7 contract.

**How this satisfies rule 7 and rule 8:**
- **In flight when the cache closes.** `done` is open and `closed` is true, so the waiter gets `ErrClosed`
  (a), even if its own ctx is also done.
- **Load resolves after Close has started.** runLoad sees `closed` (I6 covers cancel too), discards the
  result, and publishes `cl.err = ErrClosed`. Any waiter that sees `done` still gets `ErrClosed`. So "wakes
  every current waiter with ErrClosed" holds whichever path the waiter takes.
- **Load resolved before Close.** The result was published while the cache was open, so the waiter gets (c).
  That Get linearizes before the Close.
- **Own ctx done, load already resolved.** The waiter gets (c), including a normal Loader error.
- **Leaving the wait has no effect on the load.** A waiter that leaves through (a) or (b) does not touch the
  load, which keeps running under `c.ctx` (orphaned loads, rule 7).
- **Visibility.** The writes to `cl.val` and `cl.err` happen before `close(cl.done)`, and that close
  happens-before every receive that observes it, including the non-blocking receive made under `mu`.
- **No deadlock.** `wait` holds no other lock while taking `mu`, and the critical section contains only a
  non-blocking receive and a bool read.

### runLoad(key, cl): the single Loader call
```
func (c *cache) runLoad(key string, cl *call) {
    defer c.loads.Done()                       // runs last, on every exit path
    b, err := c.callLoader(key)                // c.load(c.ctx, key); a panic becomes an error
    var own []byte
    if err == nil { own = bytes.Clone(b) }     // rule 4: copy outside the lock; don't keep the Loader's slice
                                               // rule 3: bytes that come with an error are dropped
    c.mu.Lock()
    defer c.mu.Unlock()
    defer close(cl.done)                       // runs before Unlock, on every exit path
    delete(c.inflight, key)
    switch {
    case c.closed:                             // rule 8: late result discarded; I6 covers "ctx cancelled"
        cl.err = ErrClosed
    case err != nil:                           // rule 3: nothing stored; the next Get re-loads
        cl.err = err
    default:                                   // rules 2, 5, 6: the value becomes resident as MRU
        c.admit(key, own)                      // I5: entry.val and cl.val alias `own`; neither is ever written
        cl.val = own
    }
}
```
- **Panic safety.** Deferring `close(cl.done)` and `Unlock` means even an impossible panic inside `admit`
  still wakes the waiters, releases the lock and decrements `loads`.
- **No admission after Close.** The switch admits only when `!c.closed`.
- **One discard condition.** `c.closed` is the only condition, so `ErrClosed` is published only for an actual
  Close. A Loader error seen while the cache is open (for example the Loader's own
  `context.DeadlineExceeded`) is published unchanged.
- **Loader ownership.** Once `c.load` returns, the cache treats the returned slice as handed over and clones
  it at once. A Loader that keeps writing to that slice from another goroutine after returning is breaking
  the ownership handoff in rule 4. The cache keeps nothing that references it.

`admit(key, val)` (called with mu held):
```
for c.lru.Len() >= c.capacity {               // make room: evict the LRU entry (rule 5)
    back := c.lru.Back()
    delete(c.items, back.Value.(*entry).key)
    c.lru.Remove(back)
}
c.items[key] = c.lru.PushFront(&entry{key: key, val: val})   // MRU (rule 6)
```
- **Len never overshoots.** Eviction and insertion happen in one critical section, so `Len()` never sees
  `capacity+1`. Since `capacity >= 1` (New panics otherwise), the loop always ends with a non-empty list
  being shrunk.
- **Pending loads are protected.** In-flight loads are not in `lru` or `items`, so they are not counted and
  cannot be evicted.
- **Recency follows completion order.** Concurrent loads of distinct keys are admitted in the order they
  *complete*, and each becomes MRU at that moment (rule 6: "a successful load marks MRU").

**Loader panics.** `callLoader` runs the Loader under a deferred `recover`:
- If the recovered value is an `error`, the result is `fmt.Errorf("blockcache: loader panicked: %w", r)`,
  so callers can still match it with `errors.Is` or `errors.As` **[R3]**.
- Otherwise the result is `fmt.Errorf("blockcache: loader panicked: %v", r)`.
- The key is never interpolated into the message.

After that the bookkeeping runs normally: waiters get the error, nothing is stored, and a later Get retries.
The spec doesn't define what happens on a panic. Recovering guarantees that a panic never turns into a hang.

**nil vs empty.** `bytes.Clone` keeps nil as nil and empty as empty. `(nil, nil)` is a successful resident
empty block.

### Len
`mu.Lock(); n := lru.Len(); mu.Unlock(); return n`. It counts resident entries only, and by I1 it is never
above capacity.

### Close **[R3: closeDone closed via defer]**
```
c.mu.Lock()
if c.closed {                 // second or later Close
    c.mu.Unlock()
    <-c.closeDone             // wait for the first Close's join + drop (rule 8 idempotency)
    return nil
}
c.closed = true               // I7: closed and closing change together, here only
c.cancel()                    // I6: same critical section as closed = true
close(c.closing)              // wake in-flight waiters -> ErrClosed
c.mu.Unlock()
defer close(c.closeDone)      // later Closes are released even if anything below panics
c.loads.Wait()                // every in-flight Loader call and its bookkeeping has finished
c.mu.Lock()
clear(c.items); c.lru.Init()  // drop all entries
c.mu.Unlock()
return nil
```
- **No race on `loads`.** `loads.Add(1)` runs only in Get, under `mu`, after Get has seen `!closed`. Close
  sets `closed` under the same `mu` before it calls `Wait`, so no `Add` can race with `Wait`.
- **No lock held during `Wait`.** runLoad needs `mu` to finish.
- **Nothing survives Close.** After `Wait`, `inflight` is empty. Every runLoad that finished after `closed`
  was set stored nothing, and the drop removes anything stored earlier. I4 stops new admissions, so
  `Len() == 0` holds from then on.
- **[R3] Len during Close.** A `Len()` that runs concurrently with Close can still report entries until
  the drop's critical section. Once Close has returned, every later `Len()` returns 0: the drop happens before
  `close(closeDone)` and before Close returns. Any `Len()` that sees an entry is ordered before the drop. The
  spec requires "drops all entries" as an effect of Close, which is visible once Close has returned. It never
  requires `Len()==0` while Close is still joining, and the cap is never exceeded.
- **[R3] Why `closeDone` is closed by defer.** If something between `Unlock` and the end panicked, later
  Closes would otherwise block forever. Nothing in that span can realistically panic, but this costs nothing.
- **Wake order.** Waiters are woken *before* the join finishes. They get `ErrClosed` either way, and they
  never depend on Loader latency. Close itself still returns only after every Loader call has returned.
- **The join is unbounded on purpose** (rule 8; see the round-2 rebuttal of finding 2, which the round-2
  adversary accepted). **[R3]** The resulting hazard is now documented on `Close` itself.

## 4. Rule-by-rule traceability

| Rule | How it is satisfied |
|---|---|
| 1 Hit | `items` lookup under mu, `MoveToFront`, then a clone made outside the lock. No Loader call. The caller's ctx is not consulted. |
| 2 Single-flight | The `inflight` check and insert happen in the same critical section as the residency check. Only the first miss creates a `call` and starts a goroutine; later misses wait on `cl.done`. Each waiter clones `cl.val` separately. |
| 3 Error | `err != nil` → `cl.err = err`. Nothing is admitted and the inflight entry is deleted, so the next Get re-loads. Bytes returned together with an error are never cloned. Waiters get `(nil, err)`. |
| 4 Ownership | runLoad clones the Loader's slice and never keeps it. Every hit and every waiter clones again. Stored slices are never mutated (I5). |
| 5 Capacity/LRU | `admit` evicts `Back()` until there is room, then inserts, all under mu. Pending loads live only in `inflight`. `Len` reads `lru.Len()` under mu. |
| 6 Recency | A hit calls `MoveToFront`. A successful load calls `PushFront` when it completes. |
| 7 Context | The Loader gets `c.ctx`, which derives from `context.Background`. A waiter's ctx never cancels the load. Orphaned loads still admit on success. `wait` decides under mu: resolved (c) > closed (a) > own ctx (b). |
| 8 Close | Under mu: set closed, cancel, wake. Then join with `loads.Wait`, drop all entries, close `closeDone`. Late results are discarded via `closed`, and their waiters get ErrClosed. Get checks closed before starting any Loader. A concurrent second Close blocks on `closeDone`. Close always returns nil. |
| 9 Concurrency | One mutex guards all mutable state, and channels publish results. No lock is held across a Loader call, a blocking channel wait, `loads.Wait`, or a copy. Checked with `-race`. |

## 5. Test approach

**Commands:**
- `gofmt -l .` (must print nothing)
- `go vet ./...`
- `go test -race -count=1 ./...`
- Stress pass: `go test -race -count=50 -run 'Stress|SingleFlight|Close' ./...`

### 1. Public suite
The suite is unchanged. All 7 tests must pass with and without `-race`.

### 2. Internal tests (`blockcache_internal_test.go`)
Overlap is made deterministic with `synctest.Test`. Every wait is bounded by a 5s ctx. Failures inside a
bubble use `t.Errorf` and then drain, so parked goroutines don't hide the cause.

**Copies and ownership**
- **Per-waiter copies.** N waiters share one load. Mutate result[0]. The other results and a later hit are
  unaffected, and the results have distinct backing arrays.
- **Loader slice not retained.** The Loader returns a slice that the test also holds. *After* `Get` returns
  (the copy has been taken, so no race), the test overwrites that slice. A later hit still returns the
  original bytes.
- **Bytes + error.** A Loader returning `([]byte("x"), err)` gives every waiter `(nil, err)` and leaves
  `Len()==0`. The next Get calls the Loader again.

**Hits** **[R3]**
- **Hit with an already-cancelled ctx.** `a` and `b` are resident (capacity 2). Get `a` with a cancelled ctx:
  it returns the bytes with nil error and no Loader call. Then load `c`: `b` is evicted and `a` survives. This
  proves the cancelled-ctx hit still marked `a` as MRU.

**Waiter context (rule 7)**
- **Waiter ctx cancelled while blocked.** The waiter gets `context.Canceled`. Release the Loader: `Len()==1`,
  and a later Get hits with no extra Loader call.
- **Already-cancelled ctx on a cold miss** **[R3].** The Loader blocks on a release channel. `Get` with a
  pre-cancelled ctx returns `context.Canceled` and the Loader was entered once. Release it: `Len()==1`, and a
  live Get hits.
- **All waiters leave.** Every waiter leaves before the load finishes. The load still completes and admits.
- **Loader gets the cache ctx.** Cancelling the caller's ctx does not cancel the Loader's ctx. The Loader's
  ctx is Done after Close.
- **Open-cache Loader error published as-is.** A Loader that returns `context.DeadlineExceeded` (or any
  error) while the cache is open: waiters get exactly that error, not `ErrClosed`.

**Rule-7 priority, white-box.** These call `c.wait` directly on hand-built states. `closing` is closed
together with `c.closed = true`, as in Close.
- (i) `done` closed with a value, cache closed, ctx cancelled → the value.
- (ii) `done` closed with `err=ErrClosed`, cache closed → ErrClosed.
- (iii) `done` open, cache closed, ctx cancelled → ErrClosed ((a) beats (b)).
- (iv) `done` open, cache open, ctx cancelled → `ctx.Err()`.
- (v) **[R3]** `done` closed with a normal Loader error, cache open, ctx cancelled → that Loader error ((c)
  beats (b), error variant).

**Close (rule 8)**
- **Resolved before Close, then Close.** The load resolves (`synctest.Wait`) and all waiters have their bytes;
  then Close. Afterwards `Len()==0`, and the earlier results are intact.
- **Close joins Loaders.** The Loader blocks on `<-ctx.Done()`, then sleeps (fake time). Close does not return
  before the Loader returns (checked with an atomic flag). Waiters get ErrClosed, and `Len()==0` once Close
  returns.
- **Close discards late success.** The Loader ignores cancel for a bounded fake-time sleep and then returns
  success. Nothing is stored, its waiters get ErrClosed, and `Len()==0`.
- **Concurrent second Close.** While the first Close is joining, a second Close stays blocked (checked with
  `synctest.Wait` and a flag). Both return nil once the Loader exits.
- **Get after Close on a previously resident key.** Returns ErrClosed, with no Loader call.

**Capacity and recency (rules 5 and 6)**
- **Pending load is neither counted nor evictable.** Capacity 1, `a` resident, `b` loading: `Len()==1` and
  `a` hits. After `b` resolves, `a` is evicted and `Len()==1`.
- **Completion-order recency** **[R3].** Capacity 2. Start loads `x` then `y` concurrently, each blocked on its
  own release channel. Release `y` first and then `x` (with `synctest.Wait` between). Now load `z`. `y`, which
  completed first and is therefore LRU, is evicted. `x` still hits and `y` reloads.

**Loader panic**
- The waiter gets an error that doesn't contain the key. When the panic value is an error, `errors.Is`
  matches it **[R3]**. The next Get retries, nothing hangs, and a later Close returns.

**Stress (real scheduler, -race)** **[R3: oracle tightened]**

Setup: 64 goroutines, a 32-key space, capacity 8, random sleeps, and an injected error that only fires while
an atomic `injectErrors` flag is on. A poller asserts `Len() <= 8` throughout.

Oracle:
- **Single-flight under the real scheduler.** A per-key atomic "loads in progress" counter is incremented
  when the Loader is entered and decremented when it returns. Any value above 1 fails the test (rule 2).
- **Values.** Every successful value must equal `"v:"+key`.
- **Outcomes.** Every Get returns one of: the correct value, the injected error, ErrClosed, or `ctx.Err()`
  (only when that Get's ctx was cancelled).
- **Errors not cached (rule 3).** Phase 2: turn `injectErrors` off and wait for all phase-1 goroutines. Then
  Get every key once with a live ctx. Each must succeed, because a cached error would come back here.
- **Close mid-run.** Phase 3: run a concurrent burst with Close called mid-burst. Afterwards `Len()==0`, every
  Get returns ErrClosed, and nothing deadlocks (5s deadline).

### 3. Review checklist
- Doc comments on all exports, including the Close blocking note and the must-call-Close note.
- The code comments required by I5, I7, the unbounded-inflight note, and the `wait` "do not collapse the
  select" warning.
- No lock held across a Loader call, a blocking channel wait, `Wait`, or a copy.
- `go vet` clean, `gofmt` clean.
- Public test file byte-identical to the shipped one.
