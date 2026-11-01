# Implementation Plan — `blockcache` (bounded, concurrent, single-flight block cache)

Status: **round 3**. The round-2 review (`plan_findings.md`) reports no blocker or major issues and
confirms the round-2 waiter-settlement fix. The design is therefore unchanged; this round only
re-verifies every rule against the spec and public suite and makes two previously implicit choices
explicit (§4.4 wake-before-join ordering; §7). History: round 1 → one `major` finding
(post-resolution caller-ctx cancellation could override the load result), fixed in round 2 by a
per-waiter outcome *settled exactly once, under the cache mutex, at the first linearizing event*
(load resolution, Close, or the waiter observing its own ctx while the flight is still unresolved).
See §4.1–§4.4 and `plan_rebuttal.md`.

Target: module `blockcache` (`go 1.25`, see `worktree/go.mod`), package `blockcache`. The public
suite uses `testing/synctest` (GA in 1.25) and is graded under `-race`.

---

## 1. Files

| File | Contents |
|---|---|
| `blockcache.go` | Public API (`Loader`, `Cache`, `New`, `ErrClosed`) + unexported `cache` implementation (~200 lines). |
| `blockcache_public_test.go` | Shipped public suite, unchanged. |
| `blockcache_test.go` | Additional tests (§6). |

Stdlib only: `bytes`, `container/list`, `context`, `errors`, `fmt`, `sync`.

## 2. Public API (exactly as specified)

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

`New` panics with descriptive messages (`"blockcache: capacity must be >= 1, got %d"`,
`"blockcache: nil Loader"`), creates the cache-scoped context with
`context.WithCancel(context.Background())`, and returns `*cache` as `Cache`. Every exported
identifier gets a doc comment restating the contract.

## 3. Internal data model

```go
type entry struct {
    key string
    val []byte // cache-owned; never mutated after store; never handed out directly
}

// waiter is one Get blocked on a flight. Its outcome is settled exactly once, under c.mu.
type waiter struct {
    ctx     context.Context // the caller's ctx (read via ctx.Err() at settle time)
    settled bool            // guarded by c.mu
    val     []byte          // guarded by c.mu; shared immutable flight value (cloned by the waiter)
    err     error           // guarded by c.mu
}

// flight is one in-flight Loader call.
type flight struct {
    done    chan struct{}         // closed exactly once, after the Loader returned and waiters were settled
    waiters map[*waiter]struct{}  // guarded by c.mu; unsettled waiters only
}

type cache struct {
    capacity int
    load     Loader
    ctx      context.Context    // cache-scoped; passed to every Loader call
    cancel   context.CancelFunc

    closing    chan struct{}    // closed by the first Close (under mu): wakes every waiter
    closedDone chan struct{}    // closed at the end of the first Close: joins later Closes

    mu       sync.Mutex                 // guards everything below + waiter.settled/val/err + flight.waiters
    closed   bool
    lru      *list.List                 // front = MRU; elements hold *entry; resident only
    entries  map[string]*list.Element   // resident only
    inflight map[string]*flight         // unresolved loads only
}
```

Invariants (all under `mu`):
- **I1.** A key is never in both `entries` and `inflight` (a flight is created only on a miss; the
  entry is created only by that flight's resolution, which removes it from `inflight` in the same
  critical section).
- **I2.** `lru.Len() == len(entries) <= capacity`.
- **I3.** Once `closed` is true no flight is created and no entry is stored.
- **I4.** Every flight in `inflight` has exactly one goroutine that will settle its waiters and close
  `done`.
- **I5.** Stored `val` slices are never mutated and never returned; callers only get clones.
- **I6 (new).** A waiter's outcome is written once (`settled` false→true) under `mu`, by exactly one
  of: the flight's resolution (§4.2), Close (§4.4), or the waiter itself when it observes its ctx
  Done while still unsettled (§4.1). Whoever settles first wins; nobody overrides a settled waiter.

No background goroutines beyond one per in-flight load, each exiting when its Loader returns
(keeps synctest bubbles clean).

## 4. Algorithms

### 4.1 `Get(ctx, key)`
```
mu.Lock()
if closed             -> Unlock; return nil, ErrClosed         // rule 8: no Loader call
if el := entries[key] -> lru.MoveToFront(el); v := el.val; Unlock
                         return bytes.Clone(v), nil            // rules 1, 4, 6
f := inflight[key]
if f == nil {                                                  // rule 2: start exactly one flight
    f = &flight{done: make(chan struct{}), waiters: map[*waiter]struct{}{}}
    inflight[key] = f
    go c.run(key, f)
}
w := &waiter{ctx: ctx}
f.waiters[w] = struct{}{}                                      // join
mu.Unlock()

select { case <-f.done: case <-c.closing: case <-ctx.Done(): } // wake-up only; NOT the decision

mu.Lock()
if !w.settled {
    // Only reachable via ctx.Done(): resolution and Close both settle every registered waiter
    // under mu before closing their channel. So the flight is still unresolved and the cache is
    // not closed => rule 7(b).
    delete(f.waiters, w)
    w.settled, w.err = true, ctx.Err()
}
val, err := w.val, w.err
mu.Unlock()
if err != nil { return nil, err }
return bytes.Clone(val), nil                                   // rule 4: own copy per waiter
```
Why this fixes the finding: the decision is no longer "whatever is true when the goroutine happens
to be scheduled". It is fixed at the first linearizing event under `mu`:
- Flight resolved first (ctx cancelled afterwards, even before this goroutine ran) ⇒ `run` settled
  `w` with the load result while `ctx.Err()` was still nil ⇒ Get returns the result (7c). A later
  cancellation cannot override it.
- Caller ctx cancelled while the load is in flight ⇒ either the waiter wakes and settles itself with
  `ctx.Err()` before resolution, or — if resolution wins the race to `mu` — `run` observes
  `w.ctx.Err() != nil` at settle time and settles it with `ctx.Err()` (§4.2). Either way 7(b). This
  also covers "cancel, then release Loader" sequences where the waiter has not yet been scheduled.
- Cache closed while waiting ⇒ Close settled it with `ErrClosed` (7a), regardless of ctx state.

Other notes:
- Reading `w.val/w.err` is under `mu` (no reliance on channel happens-before; trivially race-free).
- Cloning outside the lock is safe: `w.val` is the immutable cache-owned flight value (I5).
- Deregistering a self-settled waiter keeps `f.waiters` bounded when many callers abandon a long load.
- Miss with an already-Done ctx still starts/joins the flight; the select returns immediately, the
  waiter self-settles with `ctx.Err()`, and the orphaned load still populates the cache (rule 7).
- Hit with a Done ctx returns the data (no waiting; rule 7 governs waiting Gets only).

### 4.2 `run(key, f)` — the single Loader call for a flight
```
b, err := c.callLoader(key)        // c.load(c.ctx, key); recover() turns a panic into an error
var val []byte
if err == nil { val = bytes.Clone(b); if val == nil { val = []byte{} } }  // rule 4; rule 3 discards b on err

mu.Lock()
delete(inflight, key)
if closed {
    // rule 8: result after cache ctx cancelled is discarded, never stored.
    // All waiters were already settled with ErrClosed by Close; f.waiters is empty.
} else {
    if err == nil { c.admit(key, val) }                       // rules 2, 5, 6; err: not stored (rule 3)
    for w := range f.waiters {
        w.settled = true
        if cerr := w.ctx.Err(); cerr != nil {                  // ctx Done before resolution: rule 7(b)
            w.err = cerr
        } else if err != nil {                                 // rule 3: every waiter gets the error
            w.err = err
        } else {
            w.val = val                                        // shared immutable; each waiter clones
        }
    }
    clear(f.waiters)
}
mu.Unlock()
close(f.done)
```
- Everything from `mu.Lock()` through `close(f.done)` is the helper `c.resolve(key, f, val, err)`;
  §4.1 from `select` onward is `c.await(ctx, f, w)` (factored for the white-box tests in §6).
- Loader always gets `c.ctx`, never a caller ctx (rule 7).
- Clone of the Loader bytes happens outside the lock.
- `closed` is set under `mu` *before* `cancel()` (§4.4), so any Loader returning after cancellation
  sees `closed == true` and is discarded — covers zero-waiter and close-window loads.
- `ctx.Err()` under `mu` is safe: it only takes the context's internal lock and never calls back
  into the cache (no lock-order cycle).
- Panic recovery (`fmt.Errorf("blockcache: loader panicked: %v", r)`) guarantees `done` is always
  closed and the flight removed (defensive, rule 9).

### 4.3 `admit(key, val)` (holds `mu`)
```
if el, ok := entries[key]; ok { el.Value.(*entry).val = val; lru.MoveToFront(el); return } // defensive; unreachable by I1
if lru.Len() >= capacity { back := lru.Back(); lru.Remove(back); delete(entries, back.Value.(*entry).key) }
entries[key] = lru.PushFront(&entry{key: key, val: val})
```
Evict + insert in one critical section ⇒ `Len()` never observes `capacity+1` (rule 5). Flights
live only in `inflight` ⇒ not counted, not evictable (rule 5). New entry at front = MRU (rule 6).

### 4.4 `Close()`
```
mu.Lock()
if closed { d := closedDone; mu.Unlock(); <-d; return nil }   // idempotent; 2nd Close joins the 1st
closed = true
flights := make([]*flight, 0, len(inflight))
for _, f := range inflight {
    for w := range f.waiters { w.settled, w.err = true, ErrClosed }   // rule 7(a)/8: every current waiter
    clear(f.waiters)
    flights = append(flights, f)
}
close(closing)                       // wake all waiters (already settled)
mu.Unlock()
cancel()                             // cancel cache-scoped ctx
for _, f := range flights { <-f.done }   // wait for every in-flight Loader call to return
mu.Lock(); clear(entries); lru.Init(); mu.Unlock()   // drop all entries
close(closedDone)
return nil
```
- Snapshot is complete: after `closed = true` no flight can be created (Get checks under the same
  `mu`).
- A waiter already settled by a resolution before Close is not a "current waiter" (its outcome was
  fixed); it returns its load result. Waiters settled by Close get `ErrClosed` even if their ctx is
  also Done (priority (a) > (b)).
- All blocking is on channels (durable under synctest); no lock held while blocking or calling the
  Loader; single mutex ⇒ no deadlock.
- `Len()` after Close is 0.
- Ordering vs. the spec's prose ("cancels, waits, wakes, drops"): waiters' outcomes are *fixed* as
  `ErrClosed` in the first critical section and they are woken immediately rather than after the
  join. Every observable guarantee holds — every current waiter receives `ErrClosed`, no later than
  Close returns, and no Loader result is stored — while waiters are not held hostage to a slow
  Loader. Entries are dropped after the join; nothing can be admitted in between (I3), so the
  final state is identical either way.

### 4.5 `Len()`
`mu.Lock(); n := lru.Len(); mu.Unlock(); return n` — resident only, ≤ capacity by I2.

## 5. Rule-by-rule traceability

| Rule | Satisfied by |
|---|---|
| 1 Hit | §4.1 hit branch: `MoveToFront`, `bytes.Clone`, no Loader call. |
| 2 Single-flight | `inflight` map under `mu`: first miss creates flight + goroutine; others join. All settled from the same result. Success → `admit`. |
| 3 Error | §4.2: err ≠ nil ⇒ bytes discarded, nothing stored, flight deleted ⇒ next Get reloads; every unsettled waiter (ctx live) gets err, `nil` bytes. |
| 4 Ownership/copy | Loader bytes cloned into cache storage; every return path clones; stored slices never escape. |
| 5 Capacity/LRU | `admit` evicts `lru.Back()` before insert in one critical section; flights excluded; `Len` = `lru.Len()` under `mu`. |
| 6 Recency | Hit → `MoveToFront`; successful load → `PushFront`. |
| 7 Caller ctx/orphans | Loader gets `c.ctx`; abandoning waiter only deregisters itself; flight completes and admits. Priority (a) closed > (b) ctx-Done-while-in-flight > (c) result, decided at the first linearizing event under `mu` (I6) — post-resolution cancellation cannot override the result. |
| 8 Close | §4.4; discard-after-cancel via `closed` check in `run`; Get's closed check precedes any Loader call; concurrent 2nd Close waits on `closedDone`. |
| 9 Concurrency | One mutex for all shared state incl. waiter outcomes; no blocking under lock; `-race` + stress tests. |

## 6. Test approach

Commands (all must pass): `go build ./...`, `go vet ./...`, `go test ./...`,
`go test -race -count=20 ./...`, `gofmt -l .` empty.

Public suite unchanged. Additional tests in `blockcache_test.go` (synctest where ordering matters;
non-synctest waits bounded by a 5s ctx; `t.Errorf` inside bubbles before draining goroutines):

1. **PerWaiterCopies** — N waiters on one flight; mutate one result; others and a later hit unaffected; mutating the Loader's own slice after return does not affect the cache.
2. **ErrorWithBytesDiscarded** — Loader returns `(bytes, err)`: all waiters get err + nil bytes; `Len()==0`; next Get reloads.
3. **ErrorSharedByAllWaiters** — synctest: N waiters, one Loader call, all get the same error.
4. **CallerCtxCancelWhileInFlight** — synctest: cancel waiter ctx while Loader blocked ⇒ `context.Canceled`; release ⇒ `Len()==1`, next Get hits (orphaned load populates).
5. **CancelThenReleaseBeforeWaiterRuns** (finding regression, 7b) — synctest: waiter parked; in the root goroutine `cancel()` then immediately `close(release)` with no `synctest.Wait` in between ⇒ Get returns `context.Canceled` deterministically (settled by `run` via `w.ctx.Err()` if the waiter had not run yet).
6. **ResolveThenCancelReturnsResult** (finding regression, 7c) — white-box, deterministic. The
   implementation factors §4.1's post-join half into `c.await(ctx, f, w) ([]byte, error)` and §4.2's
   locked section into `c.resolve(key, f, val, err)`. The test registers a waiter on a hand-built
   flight (under `mu`, exactly as Get does), calls `c.resolve(...)` with a success value, *then*
   cancels the waiter's ctx, *then* calls `c.await` — both `f.done` and `ctx.Done()` are ready at
   wake-up, which is exactly the finding's scenario. Assert `await` returns the loaded bytes and
   `nil` error. A mirror case (cancel → resolve → await) asserts `context.Canceled`.
7. **OtherWaitersUnaffectedByOneCancel** — one waiter cancels, another still gets the value; `f.waiters` deregistration verified white-box (map empty after).
8. **LoaderGetsCacheCtx** — caller cancel does not cancel Loader's ctx; Close does.
9. **CloseWaitsForLoader** — synctest: Loader blocks until ctx Done plus a delay; Close does not return earlier; waiters get `ErrClosed`; `Len()==0`; success-after-cancel not stored.
10. **CloseWithCancelledWaiter** — waiter ctx cancelled and Close in the same step ⇒ `ErrClosed` (priority a).
11. **CloseZeroWaiterLoad** — waiter left via ctx, then Close; Loader returns success after cancel ⇒ nothing stored.
12. **ConcurrentClose** — synctest: second Close during the first's join returns only after it; both nil.
13. **GetAfterCloseNoLoader** — Loader count stays 0.
14. **InflightNotCountedTowardCapacity** — cap 1, one resident, one blocked flight: resident still hits, `Len()==1`; on resolution old key evicted.
15. **LoadMarksMRU** — cap 2: a, b, c evicts a.
16. **LenNeverAboveCapacityUnderPoll** — poller asserts `Len() ≤ capacity` during concurrent loads.
17. **StressRace** — many goroutines × random keys × small capacity × random ctx cancels, Close at end; bytes match key; nothing hangs.
18. **LoaderPanic** — panic ⇒ error to waiters, no hang, cache usable.
19. **NewPanics** extra — negative capacity.

## 7. Judgment calls (documented in code comments)

- Rule 7 priority is applied at the first linearizing event under `mu` (resolution / Close / waiter
  observing its own ctx while unsettled), not at goroutine wake-up time.
- A waiter settled with its load result before Close returns that result even if Close runs before
  the waiter goroutine is scheduled (it is no longer a "current waiter").
- Miss with an already-Done ctx still starts the load (orphaned; populates cache).
- Hit with a Done ctx returns the data.
- Loader `(nil, nil)` caches an empty non-nil block.
- Loader panic converted to an error (keeps the no-deadlock guarantee).
- `ctx` passed to `Get` must be non-nil (Go convention; not checked).
- Close wakes waiters (with their already-settled `ErrClosed`) before joining Loaders (§4.4).
