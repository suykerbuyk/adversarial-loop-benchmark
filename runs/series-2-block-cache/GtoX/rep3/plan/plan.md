# Implementation plan: `blockcache`

Package `blockcache` (module already at `worktree/`, Go 1.25). One file: `worktree/blockcache.go`. Public tests already live at `worktree/blockcache_public_test.go` (same content as `instrument/resolve_public_test.go`). No other packages, no extra dependencies. Do not edit the public tests.

There are no prior adversary findings (`plan/plan_findings.md` is absent), so this plan is the initial design.

## Public API

Match the spec signatures exactly. Nothing else is exported.

```go
package blockcache

import (
	"context"
	"errors"
)

// Loader fetches the block for key. The cache calls it at most once per in-flight
// miss for a key (single-flight). It is given a cache-scoped context, not any
// single caller's. A Loader MUST return promptly when its ctx is Done.
type Loader func(ctx context.Context, key string) ([]byte, error)

type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Len() int
	Close() error
}

func New(capacity int, load Loader) Cache

var ErrClosed = errors.New("blockcache: closed")
```

`New` panics if `capacity < 1` or `load == nil` (any panic value is acceptable; the public test only checks that `recover()` is non-nil). The concrete type is unexported (`*cache`). `ErrClosed` is a sentinel; callers use `errors.Is`. `Close` always returns `nil`.

## Internal data model

```go
type entry struct {
	key  string
	val  []byte // cache-owned copy; never the Loader's slice and never a caller's slice
	prev *entry // toward LRU
	next *entry // toward MRU
}

// flight is one in-flight single-flight load. It is not resident and does not
// count toward capacity until it resolves successfully and is admitted.
type flight struct {
	key    string
	done   chan struct{} // closed exactly once, after result is published
	result []byte        // cache-owned copy, or nil
	err    error
	gen    uint64        // flight generation captured at start; stale if cache was closed
}

type cache struct {
	cap   int
	load  Loader
	root  context.Context
	stop  context.CancelFunc

	mu       sync.Mutex
	cond     *sync.Cond          // signaled when closed becomes true (second Close waits here)
	closed   bool
	closeGen uint64              // bumped once, under mu, when Close begins
	joining  bool                // a Close is in Wait for in-flight loaders
	resident map[string]*entry   // only successfully admitted entries
	inflight map[string]*flight  // key -> the one live load; absent means no load
	head     *entry              // LRU sentinel
	tail     *entry              // MRU sentinel
	n        int                 // len(resident); always 0 <= n <= cap
	loads    sync.WaitGroup      // one Add per Loader goroutine still running
}
```

LRU is an intrusive doubly linked list with two sentinels so insert/unlink never special-cases empty ends:

- `head.next` is the least-recently-used resident entry.
- `tail.prev` is the most-recently-used resident entry.
- A hit or a successful admit unlinks the entry and relinks it immediately before `tail`.
- Eviction unlinks `head.next` and deletes that key from `resident`.
- Ties cannot arise: every mark-MRU moves the node to the unique MRU end, so the LRU end is a total order. "Any least-recent" is therefore just `head.next`.

`resident` and `inflight` are separate maps. A key is in at most one of them. A key that is loading is not in `resident` and is not evictable. `Len` reads `n` (or `len(resident)`) under `mu` and returns that integer; it never observes a value above `capacity`.

Byte ownership:

- On Loader success, `append([]byte(nil), src...)` copies into cache storage before the flight publishes. The Loader's slice is not retained. A nil or empty successful payload is stored as a non-nil empty slice (`[]byte{}`) only if the Loader returned non-nil empty; a nil successful slice is stored as a nil slice and returned as a nil slice. Either way the stored slice is not the Loader's backing array. (Public tests always use non-empty payloads. Hidden tests may pass nil or empty; both must not alias the Loader.)
- Every `Get` return copies again with `append([]byte(nil), stored...)`. Distinct waiters each get their own copy. Mutating a returned slice cannot change the cache or another waiter.
- On `(non-nil bytes, non-nil err)` the bytes are discarded and not copied into storage.

Flight publication:

- The Loader goroutine writes `result` and `err`, then `close(done)`, with those writes happening before the close (the close is the publication edge; waiters receive from the channel and then read the fields). No extra mutex is required for that handoff because the channel close happens-before receives.
- `flight.gen` is the `closeGen` observed when the load was started. On return, a result is stored only if `!closed && flight.gen == closeGen`. Close bumps `closeGen` (once) before cancelling, so a Loader that returns after cancel, including the zero-waiter case, cannot admit.

## Concurrency and locking

One mutex, `mu`, guards `closed`, `closeGen`, `joining`, `resident`, `inflight`, the LRU list, and `n`. `Len` takes `mu`.

Rules that keep the lock out of Loader and out of caller-cancel waits:

- Never call `load` while holding `mu`.
- Never wait on a caller's `ctx` while holding `mu`.
- Never call `loads.Wait` while holding `mu` (Close drops `mu` around the wait).
- The only channel a Get waits on while it might also need `mu` is `flight.done`, and it waits with `mu` released, via `select` together with `ctx.Done()` and a closed-notification path.

Closed notification without holding `mu` across the wait: each cache has `closedCh chan struct{}` created in `New` and closed exactly once when Close sets `closed`. Get selects on `closedCh`, `ctx.Done()`, and `flight.done`. After waking, it rechecks under `mu` in the priority order of rule 7 (ErrClosed, then caller ctx, then load result) so a race between close and load completion prefers `ErrClosed`.

`sync.Cond` is not required if `closedCh` exists. Second `Close` still needs to wait until the first Close has finished `loads.Wait`. Use a `doneClose chan struct{}` that the first Close closes after `loads.Wait` returns and after entries are dropped. A second Close, seeing `closed` already true, waits on `doneClose` (outside `mu`) and returns nil. That satisfies "a second Close arriving while the first is still joining waits for that same join."

Loader goroutine lifetime:

- The goroutine that won the single-flight does `loads.Add(1)` before releasing `mu` and starting the goroutine, and `loads.Done()` when the Loader returns (via `defer`), after publishing the result and after the admit-or-discard critical section.
- Close's `loads.Wait` therefore cannot return until every Loader has returned and its result has been either admitted or discarded. After `Wait`, Close drops `resident` (should already be empty of anything admitted post-cancel; drop anyway), sets `n = 0`, clears `inflight`, closes `doneClose`, unlocks.

No goroutine is left running after `Close` returns, except none: every Loader has returned, and Get waiters have been released by `closedCh` or by `flight.done`.

## Get algorithm

```
Get(ctx, key):
  1. Lock mu.
  2. If closed: unlock, return nil, ErrClosed. Do not call Loader.   // rule 8, late Get
  3. If e, ok := resident[key]: mark MRU (unlink, link before tail); copy := clone(e.val); unlock; return copy, nil.  // rules 1, 4, 6
  4. If f, ok := inflight[key]: unlock; go to wait(f).                // join existing single-flight
  5. Else create f, inflight[key] = f, loads.Add(1), unlock, start loader goroutine, go to wait(f).
```

Wait path (caller is not holding `mu`):

```
wait(f):
  select {
  case <-closedCh:
  case <-ctx.Done():
  case <-f.done:
  }
  Lock mu.
  // Priority (a) > (b) > (c), rechecked under mu so the winner is stable.
  if closed:
    unlock
    return nil, ErrClosed
  if ctx.Err() != nil && f not yet done:   // caller's ctx fired and load still in flight
    unlock
    return nil, ctx.Err()
  unlock
  // Load has resolved (or resolved and was discarded only when closed, which was handled above).
  <-f.done                                 // no-op if already closed
  if f.err != nil:
    return nil, f.err                      // bytes discarded; do not copy
  return clone(f.result), nil
```

The select may wake on `ctx.Done()` in the same instant the load finishes. Recheck under `mu`: if `f.done` is already closed, rule 7(c) applies and the caller gets the result (its ctx being done does not discard a resolved result). If the load is still in flight, rule 7(b) applies and the caller returns `ctx.Err()` without removing the flight. The load continues (orphaned load, rule 7). If `closed` is set, rule 7(a) wins even if the load has also resolved.

Leaving waiters do not delete `inflight[key]` and do not cancel the Loader. Only the Loader goroutine removes the flight, and only after the Loader returns.

## Loader goroutine

```
go func() {
  defer loads.Done()
  b, err := load(root, key)          // root is the cache-scoped context, never the caller's
  if err != nil {
    f.err = err                      // ignore b; do not retain it
    f.result = nil
  } else {
    f.result = clone(b)              // own the bytes before publication
    f.err = nil
  }
  close(f.done)                      // publish to waiters; happens-before their reads of f.result/f.err

  mu.Lock()
  delete(inflight, key)              // always; a later Get must be able to miss or hit, not join a finished flight
  if err == nil && !closed && f.gen == closeGen {
    admit(key, f.result)             // may evict LRU if n == cap
  }
  // else: error, or closed, or stale generation -> discard, do not store
  mu.Unlock()
}()
```

`admit` (caller holds `mu`, `n <= cap`, key not in `resident`):

```
if n == cap {
  evict LRU: victim := head.next; unlink; delete(resident, victim.key); n--
}
e := &entry{key: key, val: stored}   // stored is already a cache-owned copy
resident[key] = e
link before tail                    // mark MRU (rule 6)
n++
```

Eviction runs only at admit time, only among resident entries. In-flight flights are not in the list, so they are not evicted and do not consume a slot. After admit, `n <= cap`. `Len` takes the same mutex, so a concurrent `Len` cannot observe `n` mid-evict above capacity. The "may lag a completion by one critical section" clause is satisfied because `Len` blocks on `mu` until the admit critical section finishes, or it runs just before that section and reports the pre-admit count. It never reports an in-flight key as resident.

If admit finds the key already resident, that is a bug (a second flight was started). The single-flight insert under `mu` prevents it: a key is inserted into `inflight` before the goroutine starts, and a resident key never starts a flight. Still, admit should treat "already resident" as replace-and-mark-MRU only if it can happen; it cannot, so a panic is unnecessary. Do not add a second code path.

Successful admit of a key that was evicted and reloaded is a normal miss: the old entry is gone, a new flight ran, admit inserts a new entry.

## How each rule is satisfied

1. **Hit.** Step 3 of Get: resident key returns `clone(entry.val)`, relinks to MRU, Loader is not called. `TestHitReturnsCopy` checks the call count stays 1.

2. **Single-flight miss.** The transition "absent from resident and absent from inflight" to "present in inflight" happens under `mu` before any Loader starts. Concurrent Gets either see the flight and join it, or one of them creates it. Waiters block on the same `f.done` and each receive `clone(f.result)` or the same `f.err`. `TestSingleFlight` parks 50 Gets inside `synctest`, asserts `calls == 1` while the Loader is still blocked, then releases and checks every waiter got `block:k`.

3. **Error.** Non-nil `err` skips `admit`, deletes the flight, and is returned as-is (not wrapped, so `errors.Is` matches the Loader's error, as `TestErrorNotCached` requires with `errors.Is(err, boom)`). Bytes accompanying an error are dropped. A later Get finds neither resident nor inflight and calls Loader again.

4. **Ownership / copy.** Two copies: Loader slice → `f.result` / `entry.val`, and `entry.val` or `f.result` → each Get return. The cache never stores the Loader's slice and never returns `entry.val` directly. Independent copies mean one waiter's mutation cannot change another's (same rule the public hit test checks for sequential Gets).

5. **Capacity / LRU.** `n` starts at 0 and `admit` evicts `head.next` while `n == cap` before inserting, so `n` never exceeds `cap`. In-flight loads are only in `inflight`. `Len` returns `n` under `mu`. `TestLenBounded` loads 5 keys into capacity 3 and expects `Len() <= 3` after each Get and `Len() == 3` at the end. `TestLRUEviction` is the recency oracle (below).

6. **Recency.** Hit path and `admit` both link the entry before `tail`. `TestLRUEviction` sequence with capacity 2: load `a`, load `b` (LRU = a), hit `a` (LRU = b), load `c` evicts b, hit `a` (still resident, one load), load `b` (second load). Expected Loader counts: a=1, b=2, c=1.

7. **Caller context and orphaned loads.** Loader is invoked as `load(c.root, key)` where `root` is `context.WithCancel(context.Background())` created in `New`, not the Get's `ctx`. A Get whose `ctx` fires while `f.done` is still open returns `ctx.Err()` and does not cancel `root` and does not delete the flight. When the Loader later returns successfully and the cache is still open, `admit` stores the bytes. Priority on wake: `closed` → `ErrClosed`; else own `ctx` done and load not done → `ctx.Err()`; else the load result. A resolved load wins over a done caller ctx so a completion that landed before the waiter notices is delivered (rule 7(c) "once resolved").

8. **Close.** First `Close`: under `mu`, if already `closed`, wait for `doneClose` and return nil. Otherwise set `closed`, bump `closeGen`, `close(closedCh)`, `stop()` to cancel `root`, drop `mu`, `loads.Wait()`, re-lock, clear `resident` / `inflight` / list / `n`, close `doneClose`, unlock, return nil. Idempotent. A Get that observes `closed` before creating a flight returns `ErrClosed` and does not call Loader (`TestClosedReturnsErrClosed`). A Loader returning after `stop()` fails the `!closed && gen` check and is discarded, so a close during an orphaned load does not repopulate the cache. Waiters blocked in `select` wake on `closedCh` and return `ErrClosed`. `errors.Is` is the contract; returning the sentinel directly satisfies it. Close does not wrap.

9. **Concurrency.** All map, list, and flag access is under `mu`. The Loader runs outside `mu`. WaitGroup and channel close provide the Loader/waiter/Close edges. No lock is held while waiting on a caller context, a flight, or `loads.Wait`. `go test -race` is part of the acceptance run. Deadlock avoidance: Close does not need a Get to proceed (it cancels `root` and waits only for Loaders, which must return on `ctx.Done()`); a Get does not need Close to hold a lock it also needs; a waiter never holds `mu` while waiting.

## Edge cases the public suite does not cover (implement anyway)

These follow from the stated rules and are the likely hidden-test surface. The implementation must get them right; extra tests below lock them in.

- **Capacity 1.** Admit of a second key evicts the only resident entry. Hit on the survivor still works.
- **Nil and empty values.** Successful `(nil, nil)` is cached as a nil slice and returned as nil, distinct from an error. Successful `([]byte{}, nil)` is cached as an empty non-nil slice. Error `(nil, err)` and `([]byte{1}, err)` both surface `err` and store nothing. `TestErrorNotCached` already requires nil bytes on error.
- **Duplicate join after error.** Two waiters on a failing load both get the same error; a third Get after `f.done` starts a new flight (flight map entry already deleted).
- **Caller cancel while waiting.** Get returns `context.Canceled` or `context.DeadlineExceeded` via `ctx.Err()`. Loader call count stays 1. A later Get with a live context hits if the orphaned load succeeded, or re-loads if it failed.
- **Caller already canceled before Get.** If the key is resident, a hit still returns the copy (rule 1 does not consult ctx; rule 7's ctx priority applies to a *waiting* Get). If the key is absent, Get must not start a Loader solely to abandon it if we can see `ctx.Err()` before creating a flight — but the spec's priority list is for a waiting Get, and rule 8 only forbids the Loader after close. Starting a load and then immediately returning `ctx.Err()` is legal and matches "load continues even if all waiters leave." Prefer: check `closed` first; if not closed and not resident, join or start the flight even when `ctx` is already done, then the wait path immediately takes branch (b) if the load is not done or branch (c) if a completed flight is somehow visible. Do not skip the Loader on a pre-canceled miss, because a concurrent Get with a live ctx must still single-flight with this one. If this caller is the only one, the orphaned load still populates the cache. That is rule 7, not a leak.
- **Close during in-flight load.** Loader observes `root` done, returns, result discarded, waiters get `ErrClosed`, `Len()` is 0 after Close returns. A Loader that ignores cancel would stall Close; the spec says a Loader must return promptly on `ctx.Done()`, so Close may block until it does. The implementation must not add a timeout that abandons the wait (that would violate "waits for every in-flight Loader call to return").
- **Close with no in-flight loads.** `loads.Wait` returns immediately, entries dropped, `Len()==0`, subsequent Get returns `ErrClosed`.
- **Concurrent Close and Get.** A Get that has passed the closed check and started or joined a flight still returns `ErrClosed` if Close wins the race before delivery (recheck under `mu`). A Get that has already copied a hit out and released `mu` before Close may return the copy; that Get has passed the closed check. That is consistent with rule 8 ("a Get that has not yet passed the closed check").
- **Second Close concurrent with first.** Both return nil; the second blocks on `doneClose` until `loads.Wait` finishes. No double `close` of channels, no double `stop` issues (`context` cancel is idempotent, but only the first Close calls `stop` and closes the channels).
- **Len during in-flight miss.** `Len` does not include the flight. After the successful Loader returns and admit runs, `Len` includes it, still `<= cap`.
- **Eviction of a key that has no flight.** Evicted key's next Get is a fresh miss (one new Loader call), not a join of a stale flight (flight was deleted at the end of the earlier load).
- **Large concurrent distinct keys.** Many keys in flight at once (more than `cap`) are allowed. As they resolve, admit evicts down to `cap`. Order of completion determines which survive; LRU among those admitted follows completion order because each admit marks MRU. No deadlock, no `n > cap`.
- **Do not wrap Loader errors.** Return `f.err` directly so `errors.Is` / `==` against the Loader's error both work unless the Loader itself wrapped.
- **Key space.** Keys are arbitrary strings, including empty string. The empty string is a normal key.
- **Zero-value not used.** Callers must use `New`. Methods on a nil `*cache` are not part of the contract.

## File layout and style

`worktree/blockcache.go` only, plus the existing `go.mod` and public test file. Standard library only: `context`, `errors`, `sync`.

Keep helpers unexported and small: `clone`, `linkMRU`, `unlink`, `evictLRU`, `admit`. Comments only where the rule is non-obvious (generation check, wait priority, why the Loader context is `root`). No `singleflight` package: the std `singleflight` does not provide cache-scoped context, LRU admission, or close-discard, and wrapping it would fight those rules.

Panic text in `New` can be a plain string (`"blockcache: capacity < 1"` / `"blockcache: nil Loader"`). Not part of the contract.

## Test approach

Public suite (already in the worktree; do not modify):

- `TestHitReturnsCopy` — rules 1 and 4.
- `TestSingleFlight` — rule 2, deterministic via `testing/synctest`.
- `TestErrorNotCached` — rule 3.
- `TestLRUEviction` — rules 5 and 6.
- `TestLenBounded` — rule 5.
- `TestClosedReturnsErrClosed` — rule 8, idempotent Close.
- `TestNewPanics` — `New` preconditions.

Run from `worktree/`:

```
go test .
go test -race .
go vet .
```

`go test` must be race-clean and vet-clean. Public tests use a 5s deadline and `t.Fatal` on expiry, so a deadlock fails the test rather than hanging the run. `TestSingleFlight` uses `t.Errorf` (not `Fatalf`) while the 50 goroutines are parked, so the bubble is not torn down early; the implementation does not depend on that, but the wait path must actually block in a `select`/`chan` receive that `synctest` considers durable. Blocking on a channel receive or `select` is durable. Blocking on `mu` is also tracked. Do not busy-spin.

Hidden tests are not in the tree. The implementation is graded against the rules, not only the public suite, and under `-race`. After the public suite is green, add a temporary `blockcache_extra_test.go` (same package) to check the edge cases above, run it with `-race`, then delete it before delivery if the task forbids extra files. The required deliverable is the package; extra tests are a private verification aid, not part of the API. If they stay, they must not change public behavior. Prefer deleting them so the deliverable stays the package plus the shipped public test.

Private checks to run at least once:

- Orphan: start a load, cancel the only waiter, wait until Loader returns, Get again with a fresh context, assert one Loader call and a hit.
- Close during load: Loader blocks on `ctx.Done()`, Close returns, Loader was entered once, result not resident (`Len()==0`, and a post-Close Get is `ErrClosed`).
- Cancel vs success race: many Gets, cancel half, release Loader, canceled waiters that woke before resolve get `ctx.Err()`, waiters that are still waiting get the bytes; a follow-up Get hits.
- `(bytes, err)` discarded: second Get calls Loader again.
- Capacity 1 eviction and empty-string key.
- Two Close calls in parallel while a Loader is blocked on `root.Done()`; both return nil; no race.
- 100 goroutines, 10 distinct keys, capacity 3, mixed hits and misses; `Len()<=3` always; no race.

## Implementation order

1. Types, `New` (panic checks, sentinel list, `root`/`stop`, maps, channels), `clone`, list helpers.
2. `Get` hit path and admit path without concurrency (enough for `TestHitReturnsCopy`, `TestLRUEviction`, `TestLenBounded`, `TestNewPanics`).
3. Single-flight flight map and Loader goroutine (enough for `TestSingleFlight`, `TestErrorNotCached`).
4. Caller-cancel select and orphan-admit.
5. `Close`, `closedCh`, generation discard, idempotent second Close (`TestClosedReturnsErrClosed`).
6. `go test`, `go test -race`, `go vet`, then the private edge checks, then remove the private test file.
