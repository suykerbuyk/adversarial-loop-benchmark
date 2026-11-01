# Implementation Plan: `blockcache` (round 3)

Round 3 responds to the round-2 `plan_findings.md`. Point-by-point answers are in
`plan_rebuttal.md`. Changes since round 2:

- **R2-F1 (major, fixed):** this plan no longer claims full compliance for a Close called from
  inside a Loader. §3.7 now says plainly that the spec cannot be satisfied in that case. Rule 8
  says Close waits for every in-flight Loader call, and rule 9 says nothing deadlocks. A Loader
  that synchronously calls Close on its own cache makes those two rules contradict each other.
  The plan resolves the conflict with the spec's own Loader contract: such a Loader does not
  "return promptly when its ctx is Done". Close keeps rule 8 literally, with no exceptions. The
  case is documented as a forbidden use in the Loader and Close doc comments.
- **R2-F2 (major, fixed):** `goid()`, `goid.go`, `loaderGs`, `loadersInClose`, and the
  reentrancy exclusion are all **removed**. No correctness path depends on undocumented runtime
  formatting. The join is back to a plain `sync.WaitGroup`.
- **R2-F3 (minor, fixed):** Loader panic recovery is **removed**. The package policy is stated
  in the `Loader` doc comment: a Loader must not panic, and if it does, the panic is not
  recovered. It crashes the program like any other unrecovered goroutine panic. The cache
  converts nothing and swallows nothing.
- The round-1 F2 fix stays: waiters decide under `mu`, and results are published under `mu`.

Target: module `blockcache` (go 1.25, per `worktree/go.mod`), package `blockcache`.
- Production code: one file, `blockcache.go`.
- Extra tests: one file, `blockcache_internal_test.go`.
- The public test file is used exactly as shipped.

---

## 1. Public API (exactly as specified, nothing added)

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

- No other exported identifiers exist. The concrete type `*cache` is unexported, and `New`
  returns it as `Cache`.
- The doc comments add usage notes only, not new behaviour:
  - **`Loader` must not call `Close` on the cache it serves.** It must not do so directly, and
    it must not block on another goroutine that does. Doing so is a case of not returning
    promptly once ctx is Done (§3.7).
  - **`Loader` must not panic.** A panic is not recovered (§3.6).
  - **`Get` returns a copy.** On error it returns nil bytes. It follows the rule 7 priority order.
  - **`Close` wakes waiters immediately**, waits for all Loader calls, and is idempotent.

---

## 2. Internal data model

```go
type cache struct {
    load     Loader
    capacity int

    ctx    context.Context    // cache-scoped; every Loader call receives this one
    cancel context.CancelFunc

    loads sync.WaitGroup      // one count per run goroutine; Add only under mu while !closed

    mu       sync.Mutex                // guards every field below
    closed   bool
    closing  chan struct{}             // closed (under mu) when the first Close starts
    entries  map[string]*list.Element  // key -> element in lru; element.Value is *entry
    lru      *list.List                // front = MRU, back = LRU; resident entries only
    inflight map[string]*call          // unresolved loads; NOT resident, NOT counted, NOT evictable
}

type entry struct {
    key string
    val []byte // cache-owned; never written after it is stored (immutable)
}

type call struct {
    done     chan struct{} // closed exactly once, under mu, after val/err/resolved are set
    resolved bool
    val      []byte        // cache-owned copy on success (the same immutable slice as the entry)
    err      error
}
```

Invariants (each holds whenever `mu` is not held):
- **I1** `len(entries) == lru.Len() <= capacity`.
- **I2** When a new load begins, the key is in neither `entries` nor `inflight`. This holds
  because the miss check and the insert into `inflight` happen in one critical section.
- **I3** Stored byte slices (`entry.val`, `call.val`) belong to the cache and are never mutated.
  That makes copying them outside the lock safe.
- **I4** `loads.Add(1)` only runs under `mu` while `closed == false`. Close's `loads.Wait()` only
  runs after `closed = true` has been set under `mu`. So every `Add` happens-before every `Wait`.
  This is the documented `WaitGroup` requirement: no positive `Add` may race a `Wait` when the
  counter is zero.
- **I5** Once `closed` is true, nothing is ever inserted into `entries` or `inflight` again.
  - Get refuses to insert at its closed check.
  - `run` refuses to admit when `closed` is true.

`New` does the following:
1. Panics with a descriptive message if `capacity < 1` or `load == nil`.
2. Sets `ctx, cancel = context.WithCancel(context.Background())`.
3. Makes the maps, the list, and `closing`.

---

## 3. Algorithms

### 3.1 `Get(ctx, key)`
```
mu.Lock()
if closed { mu.Unlock(); return nil, ErrClosed }      // rule 8, last sentence: no Loader call
if el, ok := entries[key]; ok {                       // rule 1: hit
    lru.MoveToFront(el)                               // rule 6
    v := el.Value.(*entry).val
    mu.Unlock()
    return clone(v), nil                              // rule 4 (copy outside lock; I3)
}
cl, ok := inflight[key]
if !ok {                                              // rule 2: the first misser starts the one load
    cl = &call{done: make(chan struct{})}
    inflight[key] = cl
    loads.Add(1)                                      // I4
    go c.run(key, cl)
}
mu.Unlock()
return c.wait(ctx, cl)
```

**`clone`.** `clone(v)` is `out := make([]byte, len(v)); copy(out, v); return out`.
- It always returns a fresh, non-nil slice. A successful load of nil or empty bytes therefore
  comes back as a non-nil empty slice.
- Error paths return a nil slice. The public test asserts `b == nil` on error.

**Hits.** A hit does not consult the caller's ctx. Rule 7's priority applies to a *waiting* Get,
and a hit does not wait.

**Misses with a ctx that is already Done.** The load still starts, because loads are not tied
to callers (rule 7). `wait` then returns `ctx.Err()` immediately, unless the cache has closed or
the load has already resolved.

### 3.2 `wait(ctx, cl)`: rule 7 priority, decided under `mu`
```
select {
case <-cl.done:
case <-ctx.Done():
case <-c.closing:
}
mu.Lock()
closed, resolved, val, err := c.closed, cl.resolved, cl.val, cl.err
mu.Unlock()
switch {
case closed:     return nil, ErrClosed      // (a)
case !resolved:  return nil, ctx.Err()      // (b) own ctx Done while the load is still in flight
case err != nil: return nil, err            // (c) error
default:         return clone(val), nil     // (c) success, own copy (rules 2 and 4)
}
```
- **The decision is atomic.** The `select` only waits. The decision is made from one snapshot
  under `mu`, so the random choice among ready `select` cases has no effect on the result.
- **One linearization point per waiter.** `closed` changes only under `mu` in Close, and
  `resolved` changes only under `mu` in `run`. Each waiter therefore has one linearization
  point: its critical section in `wait`.
  - If that section comes after Close, the waiter returns `ErrClosed`, even if the load had
    already succeeded.
  - If it comes before Close, the waiter returns the result, or `ctx.Err()`.
- **Liveness.** Whichever channel fired, the flag that goes with it is already set, because
  each flag is set before its channel is closed. If only `ctx.Done` fired, then
  `!resolved && !closed` correctly means the load is still in flight.

### 3.3 `run(key, cl)`: the single Loader call
```
defer loads.Done()                       // runs after the publishing critical section below
val, err := c.load(c.ctx, key)           // cache-scoped ctx, never a caller's (rule 7)
var owned []byte
if err == nil { owned = clone(val) }     // rule 4: copy before locking; Loader's slice not retained
mu.Lock()
delete(inflight, key)
switch {
case closed:                             // rule 8: result arriving after cancellation is discarded
    cl.err = ErrClosed
case err != nil:                         // rule 3: error wins, even with non-nil bytes; not stored
    cl.err = err
default:
    c.admit(key, owned)                  // rules 2, 5, 6
    cl.val = owned
}
cl.resolved = true
close(cl.done)
mu.Unlock()
```

**Checking `closed` stands in for checking the cancelled context.** Close sets `closed = true`
and calls `cancel()` in the same critical section. So, as seen under `mu`, "closed" and "cache
ctx cancelled" are the same state.
- A result that lands after cancellation is always discarded. That includes the zero-waiter
  case and the close-window case. This is why "drops all entries" holds after Close.
- A success that is published before Close takes `mu` is admitted normally. Close's drop then
  removes it.

**Ordering of `Done`.** `loads.Done()` runs after `mu` is released. So when `loads.Wait()`
returns in Close, every `run` has finished publishing.

### 3.4 `admit(key, val)` (called under `mu`)
```
if el, ok := entries[key]; ok {           // defensive only; I2 makes it unreachable
    el.Value.(*entry).val = val; lru.MoveToFront(el); return
}
if lru.Len() >= capacity {                // rule 5: evict the LRU entry first
    back := lru.Back(); lru.Remove(back); delete(entries, back.Value.(*entry).key)
}
entries[key] = lru.PushFront(&entry{key: key, val: val})   // MRU (rule 6)
```
- Eviction and insertion happen in one critical section, so `Len()` can never observe
  `capacity+1`, even while another goroutine is polling it.
- Unresolved loads live only in `inflight`, so they are never counted toward capacity and never
  evicted.

### 3.5 `Len()`
`mu.Lock(); n := lru.Len(); mu.Unlock(); return n`.
- It counts resident entries only.
- It is never above capacity (I1).
- It can lag a completing load by at most `run`'s publishing critical section.

### 3.6 Loader panics: explicit policy, no recovery (R2-F3)
The spec defines what happens for each Loader *return value*. It does not authorize turning a
panic into an error, so the cache does not try. `run` calls the Loader directly.

A panic unwinds the `run` goroutine, which has no `recover`. Go then terminates the program and
prints the panic and its stack, the same as for any unrecovered panic in a goroutine.
Consequences:
- Panic behaviour is never silently lost.
- No waiter is left hanging. A crashed process cannot hang, so the deadlock audit is not
  affected.

The `Loader` doc comment states this policy. If a user wants panics turned into errors, they can
wrap their own Loader. That is their decision, not the cache's.

### 3.7 `Close()`
```
mu.Lock()
if !closed {
    closed = true
    cancel()                    // rule 8: cancel the cache-scoped ctx
    close(closing)              // rule 7(a)/8: wake every current waiter -> ErrClosed
    clear(entries); lru.Init()  // rule 8: drop all entries (I5 keeps the cache empty from here on)
}
mu.Unlock()
loads.Wait()                    // rule 8: wait for every in-flight Loader call to return
return nil
```

**How Close meets rule 8.**
- **Cancel.** `cancel()` runs exactly once, in the first Close. It is non-blocking, so calling it
  under `mu` is safe.
- **Wait for every in-flight Loader call, with no exceptions.**
  - By I4, every load that was started was counted before `closed` was set, and no load can be
    started afterwards.
  - So `loads.Wait()` returns only once every Loader call ever started has returned and its
    `run` has published.
  - That includes zero-waiter orphaned loads.
- **Wake every current waiter.** `closing` is closed as soon as Close starts.
  - By the snapshot rule in §3.2, every waiter whose decision comes after this point returns
    `ErrClosed`.
  - The waiters that were blocked when Close began therefore all return `ErrClosed`.
- **Drop all entries.** The drop happens at the start of Close. That is equivalent to dropping
  after the join, because by I5 nothing can be admitted once `closed` is true:
  - `run` discards late results.
  - Get refuses before it touches the maps.

  At every instant after Close begins, `Len() == 0`. That is at least as strong as "dropped by
  the time Close returns".
- **Idempotent, with a shared join.** A second Close, whether it runs concurrently or later,
  skips the `if` block and calls `loads.Wait()` on the same WaitGroup.
  - While the first Close is still joining, the second waits for that same join and returns
    nil. This is the rule 8 parenthetical.
  - After the join has finished, the counter is zero, so the second Close returns immediately.
  - Calling `WaitGroup.Wait` concurrently from several goroutines is supported.
- **Late Gets.** Any Get whose closed check runs after `closed = true` returns `ErrClosed`
  without calling the Loader. That includes Gets that would have been hits.

**Why waiters are woken when Close starts, not after the join.**
- Rule 7(a) only says a waiting Get returns `ErrClosed` "if the cache closes".
- Waking waiters after the join would make a legitimate Loader deadlock. Suppose a Loader calls
  `Get` for a different key and waits on that load. That waiter would wait for the join, and the
  join would wait for the Loader.
- By the time Close returns, every postcondition in rule 8 holds.

**Close called from inside a Loader: the spec cannot be satisfied here, and this plan says so
(R2-F1).**
- **The conflict.** Suppose a Loader synchronously calls `Close` on its own cache, directly or by
  blocking on another goroutine that calls it. Rule 8 then requires Close to wait for that
  Loader call to return. But the Loader cannot return until Close returns. No implementation can
  satisfy both rule 8 ("waits for every in-flight Loader call to return") and rule 9 ("must not
  deadlock") in this case, so the spec is unsatisfiable for that use.
- **The resolution.** The spec's own contract settles it: "A Loader MUST return promptly when
  its ctx is Done."
  - Close cancels the ctx before it joins.
  - A Loader that is parked inside Close is therefore a Loader that is not returning promptly
    after its ctx is Done.
  - This is the same contract violation as a Loader that ignores ctx entirely. That case also
    makes Close block, and rule 8 *requires* Close to block in it.
- **What the plan does.**
  - It keeps rule 8 literally: no exclusions and no goroutine-identity heuristics.
  - It documents the case as forbidden use, in the `Loader` and `Close` doc comments.
  - It does **not** claim that this use is supported or free of deadlock.
- **Scope of rule 9.** The no-deadlock guarantee covers every use that keeps to the Loader
  contract.

**Allowed reentrancy, which stays deadlock-free.**
- A Loader may call `Get` on the same cache, for other keys.
- A Loader may call `Len`.
- When the cache closes, those inner Gets are released by `closing`, so the Loader can then
  return. A Loader that calls `Get` for its *own* key waits on its own load. That is a cycle in
  the Loader's own logic. It is still released by `closing` when the cache closes, and is
  documented as pointless.

---

## 4. Rule-by-rule traceability

| Rule | Where satisfied |
|---|---|
| 1 Hit: copy, MRU, no Loader | §3.1 hit branch: `MoveToFront` plus `clone`; no `run` is started |
| 2 Single-flight | The `inflight` check and insert are in the same critical section as the `entries` miss check (I2). Only the creator starts `run`. All waiters share one `call`, and each gets its own `clone(val)` |
| 3 Error | §3.3 checks `err` first. Bytes are ignored when `err != nil`. The key leaves `inflight` and is never admitted, so the next Get reloads |
| 4 Ownership | The Loader's slice is cloned in `run` and not retained. Every return of bytes goes through `clone` |
| 5 Capacity/LRU | §3.4 evicts `lru.Back()` and pushes the new entry in one critical section. In-flight loads exist only in `inflight`. `Len` is `lru.Len()` read under `mu` |
| 6 Recency | `MoveToFront` on a hit; `PushFront` on a successful admit |
| 7 Ctx/orphans | The Loader gets `c.ctx`, never a caller's ctx. `run` does not depend on waiters, so an orphan's success is admitted. The (a)/(b)/(c) priority is decided atomically in §3.2 |
| 8 Close | §3.7: cancel, wake, and drop at start, then `loads.Wait()` for every Loader call (no exclusions). Late results are discarded (§3.3). Concurrent and later Closes share the same `Wait`. Get's closed check prevents any Loader call after Close. A Loader calling its own Close is flagged as the one case where rule 8 and rule 9 conflict, and is classed as a Loader-contract violation |
| 9 Concurrency | All shared state is under one mutex. Stored bytes are immutable. Results are published under `mu`, and wake-ups use channel close. No lock is held across the Loader call, the `select`, or `loads.Wait()`. The WaitGroup Add/Wait ordering follows I4 |

**Deadlock audit.**
- `mu` is never held across a Loader call, a channel wait, or `loads.Wait()`. Every critical
  section is straight-line code with no blocking.
- `run` always reaches its publishing section and `loads.Done()` once the Loader returns. The
  Loader returns under its contract, and a panic terminates the process (§3.6).
- Every waiter wakes on one of `done`, its own `ctx`, or `closing`. Each of those closes or fires
  eventually, provided the Loader respects its contract.
- The only cycle left is a Loader that waits on its own cache's Close (§3.7). That use is a
  contract violation which the spec itself makes unsatisfiable, and it is documented.

---

## 5. Code-quality notes
- Each exported item has a doc comment that restates its contract. That includes the copy
  semantics, the priority order, Close behaviour, and the two Loader rules: no Close of its own
  cache, and no panics.
- The LRU uses `container/list`, which is standard and O(1). Maps are emptied with the builtin
  `clear`.
- There are no goroutine-identity tricks, no `recover`, and no `runtime` package use.
- The only goroutines are one per in-flight load.
- The code must be `gofmt`-clean and `go vet`-clean. Storing `cancel` in the struct does not
  trigger `lostcancel`.

---

## 6. Test approach

1. **Public suite.** `blockcache_public_test.go` is used unchanged. Run
   `go test -race -count=20 ./...` and `go vet ./...`.
2. **Extra tests** go in `blockcache_internal_test.go`.
   - Overlap is forced with `testing/synctest`.
   - Waits on channels count as durably blocking inside the bubble. So does `WaitGroup.Wait`,
     because the WaitGroup is used from goroutines inside the bubble.
   - Every wait is bounded by a 5s ctx, so a deadlock fails the test instead of hanging it.
   - Checks made while goroutines are still parked use `t.Errorf`, as the public suite does.

   The tests:
   - *Per-waiter copies.* N overlapping waiters. Mutating one result does not affect the
     others, or a later hit.
   - *Loader slice not retained.* The Loader keeps its returned slice and mutates it after
     returning. The cached bytes are unchanged.
   - *Error with bytes.* A Loader returns `(non-nil, err)`. Every waiter gets `err` and nil
     bytes. `Len()==0`, and the next Get reloads.
   - *Error fan-out.* N overlapping waiters all get the same error from one call.
   - *Caller ctx cancel / orphan.* Cancel a waiter's ctx while the load is blocked.
     - The waiter gets `ctx.Err()`.
     - The Loader's ctx is not Done.
     - After release, the key is resident, and a later Get hits with a call count of 1.
   - *Resolved beats ctx.* The load resolved before the waiter decided, and the waiter's ctx is
     also Done. The waiter returns the bytes. This is deterministic because the decision is
     made under `mu`.
   - *Close joins Loaders.* A ctx-respecting Loader sleeps briefly after it sees Done, then sets
     a `returned` flag.
     - When Close returns, `returned` is true.
     - Every blocked waiter got `ErrClosed`.
     - `Len()==0`.
   - *Close waits even with zero waiters.* An orphaned load whose only waiter left. Close does
     not return before that Loader returns, and its success-after-cancel result is not stored
     (`Len()==0`).
   - *Late result discarded.* The Loader ignores Done for a bounded time and then returns
     success. Close returns only after that, and `Len()==0`.
   - *Concurrent Close.* Two Closes while a Loader is still joining. Neither returns before the
     Loader returns, and both return nil. A third Close after the join returns immediately with
     nil.
   - *ErrClosed priority.* A waiter whose own ctx is also Done during Close gets `ErrClosed`.
   - *Resolved-then-closed.* Release the load and call Close in the same step, before the
     waiter's goroutine runs its decision. The result is either the bytes or `ErrClosed`, never
     `ctx.Err()`. A white-box variant holds `c.mu` from the test so that `closed` is set before
     the waiter's snapshot, and asserts `ErrClosed`.
   - *Get after Close does not call the Loader.* The call count is unchanged, and the result
     includes a would-be hit.
   - *In-flight loads are not evictable or counted.* Capacity 1, key A resident, key B loading.
     - `Len()==1`, and A is still a hit.
     - When B resolves, A is evicted and `Len()==1`.
   - *Len poll under race.* One goroutine spins on `Len()` and asserts `<= capacity` while
     many goroutines Get distinct keys.
   - *Reentrant Get from a Loader.* A Loader calls `Get` for another key and works. If Close
     happens while the inner Get is waiting, the inner Get returns `ErrClosed`, the Loader
     returns, and Close completes.
   - *Stress.* Random keys, random ctx cancellations, and concurrent external Closes, all under
     `-race`. Asserts no panic, every goroutine finishes within the deadline, and
     `Len()<=capacity`.
   - **Not tested:** Close called from a Loader, and Loader panics. Both are documented forbidden
     use (§3.6, §3.7). One would hang and the other would crash the test binary by design.
3. **Gate.** `gofmt -l` is empty, `go vet` is clean, and `go test -race -count=20` passes.
