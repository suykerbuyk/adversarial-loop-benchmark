# Implementation Plan: `blockcache`, a bounded concurrent single-flight block cache

Round 3. This revision responds to the round-2 `plan/plan_findings.md`. The point-by-point answer is in
`plan/plan_rebuttal.md`. Changes from round 2:
- **Major (ErrClosed on closed-branch calls): rebutted.** The plan now gives a linearizability
  argument showing `ErrClosed` is the only correct answer for that reader (section 3, `run`). It also
  adds a deterministic test that asserts it (test 8e).
- **Minor (test 8b does not hit the live race): fixed.** An unexported, nil-by-default test seam
  `testHookWait` runs between the `closing` recheck and the `cl.done` recheck. It lets tests put
  resolution, cancellation, or Close exactly into that window (tests 8b to 8e).
- **Minor (dead re-admit branch in `admit`): fixed.** The branch is deleted, and the plan argues why I2 holds.
- **Nit (empty success gives non-nil slice): fixed.** This is now documented in the package comment.
- **Nit (`loaderPanicError.Error` can re-enter user code): fixed.** The message uses only `%T`, and
  the value is kept in a field.

## 0. Layout
- `worktree/blockcache.go`: the whole package (doc comment, types, `New`, `Get`, `Len`, `Close`, the
  load goroutine, `loaderPanicError`).
- `worktree/blockcache_test.go`: extra tests by the implementor (internal, `package blockcache`), next
  to the shipped `blockcache_public_test.go`, which stays byte-identical.
- Module `blockcache`, `go 1.25` (already in `go.mod`; `testing/synctest` is available). Standard
  library only: `container/list`, `context`, `errors`, `fmt` (only `%T` in the panic message), `sync`.

## 1. Public API (exactly as the spec gives it)
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
Nothing else is exported. The concrete type `*cache` is unexported. `ErrClosed` is returned unwrapped,
so both `==` and `errors.Is` work.

Package doc comment (summary of what it documents): Get returns a fresh copy that the caller owns. A
successful load of a nil or empty block is returned as a non-nil, zero-length slice (`[]byte{}`). On
any error, Get returns a nil slice. Callers must therefore use `err`, not `b == nil`, to detect
failure. Loaders must honour ctx. Out-of-contract uses (Close from inside a Loader, a Loader waiting
on its own key, panicking Loaders) are covered in section 5.

## 2. Internal data model
```go
type entry struct {
    key string
    val []byte // owned by the cache; never changed after it is stored
}

type call struct {
    done chan struct{} // closed exactly once, when the call resolves
    val  []byte        // result owned by the cache (nil on error); read-only after done is closed
    err  error
}

type cache struct {
    capacity int
    load     Loader

    ctx    context.Context    // cache-scoped; every Loader runs under it
    cancel context.CancelFunc

    mu       sync.Mutex
    closed   bool                     // set once, under mu, together with cancel()
    ll       *list.List               // LRU list of *entry; Front = MRU, Back = LRU
    items    map[string]*list.Element // resident entries only
    inflight map[string]*call         // unresolved loads only (not resident, not counted)

    closing   chan struct{}  // closed in Close's first critical section; wakes all waiters
    loads     sync.WaitGroup // one count per running Loader goroutine
    closeDone chan struct{}  // closed after the first Close finishes joining and cleanup

    // testHookWait, if non-nil, runs inside wait between the closing recheck and the
    // cl.done recheck. Only internal tests set it, right after New and before any Get.
    // After that it is read-only, so reading it is race-free. In production it is always nil.
    testHookWait func(cl *call)
}
```
Invariants. Each one holds whenever `mu` is not held:
- I1 `len(items) == ll.Len() <= capacity`.
- I2 A key is never in both `items` and `inflight`. Here is why. A key enters `inflight` only in Get,
  under `mu`, after the `items` lookup missed in that same critical section. A key enters `items` only
  in `run` for the call that owns `inflight[key]`, which deletes that `inflight` entry in the same
  critical section. Only one call per key can be in `inflight`, so nobody else can admit the key while
  the call is unresolved. So `admit` never sees a resident key.
- I3 A `call` is removed from `inflight` in the same critical section that sets its result and closes
  `done`. So any `call` found in `inflight` is unresolved.
- I4 `loads.Add(1)` runs only under `mu` while `closed == false`. Once `Close` sets `closed`, no `Add`
  can race `loads.Wait()`, which is what the WaitGroup reuse rule requires.
- I5 Once `closed` is true, nothing is ever added to `items` or `inflight`.
- I6 Byte slices in `entry.val` and `call.val` are never written after publication. An `*entry` is
  never mutated after it is pushed onto `ll`. Every handout is a fresh copy. So copying a slice header
  out under `mu` and cloning it after unlocking is race-free.
- I7 Get does the closed check, the hit lookup, the inflight lookup, and on a miss the inflight insert
  plus `loads.Add(1)`, all in **one** critical section. While `mu` is free, no Get is "past the
  closed check but not yet hit/joined/started".

## 3. Algorithms

### New
Panic with a clear message if `capacity < 1` or `load == nil`. Create
`ctx, cancel := context.WithCancel(context.Background())`, the maps, the list, and the `closing` and
`closeDone` channels.

### Get(ctx, key)
```
mu.Lock()
if closed            { unlock; return nil, ErrClosed }            // rule 8: no Loader call
if el := items[key]  { ll.MoveToFront(el); v := el.Value.(*entry).val; unlock; return clone(v), nil }  // rules 1, 6
if err := ctx.Err(); err != nil { unlock; return nil, err }       // caller already gone: do not start/join
cl := inflight[key]
if cl == nil {
    cl = &call{done: make(chan struct{})}
    inflight[key] = cl
    loads.Add(1)                                                  // I4
    go c.run(key, cl)
}
mu.Unlock()
return c.wait(ctx, cl)
```
A hit copies only the slice header under `mu` and clones after unlocking, so a large block never
stalls other Gets, `Len`, `run`, or `Close`. I6 makes this race-free. If Close or eviction drops the
entry in the meantime, the caller clones the old immutable bytes, which is correct for a hit
linearized before the drop.

`clone(b)` is `append(make([]byte, 0, len(b)), b...)`. It always returns a fresh non-nil slice
(documented, section 1).

A hit ignores the caller's ctx, because a hit never waits and rule 7 is about *waiting* Gets. A miss
with an already-Done ctx returns `ctx.Err()` without starting or joining a load. No load is in flight
for that caller, so starting one would only waste backing-store work.

### wait(ctx, cl): priority (a) ErrClosed, (b) ctx.Err() while in flight, (c) result
```go
func (c *cache) wait(ctx context.Context, cl *call) ([]byte, error) {
    select { // block until at least one of the three is ready
    case <-c.closing:
    case <-ctx.Done():
    case <-cl.done:
    }
    // Go picks randomly among ready cases, so whichever one woke us, re-check in spec priority order.
    // Each check is a non-blocking select: if its channel is closed, that case IS taken.
    select {
    case <-c.closing:
        return nil, ErrClosed // (a)
    default:
    }
    if c.testHookWait != nil {
        c.testHookWait(cl) // test seam: the window between the (a) and (c) checks
    }
    select {
    case <-cl.done: // (c) resolved
        if cl.err != nil {
            return nil, cl.err
        }
        return clone(cl.val), nil // rule 4: own copy per waiter
    default:
    }
    return nil, ctx.Err() // (b): reached only if cl.done was observed NOT closed, i.e. still in flight
}
```
- **ctx.Err() only while the load is in flight.** The last line runs only after the non-blocking
  receive on `cl.done` took `default`, which means `cl.done` was not closed at that instant. When
  cancellation and resolution overlap, the waiter returns the result whenever resolution came first.
  Which case woke the first select does not matter. We reach the last line only if a first-select case
  fired while neither `closing` nor `cl.done` was closed. So `ctx.Done()` fired, and `ctx.Err()` is
  non-nil.
- Reading `cl.val`/`cl.err` after `<-cl.done` is race-free, because closing a channel happens-before
  any receive that sees it closed. Waiters only ever read `cl.val` (I6).
- On error, the Loader's error value is passed through unchanged (`errors.Is` holds), and the bytes are nil.

### run(key, cl): the single Loader invocation
```
defer c.loads.Done()
var b []byte; var err error
func() {                                         // panic safety: always resolve the call
    defer func() { if r := recover(); r != nil { b, err = nil, &loaderPanicError{value: r} } }()
    b, err = c.load(c.ctx, key)                  // cache-scoped ctx, never a caller's (rule 7)
}()
var owned []byte
if err == nil { owned = clone(b) }               // copy OUTSIDE mu; the Loader slice is never kept (rule 4)
                                                 // err != nil: the error wins, bytes discarded (rule 3)
mu.Lock()
delete(inflight, key)
switch {
case closed:     cl.err = ErrClosed              // resolved after Close: discard, do not store (rule 8)
case err != nil: cl.err = err                    // not stored, so a later Get re-loads (rule 3)
default:
    c.admit(key, owned)                          // rule 5
    cl.val = owned
}
close(cl.done)
mu.Unlock()
```
Checking `closed` under `mu` matches "returned after the cache-scoped ctx was cancelled", because
`Close` sets `closed` and calls `cancel()` in one critical section. A run that resolves before that
section stores normally, and Close then drops the entry. A run that resolves after it discards the
result. Either way Close ends with no entries.

**Why the `closed` branch publishes `ErrClosed`, not the Loader's outcome (round-2 major, rebutted).**
Look at every waiter that could read this call's published result:
1. A waiter whose `closing` recheck runs after Close's first section returns `ErrClosed` at that
   check and never reads `cl`.
2. The only other reader is waiter W. W ran its `closing` recheck before Close's first section (so it
   saw the cache open) and runs its `cl.done` recheck after `run`'s `closed` branch (so it sees the
   call resolved). W's `Get` call spans this order:
   `W.closing-check < Close.section1 < run.closedBranch(close(done)) < W.done-check`.
   For W to linearize at some point P in its interval:
   - P before Close.section1: the cache is open and the load is unresolved. The only answers there
     are (b) (if W's ctx is Done) or "still waiting". (c) is impossible because nothing has resolved.
   - P after Close.section1: the cache is closed. Rule 7 puts (a) first, so the answer is `ErrClosed`.
   So **no point in W's interval allows (c)**. The point that would need a resolved load and an open
   cache does not exist. If W returned the Loader's `([]byte, error)`, it would return a result that
   only exists after Close while claiming the cache was open. That breaks rule 7's priority. It also
   breaks rule 8's "wakes every current waiter with `ErrClosed`", because W was a current waiter when
   Close ran. `ErrClosed` is valid (P after Close). W's own `ctx.Err()` would also be valid only at a
   P before Close, and the code does not choose it, which is allowed.
3. The adversary says W "is specified by rule 7(c) to receive the load's result". That is wrong. Rule
   7(c) applies only "else", that is, when the cache has not closed. When W observes the resolution,
   it has.
- Error variant: if the Loader returned an error after cancel, W still gets `ErrClosed`, for the
  same reason. `errors.Is(err, loaderErr)` is not owed to anyone in this case. Test 8b involves no
  Close and is unaffected. Test 8e pins down the Close-window behaviour.

`loaderPanicError` (unexported):
```go
type loaderPanicError struct{ value any }
func (e *loaderPanicError) Error() string {
    return fmt.Sprintf("blockcache: loader panicked (panic value of type %T)", e.value)
}
```
`%T` prints only the dynamic type name via reflection. It never calls the value's `Error`/`String`
methods, so formatting cannot re-enter user code or re-panic. The raw value stays in `value` for
debugging. The message is not a stable API (section 5).

`run` does not depend on how many waiters are left. It holds no waiter references, and its success
is admitted even with zero waiters (rule 7: orphaned load populates the cache). It is bounded by the
Loader contract and joined by `Close`, so it is not a leak.

### admit(key, val) (under mu; precondition by I2: key not resident)
```
if ll.Len() >= capacity { back := ll.Back(); ll.Remove(back); delete(items, back.Value.(*entry).key) }
items[key] = ll.PushFront(&entry{key, val})      // MRU (rule 6)
```
No re-admit branch (round-2 minor). I2 (section 2) is a structural guarantee, so the branch would be
dead code. Eviction runs before insertion in the same critical section, so `Len()` never sees
`capacity+1` (I1). In-flight calls live only in `inflight`, never in `ll`, so they are neither counted
nor evictable (rule 5).

### Len
`mu.Lock(); n := ll.Len(); mu.Unlock(); return n`. This counts resident entries only, is at most
capacity (I1), and reflects every completion whose critical section has finished.

### Close
```
mu.Lock()
if closed { mu.Unlock(); <-closeDone; return nil }   // 2nd/concurrent Close waits on the SAME join
closed = true
cancel()                                             // cancel the cache-scoped ctx
close(closing)                                       // wake every current waiter -> ErrClosed (7a)
ll.Init(); clear(items)                              // drop all entries (Len() == 0 from here on); safe by I7
mu.Unlock()
loads.Wait()                                         // wait for every in-flight Loader call to return
mu.Lock(); ll.Init(); clear(items); clear(inflight); mu.Unlock()   // belt-and-braces; I5 means already empty
close(closeDone)
return nil
```
- Waiters are woken at the start of Close, not after the join. Rule 7(a) gives `ErrClosed` priority
  as soon as the cache closes, and a slow Loader should not hold waiters. By the time `Close` returns,
  every listed effect has happened: ctx cancelled, all Loaders returned, every waiter woken with
  `ErrClosed`, all entries dropped.
- Dropping entries before the join is safe by I7. Every Get is serialized wholly before or wholly
  after Close's first section. Gets before it have already hit, joined, or started a counted load.
  Gets after it see `closed` and return `ErrClosed` with no Loader call. I5 and `run`'s `closed`
  branch prevent re-admission during the join.
- Idempotent. Later or concurrent callers block on `closeDone` and return nil only after the same
  join completes.
- No lock is held across `loads.Wait()`. Runs need `mu` to resolve, so holding it would deadlock.

## 4. Rule-by-rule compliance
| Rule | How it is satisfied |
|---|---|
| 1 Hit | `items` lookup under mu, `MoveToFront`, return `clone` (made outside mu). No Loader call. |
| 2 Single-flight | `inflight` is checked and inserted under mu, so there is exactly one `run` per unresolved key. Every waiter selects on the same `call.done` and clones `call.val` itself. |
| 3 Error | `run` copies bytes only when `err == nil`. On error it sets `cl.err` and stores nothing. The `inflight` entry is deleted, so the next Get re-loads. |
| 4 Copy | `run` clones the Loader slice before publishing. Hits and waiters each clone again. Stored slices are never written (I6). |
| 5 Capacity/LRU | `admit` evicts `ll.Back()` before `PushFront`, in one critical section. In-flight calls are kept out of `ll`. `Len` reads `ll.Len()` under mu. |
| 6 Recency | `MoveToFront` on a hit, `PushFront` on a successful admit. |
| 7 Ctx/orphans | The Loader gets `c.ctx`. `run` does not depend on waiters. `wait` re-checks in priority order (closing, done, ctx). The ctx error is returned only if done was observed unresolved. |
| 8 Close | Cancel, wake, and drop in one section, then `loads.Wait()`, with `closeDone` making Close idempotent. `run` discards results when `closed`. Get checks `closed` first. |
| 9 Concurrency | All shared state is under `mu`. Results are published by channel close. WaitGroup Add happens only under mu before close (I4). No lock is held across a Loader call, a channel wait, or `loads.Wait()`. There is only one mutex. `testHookWait` is written before any goroutine exists and only read after that. |

## 5. Edge cases and documented limits
- A Loader returning `(nil, nil)` stores an empty block, and Get returns `[]byte{}` (non-nil, documented).
- A Loader returning both non-nil `(b, err)`: the error is returned with nil bytes, and `b` is never touched.
- A Loader panic is out of contract. It is recovered into an unexported `*loaderPanicError` for that
  call's waiters. The message names only the panic value's type, and the message is not a stable
  API. Nothing is stored, and the WaitGroup is still released, so `Close` cannot hang.
- Also out of contract, and documented in the package comment: calling `Close` from inside a Loader
  (a self-join), or a Loader that does `Get` on its own key and ignores ctx. A Loader that ignores
  ctx breaks the "return promptly" contract, and `Close` blocks until it returns, as specified.
- A nil caller ctx is not supported, following stdlib convention.

## 6. Test approach
In `worktree/`, run `gofmt -l .` (must print nothing), `go vet ./...`,
`go test -race -count=20 ./...`, and `go test -race -cpu=1,4,8 ./...`. The shipped public suite must pass
unchanged. `TestSingleFlight` runs inside synctest. That works because waiters block on channel
`select`s, the Close join is a WaitGroup (both durably blocking), and cache goroutines start inside the
bubble. Extra tests go in `blockcache_test.go`. They are deterministic: they use channels, synctest
where overlap must be forced, or `testHookWait` where a specific interleaving must be forced. Every
wait is bounded by a 5s ctx, and failures inside bubbles use `t.Errorf` before draining, as the public
suite does.

1. **Per-waiter copies**: N waiters on one load. Mutating one result does not affect the others or a later hit.
2. **Loader slice not retained**: the Loader mutates its returned slice after returning. Later hits are unaffected.
3. **Error with bytes**: `(b, err)` gives every waiter `err` and nil bytes. `Len()==0`, and the next Get reloads.
4. **Error fan-out**: N waiters under synctest all get `errors.Is(err, boom)`, with exactly one Loader call.
5. **LRU and in-flight not counted**: capacity 1, one resident key, and a blocked load for a second key.
   `Len()==1` and the resident key still hits. On release, the resident key is evicted.
6. **Len under concurrent poll**: a poller asserts `Len() <= capacity` while many keys load concurrently.
7. **Orphaned load populates the cache**: the caller's ctx is cancelled mid-load, and Get returns
   `context.Canceled`. After release, a fresh Get hits and the Loader count stays at 1.
8. **ctx priority**: a cancelled waiter gets `ctx.Err()` while the load is in flight, and a second waiter
   on the same call still gets the bytes.
8b. **Resolution in the window (live race, deterministic)**: the Loader blocks on `release`. The waiter's
   ctx is cancelled, so it wakes via `ctx.Done()` and passes the `closing` recheck. `testHookWait`
   (acting on its first invocation only) does `close(release); <-cl.done`. That puts resolution
   strictly between the `closing` and `cl.done` rechecks while ctx is already Done. Assert that the
   waiter returns the bytes, not `ctx.Err()`. Error variant: the Loader returns `boom`, and the waiter
   must get `errors.Is(err, boom)`.
8c. **Cancellation with the load still in flight**: the Loader blocks on `release`. The waiter's ctx is
   cancelled, so it wakes via `ctx.Done()`. The hook records that it ran and leaves the load
   unresolved. Assert that the hook ran and that the waiter returns `ctx.Err()`. Then release, and a
   fresh Get hits (orphan stored, Loader count 1). This pins the "observed unresolved, so (b)" path.
8d. **Pre-resolved and pre-cancelled (both first-select cases ready)**: call `c.wait` directly on a
   resolved `call` with a cancelled ctx, 1000 iterations. Must always return the result (and the error
   variant must always return the error).
8e. **Close in the window (round-2 major, pins the rebutted behaviour)**: the Loader honours ctx (it
   returns `[]byte("late"), nil` on `<-ctx.Done()`, or a `boom` error in the variant). The waiter's ctx
   is cancelled, so it wakes and passes the `closing` recheck while the cache is open. Then the hook
   calls `c.Close()` synchronously (Close cancels, the Loader returns, `run` takes the `closed`
   branch, the join completes) and then `<-cl.done`. Assert that the waiter returns
   `errors.Is(err, ErrClosed)` with nil bytes in both variants, that `Len()==0`, and that a later Get
   returns `ErrClosed` with no extra Loader call.
8f. **Hit during Close**: concurrent hits on a resident key race `Close` under `-race`. Each Get returns
   the bytes or `ErrClosed`, and the Loader count never goes above 1.
9. **Close wakes waiters**: blocked waiters all get `ErrClosed`, and the Loader saw `ctx.Done()`.
10. **Close waits for the Loader**: the Loader ignores ctx until a test channel is signalled. Under
    synctest, `Close` has not returned after `synctest.Wait()`, which is checked with a flag. After the
    signal it returns nil, and `Len()==0`.
11. **Zero-waiter result discarded**: the only waiter's ctx is cancelled, then Close is called, and the
    Loader returns success after cancel. Nothing is stored, and Get after Close gives `ErrClosed`.
12. **Concurrent Close**: two Closes are issued while the join is blocked. Both return nil, and only
    after the Loader returns.
13. **Get after Close does not call the Loader** (the call counter does not change).
14. **Pre-cancelled ctx**: on a miss it gives `ctx.Err()` with no Loader call. On a hit it still returns the bytes.
15. **Loader panic**: waiters get a non-nil error whose message contains the panic value's type, and
    whose formatting does not call the value's methods. The test uses a panic value whose `String()`
    method itself panics. A later Get reloads, and Close returns nil.
16. **Empty success**: `(nil, nil)` gives `err == nil`, `b != nil`, and `len(b) == 0`.
17. **Stress**: random Gets over a small key space, concurrent Len polls, and a mid-run Close, under
    `-race`. Checks: no panic, no deadlock (5s ctx), and every error is in {nil, ErrClosed, ctx errors}.

Hook tests (8b, 8c, 8e) set `c.testHookWait` right after `New` and before any Get, and do not run in
parallel with other users of the same cache.
