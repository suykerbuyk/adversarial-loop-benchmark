# Implementation plan: `blockcache`

Package `blockcache` (module `blockcache`, Go 1.25). Implementation files live in `worktree/`. Public tests already exist at `worktree/blockcache_public_test.go` (copy of `instrument/resolve_public_test.go`). Do not edit the public test file. Do not implement anything until this plan is accepted.

This revision replaces the previous plan. The `sync.Once` join is withdrawn (previous rebuttal item 3). Point-by-point disposition of the latest findings is in `plan/plan_rebuttal.md`.

There is one specification of each mechanism below. WaitGroup accounting, the waiter decision, Close's join, the store-clone placement, and the publish order under `mu` are each described once. Later sections refer back; they do not restate a second algorithm.

## Public API

The exported declarations match the spec: names, signatures, and the `ErrClosed` string `blockcache: closed`. Doc comments on `Loader`, `Get`, `Len`, `New`, and `ErrClosed` match the spec. `Close`'s doc comment matches the spec and then adds the deadlock sentences below. Those sentences are comments only. They do not change the signature or the required behavior. Nothing else is exported. No dependency beyond the standard library. Do not import `golang.org/x/sync/singleflight` or any other module: its context, error, and lifetime rules do not match this spec.

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
	// Get returns a COPY of the block for key, loading on a miss. Concurrent Gets
	// for the same absent key cause exactly one Loader call; all receive its result.
	Get(ctx context.Context, key string) ([]byte, error)
	// Len is the number of resident entries. Resident-only; it may lag a completion
	// by one critical section and is never above capacity.
	Len() int
	// Close cancels in-flight loads, waits for their Loader calls to return, wakes
	// every waiter with ErrClosed, drops all entries, and returns nil. Idempotent.
	//
	// Close must not be called from a Loader, or from a goroutine that a Loader
	// waits on. Close waits for that Loader to return, so the call deadlocks.
	// The spec does not define this case; the note documents the inherent join.
	Close() error
}

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache

var ErrClosed = errors.New("blockcache: closed")
```

- `New` panics if `capacity < 1` or `load == nil`. Any panic value is acceptable; the public test only checks that `recover()` is non-nil. Use a distinct string for each precondition so the two failures are distinguishable in logs.
- `Get` returns the `ErrClosed` sentinel directly so `errors.Is(err, ErrClosed)` is true. Wrapping is legal; do not wrap.
- `Close` returns `nil` always, including a second call that arrives while the first is still joining, and including a call after the first has returned.
- Capacity is a resident **entry** count, not a byte budget. There is no cap on the number of in-flight loads other than memory. Do not reject a miss because `len(resident) + len(flights) >= capacity`.
- A nil `context.Context` passed to `Get` is a caller bug. Do not special-case it; `ctx.Done()` / `ctx.Err()` will panic, which is the standard library behavior.
- Empty-string keys are ordinary keys. No normalization.

`var _ Cache = (*cache)(nil)` sits next to the concrete type so a signature drift fails at compile time.

## Internal data model

Unexported concrete type `cache` implements `Cache`. `New` returns `*cache`.

```go
type cache struct {
	capacity int
	load     Loader

	// loadCtx is cancelled by the first Close, and only then. Every Loader
	// call receives this context, never a caller's context and never a child
	// that is also cancelled when a caller cancels.
	loadCtx    context.Context
	loadCancel context.CancelFunc

	// joined is the idempotent Close join. Nil until the first Close.
	// That Close creates it under mu, and after wg.Wait closes it once.
	// A later Close copies the pointer under mu and receives outside mu.
	// Not a sync.Once: Once.Do waits on an internal mutex, and
	// testing/synctest does not treat a mutex wait as durably blocked, so
	// a second Close could not be observed with synctest.Wait. A channel
	// receive is durably blocked. See Close.
	joined chan struct{}

	// wg counts Loader goroutines that have been committed and have not
	// finished. Add only under mu, only when !closed, immediately before
	// the critical section that committed the flight unlocks, and always
	// paired with a go statement that defers Done. Wait only from the
	// Close that created joined, after closed is set and mu is released.
	wg sync.WaitGroup

	mu     sync.Mutex // guards the fields below, plus flight.finished/result/err
	closed bool

	// resident maps key -> entry. len(resident) <= capacity at every unlock,
	// including under a concurrent Len. The map is allocated in New and
	// cleared, never niled, so a missed closed-check cannot panic on write.
	resident map[string]*entry

	// head is MRU, tail is LRU. Every resident entry is on this list and
	// every list node is in resident. Recency updates happen only under mu.
	head, tail *entry

	// flights maps key -> in-flight load. A flight is not resident, does not
	// count toward capacity, and is not evictable. At most one flight per key.
	// A key is never both resident and in flights.
	flights map[string]*flight
}

type entry struct {
	key  string
	data []byte // cache-owned, immutable after the entry is linked
	prev *entry
	next *entry
}

type flight struct {
	key string
	// done is closed after finished, result, and err are published under mu.
	// It is a wake-up only. Waiters must not infer priority from which select
	// case fired. The loader closes done exactly once. Close does not close it.
	done chan struct{}
	// finished, result, and err are written under mu before close(done),
	// and read under mu. finished is the source of truth for "resolved".
	finished bool
	result   []byte // cache-owned copy, or nil
	err      error
}
```

No `flight.callers`. No `atomic` inflight counter. No `inflightDone` channel. No `sync.Once`. Orphan continuation does not depend on a waiter count: a flight runs until the Loader returns regardless of how many Gets are still waiting. Close's join is `wg` plus the `joined` channel, not a count of waiters.

`joined != nil` is the same fact as `closed == true`. Both are written in the first Close's critical section before that section unlocks. No code sets one without the other.

### Locking and lifetime (these comments go in `blockcache.go`, above the struct)

- `mu` guards `closed`, `joined`, `resident`, the LRU links, `flights`, and each flight's `finished` / `result` / `err`.
- `flight.result` and `flight.err` are written by the loader goroutine under `mu` before `finished` is set and before `close(done)`. Waiters read them only under `mu`. After publication the result bytes are immutable, so a waiter may snapshot the slice header under `mu` and `clone` outside the lock.
- `entry.data` is immutable once the entry is linked into `resident`. Concurrent readers of those bytes are race-free. Eviction unlinks the entry; it does not mutate the bytes.
- The Loader runs outside `mu`. `mu` is never held across a Loader call, a channel wait (including `<-joined`), or `wg.Wait`. Holding `mu` across `<-joined` deadlocks: the first Close closes `joined` only after `wg.Wait`, and `wg.Wait` returns only after the loader's `publish` takes `mu`.
- `loadCancel` runs only after `closed = true` has been published (written under `mu`, then `mu` unlocked). A goroutine that observes `loadCtx.Done()` and then locks `mu` therefore observes `closed == true`. That implication is what makes a conforming context unable to reach `decide`'s panic branch.
- `wg.Add(1)` happens-before `wg.Wait` for every committed load. Proof: Add runs under `mu` only when `!closed`. The first Close sets `closed` under `mu` and only then, after releasing `mu`, calls `wg.Wait`. If the leader's critical section happens-before Close's, Add has happened. If Close's happens-before the leader's, the leader sees `closed` and does not Add. `sync.WaitGroup` forbids a concurrent Add that Wait does not observe; this mutex order satisfies that rule. Only the Close that created `joined` calls `wg.Wait`.

Copy helper, used for every stored value and every successful return. The nil check is inside `bytes.Clone`; do not add another one. The wrapper exists so the nil/empty contract has one name.

```go
// clone returns a fresh copy. bytes.Clone(nil) is nil. bytes.Clone of an
// empty non-nil slice is empty and non-nil. Callers can tell those apart.
func clone(b []byte) []byte {
	return bytes.Clone(b)
}
```

`nil` stays `nil`. An empty non-nil slice stays empty and non-nil. A successful `(nil, nil)` load is stored and returned as nil. A successful `[]byte{}` is stored and returned as a non-nil empty slice. Callers tell them apart from each other; both are successes (the spec has no not-found error). On any error path the returned slice is nil, not a clone of the Loader's bytes.

## Control flow

### New

1. If `capacity < 1`, panic. If `load == nil`, panic.
2. `loadCtx, loadCancel := context.WithCancel(context.Background())`.
3. Allocate non-nil `resident` and `flights` maps. `closed` is false. `joined` is nil. `wg` is the zero value. LRU head and tail are nil.
4. Return `*cache`.

### Shared waiter decision (the only priority rule)

Rule 7 priority for a **waiting** Get, evaluated after the wait wakes, from state copied under `mu`. Which `select` case fired is irrelevant. A hit is not a waiting Get and does not use this function.

```go
// decide is the only waiter outcome rule.
//   closed                 -> ErrClosed, even if the flight also finished and even if callerErr != nil
//   finished               -> the load result (clone on success, nil slice on error), even if callerErr != nil
//   callerErr != nil       -> callerErr (load still in flight, cache still open)
//   otherwise              -> panic; see below
func decide(closed, finished bool, result []byte, loadErr, callerErr error) ([]byte, error) {
	if closed {
		return nil, ErrClosed
	}
	if finished {
		if loadErr != nil {
			return nil, loadErr
		}
		return clone(result), nil
	}
	if callerErr != nil {
		return nil, callerErr
	}
	panic("blockcache: non-conforming context: Done closed but Err is nil")
}
```

In `wait`, the `select` waits on `flight.done`, `ctx.Done()`, and `loadCtx.Done()`. Go's `select` does not spurious-wake on these. Publication invariants: `finished` is set before `close(done)`, and `closed` is set before `loadCancel`. With those invariants, this panic is reached only when the caller's context is non-conforming: its `Done` channel is closed and `Err()` returns nil. That is a caller bug, not a cache bug. The panic message names it so it is not mistaken for a cache bug. The unit test calls `decide` with those flags directly and expects the panic. A loop-back would busy-spin if `loadCtx` stayed done while `closed` were false, so the branch panics instead of retrying. Do not fall through to `nil, nil`. A future change that adds a wake source, or that cancels `loadCtx` from a parent, must extend `decide` in the same edit.

Do not add a second, lock-free pre-check on an atomic closed flag. `closed` is the bool under `mu`. There is no closed atomic.

### Get

1. Lock `mu`.
2. If `closed`, unlock and return `nil, ErrClosed`. Do not call the Loader. This is the check a Get "has not yet passed" in rule 8. It also covers a Get that arrives while Close is still in `wg.Wait`.
3. If `resident` contains `key`: snapshot `entry.data` (immutable), move the entry to MRU, unlock, return `clone(snapshot), nil`. Do not consult the caller's ctx. A hit is rule 1, not a waiting Get, so an already-cancelled caller ctx still receives the copy.
4. If `flights` contains `key`: snapshot the `*flight`, unlock, go to wait. Do not start a Loader.
5. Otherwise this caller is the single-flight leader. Still holding `mu`, and only because step 2 saw `!closed`:
   - Allocate `flight{key: key, done: make(chan struct{})}`.
   - Insert it into `flights`.
   - `wg.Add(1)`.
   - Unlock.
   - Then `go c.runLoad(f)`.
   - Then wait on the same flight as any joiner.
   The Add, the flight insert, and the closed check are one critical section. Add is never after Unlock. The `go` is after Unlock so `mu` is not held across goroutine startup, and it is unconditional after Add so every Add has a matching `defer wg.Done()`.
6. Wait, lock not held:

```go
func (c *cache) wait(ctx context.Context, f *flight) ([]byte, error) {
	select {
	case <-f.done:
	case <-ctx.Done():
	case <-c.loadCtx.Done():
	}
	c.mu.Lock()
	closed, finished := c.closed, f.finished
	result, loadErr := f.result, f.err
	c.mu.Unlock()
	return decide(closed, finished, result, loadErr, ctx.Err())
}
```

The leader uses this same wait. Its own ctx is not the Loader's ctx. If the leader's ctx is already cancelled when `wait` starts, `select` can take `ctx.Done()` immediately; the Loader still runs (orphan). If the flight has already finished, `decide` returns the result because `finished` is checked before `callerErr`.

A waiter that leaves does not delete the flight and does not cancel `loadCtx`. The loader goroutine owns flight removal and publication.

An already-cancelled caller ctx on a miss still becomes leader or joiner. The load starts (or is joined). `decide` then returns `ctx.Err()` if the flight is still unfinished, or the result if it has finished, or `ErrClosed` if the cache has closed. Do not skip the load merely because `ctx` is already done; that would make "cancel at the door" and "cancel after block" populate differently, and the orphan rule is the one the spec states.

### runLoad

```go
func (c *cache) runLoad(f *flight) {
	defer c.wg.Done()
	data, err := c.load(c.loadCtx, f.key) // cache-scoped context only
	c.publish(f, data, err)
}
```

Do not recover a Loader panic. An unrecovered panic in the loader goroutine crashes the process under ordinary Go rules. `defer wg.Done()` still runs during unwind. The spec requires the Loader to return; surviving a panic is out of scope.

### publish

One store-clone placement: on the nil-error path, `owned := clone(data)` runs **before** `mu` is taken, so a large copy does not block other Gets. `data` is not stored. The closed re-check is after the clone, under `mu`. If the cache closed during the clone, drop `owned` and take the closed branch. Do not clone on the error path. Do not also clone under `mu`.

One order under `mu`, for every terminal branch: delete the flight, set `result` / `err` / `finished`, `close(done)`, unlock. Exactly one terminal branch runs. Each closes `done` exactly once. `publish` never inserts into `flights`.

```go
func (c *cache) publish(f *flight, data []byte, err error) {
	var owned []byte
	if err == nil {
		owned = clone(data) // before mu; the only store clone
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.flights[f.key] == f {
		delete(c.flights, f.key)
	}
	f.finished = true
	if c.closed {
		// drop owned; do not admit. Waiters that see closed return
		// ErrClosed from decide and do not read f.err. Set it anyway
		// so the published flight is self-consistent.
		f.result = nil
		f.err = ErrClosed
		close(f.done)
		return
	}
	if err != nil {
		// includes (non-nil bytes, non-nil err): error wins, bytes discarded
		f.result = nil
		f.err = err
		close(f.done)
		return
	}
	c.admit(f.key, owned)
	f.result = owned
	f.err = nil
	close(f.done)
}
```

`close(done)` does not block. A new Get cannot observe "flight already deleted" and also start a duplicate load before `done` is closed, because both happen before unlock. A waiter that already holds `*flight` wakes, takes `mu`, and sees `finished`.

Checking `loadCtx.Err()` is not a second storage predicate. `closed` is the storage gate, and Close sets it before `loadCancel`.

Admit, caller holds `mu`, cache is open, key is not resident (if it is, that is an invariant break: panic rather than corrupting the list). If `len(resident) >= capacity` and `tail == nil`, panic (list and map disagree) rather than nil-deref.

1. While `len(resident) >= capacity`, evict the tail. Evict only resident entries. Flights are not on the list and are not deleted here.
2. Insert `&entry{key: key, data: owned}` into `resident` and link it as MRU (head).

Evict: unlink `tail`, `delete(resident, e.key)`. After admit, `len(resident) <= capacity`. The intermediate `len == capacity-1` is not visible to `Len` because both steps hold `mu`.

LRU helpers (caller holds `mu`):

- `linkMRU(e)`: `e` is not currently linked. `e.prev = nil`, `e.next = head`. If `head != nil`, `head.prev = e`; else `tail = e`. `head = e`.
- `unlink(e)`: `e` is linked. Splice `e.prev`/`e.next` the usual way, updating `head` if `e.prev == nil` and `tail` if `e.next == nil`. Then nil `e.prev` and `e.next`.
- `touch(e)` on a hit: if `e == head`, done; else `unlink` then `linkMRU`.

The list is a total order. The tail is the unique least-recent entry. The spec allows any least-recent on a tie; this structure does not create equal ranks.

A key is never both resident and in `flights`. Hits return before creating a flight. Admission deletes the flight in the same critical section as the insert. After an error or a discarded close-window result, the flight is gone, so a later Get (on an open cache) starts a new load.

Evicting a resident key does not cancel a flight for a different key.

### Len

Lock `mu`, return `len(resident)`, unlock. Admission and eviction both happen under `mu` and never leave `len(resident) > capacity`, so a concurrent poll cannot observe a value above capacity. `Len` does not count flights. It may miss an admission that has not yet taken `mu` (the allowed one-critical-section lag): the Loader function has returned, but `publish` has not locked yet.

After Close has cleared `resident`, `Len` is 0, including while `wg.Wait` is still blocked.

### Close

One mechanism. A mutex-guarded join channel. No `sync.Once`. No `inflightDone`.

```go
func (c *cache) Close() error {
	c.mu.Lock()
	if c.joined != nil {
		ch := c.joined
		c.mu.Unlock()
		<-ch // outside mu; durable under synctest
		return nil
	}
	ch := make(chan struct{})
	c.joined = ch
	c.closed = true
	clear(c.resident) // map stays non-nil
	c.head, c.tail = nil, nil
	clear(c.flights) // loaders still hold their *flight; they must not re-insert
	c.mu.Unlock()
	c.loadCancel() // after closed is published; must precede Wait
	c.wg.Wait()    // every committed Loader goroutine has returned (defer Done)
	close(ch)      // exactly once, only this goroutine, only after Wait
	return nil
}
```

Why the receive is outside `mu`: the first Close closes `ch` only after `wg.Wait`. `wg.Wait` returns only after `runLoad`'s `defer wg.Done()`, which runs after `publish` takes `mu`. A second Close that received while holding `mu` would deadlock against that `publish`.

Why `loadCancel` precedes `wg.Wait`: a Loader that returns on `ctx.Done()` would never return if Wait ran first, and Close would deadlock. Cancel first, then wait. A Loader that ignores cancel makes Close wait, which is what the spec requires of the cache.

Why this is not the rejected hand-rolled join: `joined` is read and written only under `mu`. Exactly one Close observes `joined == nil`. That Close is the only goroutine that closes the channel, and it closes it once, after `Wait`. `close` of a channel that other goroutines receive from is race-free. A later Close copies the pointer under `mu` and receives outside `mu`. It never closes the channel. There is no second channel to double-close.

Why this matches rule 8:

- The first Close sets `closed`, drops resident entries (`clear`, not `resident = nil`), cancels the cache-scoped context, and waits until every Loader goroutine that passed Add has executed `defer wg.Done()`. `Done` is deferred at the start of `runLoad`, so it runs after `load` returns and after `publish` returns. The channel is closed only after that wait, so a second Close cannot return while a committed Loader is still running.
- Waiters blocked in `select` wake on `loadCtx.Done()`, lock `mu`, see `closed`, and `decide` returns `ErrClosed`. Close does not close `flight.done` and does not wait for waiters. It waits only for Loader returns. Waiters need `mu` only for the short `decide` snapshot; Close is not holding `mu` during `wg.Wait`, so they cannot deadlock against the join.
- A Loader that returns after cancel takes `mu` in `publish`, sees `closed`, sets `f.err = ErrClosed`, does not admit, and closes `done`. The result is discarded. This covers zero-waiter loads and the close window, so "drops all entries" holds: nothing is inserted after `closed` is set, and entries resident at Close were cleared before `loadCancel`.
- A second `Close` while the first is inside `wg.Wait` copies `joined` under `mu` and blocks on `<-ch` until the first Close closes it, then returns nil. It does not call `loadCancel` and does not call `wg.Wait`. A `Close` after the channel is closed receives immediately and returns nil. `<-` on a closed channel returns the zero value; it does not panic.
- A Get that has not passed the closed check (step 2 of Get) returns `ErrClosed` and does not `wg.Add` or call the Loader. That includes a Get that arrives after `closed` is set and before `wg.Wait` returns.
- `loadCancel` is idempotent, but only the first Close calls it.

Interleaving with a leader, the only two orders:

1. Leader holds `mu` first: sees `!closed`, inserts the flight, `wg.Add(1)`, unlocks, then starts `runLoad`. Close later sets `closed` and `wg.Wait` includes that goroutine even if `runLoad` has not entered `load` yet. Close cannot return and then have a Loader start.
2. Close holds `mu` first: sets `closed` and `joined`, unlocks, waits. The leader sees `closed` and returns `ErrClosed` without Add and without `go`.

No path Adds after Unlock. No path Waits before `closed` is set. No path closes `joined` before `wg.Wait` returns.

Calling `Close` from inside a Loader (or from a goroutine that Loader waits on) deadlocks: `wg.Wait` waits for the same Loader. The interface doc comment states this. Do not add a re-entrancy bypass; the spec does not ask for one.

## How each rule is satisfied

1. **Hit.** Get finds `key` in `resident`, returns `clone(entry.data)`, moves the entry to head, does not call the Loader. An already-cancelled caller ctx does not suppress a hit. Covered by `TestHitReturnsCopy` (second Get, `calls == 1`, mutation of the first slice does not change the second).
2. **Single-flight miss.** The leader inserts `flights[key]` under `mu` before `go runLoad`. Later Gets for that key snapshot the same `*flight` and do not Add or call the Loader. Every waiter, including the leader, returns via `decide`, which clones on success. Admission inserts `resident` and deletes the flight in the same `mu` section, before `close(done)`, so a Get that arrives as the load finishes either joins the flight or hits. It does not start a second Loader. Covered by `TestSingleFlight` (50 waiters, `synctest.Wait` while the Loader is blocked, `calls == 1`, equal bytes).
3. **Error.** Non-nil Loader error: `publish`'s error branch. Bytes are not stored and not returned (`nil` slice). The flight is deleted, so a later Get on an open cache loads again. `(non-nil, non-nil)` takes this branch, not the success branch. Covered by `TestErrorNotCached` (`errors.Is` the `boom` sentinel, nil bytes, second Get succeeds, `calls == 2`).
4. **Ownership / copy.** `publish` stores the clone taken before `mu` and does not keep the Loader slice. Every successful Get, hit or coalesced miss, returns a new `clone`. On error, Get returns a nil slice. `TestHitReturnsCopy` mutates the first result. Extra test 12 checks that two coalesced waiters do not share a backing array with each other or with a later hit.
5. **Capacity / LRU.** Only `resident` counts. Insertion evicts the tail until `len(resident) < capacity`, then links the new entry at head, all under `mu`. Flights are absent from the list, so an unresolved load does not affect `Len` and cannot be evicted. `Len` reads `len(resident)` under `mu`, so it is never above capacity, including under a concurrent poll. Lag is the window before `publish` takes `mu`. Covered by `TestLenBounded` and extra tests 8, 9, and 10.
6. **Recency.** Hit calls `touch` (MRU). Successful admit calls `linkMRU`. Eviction removes the tail. `TestLRUEviction`: capacity 2, load `a`, load `b` (a is LRU), hit `a` (b is LRU), load `c` evicts `b`, later Get of `a` does not reload, Get of `b` does.
7. **Caller context and orphaned loads.** The Loader is called with `loadCtx` only. Waiter departure does not cancel `loadCtx`. A success whose waiters have all left still admits, unless `closed` is set. A waiting Get uses `decide`: `ErrClosed` if closed; else the result if `finished`; else `ctx.Err()` if the caller ctx is done; else panic (non-conforming context). A zero-waiter load is still joined by Close via `wg`. There is no per-flight waiter count to get wrong.
8. **Close.** As specified in the Close section: cancel, wait for every committed Loader, wake waiters with `ErrClosed`, drop entries, return nil, idempotent including a second call during the join. A Loader return after cancel is discarded in `publish`'s closed branch. A Get that has not passed the closed check does not call the Loader. Covered by `TestClosedReturnsErrClosed` and extra tests 4, 5, 14, and 16.
9. **Concurrency.** Exported methods take `mu` or use `sync.WaitGroup`, the `joined` channel, and the `done` channel as specified. No wait under `mu`. `go test -race` covers the public suite and the extra tests. `joined` is not read or written outside `mu`. Closing it while another goroutine receives is the channel's own synchronization.

## Edge cases the public suite does not lock

The spec requires these even though the shipped tests do not mention them.

- **Orphaned success.** All waiters cancel while the Loader is in flight. The Loader returns nil error. Bytes are stored. A later Get is a hit (one Loader call total).
- **Orphaned error.** Same shape, Loader returns an error. Nothing stored. A later Get calls the Loader again.
- **One of several waiters cancels.** That waiter gets `ctx.Err()` and a nil slice. Others get the result. The Loader's ctx is still `loadCtx`.
- **Close during an in-flight load.** Close blocks in `wg.Wait` until the Loader returns. Waiters already in `wait` get `ErrClosed` without waiting for the Loader, because `loadCtx` is cancelled before `wg.Wait`. The result is not stored. `Len() == 0` once `clear` has run (before Wait returns). A Get that starts after `closed` is set returns `ErrClosed` and does not call the Loader, even if Close has not returned. The implementation adds no timeout of its own; a Loader that ignores cancel makes Close wait, which is what the spec requires of the cache (and forbids of the Loader).
- **Close with zero waiters and an in-flight load.** Close still waits for that Loader and discards its result.
- **Second Close while the first is inside `wg.Wait`.** Copies `joined` under `mu`, blocks on `<-joined` until the first Close has finished `wg.Wait` and closed the channel, then returns nil. It does not call `wg.Wait` itself.
- **`(non-nil, non-nil)` Loader return.** Error returned to every current waiter, nil slice, key absent.
- **Successful `(nil, nil)` or empty slice.** Stored and returned as a copy, as `clone` defines. `Len` counts the entry.
- **Capacity 1.** A successful load of a second key evicts the only resident entry. A later Get of the evicted key loads again.
- **Hit during an unrelated in-flight load.** The hit does not wait on the other flight and does not evict it (it is not resident).
- **Admission when other keys are in flight and `resident` is full.** Evict LRU residents only. In-flight keys stay in `flights` until their own `publish`.
- **Reload after error.** Flight deletion and `close(done)` happen in the same `mu` section. A new Get either still sees the old flight and waits for its error, or starts a new load. It cannot do both.
- **Get racing the publishing loader for the same key.** Joins the flight, or hits `resident`. Does not start a second Loader.
- **`Len` concurrent with admission.** Never above capacity. May lag by one critical section.
- **Close racing a publish that already passed the Loader call.** Whichever takes `mu` first wins. If publish admits and unlocks before Close, Close's `clear` drops the entry. If Close sets `closed` first, publish discards. A waiter of that flight gets `ErrClosed` if it observes `closed`, or a private copy of the result if it observes `finished` while `!closed`. Both match rule 7 and rule 8: rule 8's "has not yet passed the closed check" does not apply to a waiter that already observed an open cache and a finished flight.
- **Recency tie.** Not observable; the tail is unique.
- **Loader ignores cancel.** Close waits. Tests that cancel use a Loader that returns on `ctx.Done()`, except where a test deliberately holds the Loader to force the join window (those tests release the Loader afterwards).

## Files to write

- `worktree/blockcache.go` — types, invariant comments, `New`, `Get`, `wait`, `decide`, `runLoad`, `publish`, LRU helpers, `clone`, `Len`, `Close`. Exported surface only as in the spec, plus the deadlock sentences on the `Close` doc comment.
- `worktree/blockcache_public_test.go` — already present. Do not edit it to make a test pass. It already declares `mustGet` and `assertPanics`. Do not redeclare them.
- `worktree/blockcache_extra_test.go` — tests below, package `blockcache`.

Source comments required in `blockcache.go` (not only in this plan):

- On `cache`: what `mu` guards, the WaitGroup Add/Wait order, and that `loadCancel` runs only after `closed` is published.
- On `joined`: nil until the first Close; created under `mu`; closed once after `wg.Wait` by that same Close; later Closes receive outside `mu`. State why this is not `sync.Once` (a mutex wait is not durably blocked under `synctest`).
- On `entry`: `data` is immutable after link; callers may snapshot the header and clone outside `mu`.
- On `flight`: `done` is a wake-up; `finished` is the resolution bit; publication order under `mu` is delete flight, set fields, `close(done)`.
- On `Close`: the deadlock if called from a Loader; the second call receives on `joined` and does not call `wg.Wait`; the receive must not run under `mu`.
- On `decide`: the priority order, and that the panic names a non-conforming context (`Done` closed, `Err` nil) so it is not mistaken for a cache bug.
- On `publish`: the store clone runs once, before `mu`, only when `err == nil`.

No other packages. `go.mod` already says `module blockcache` / `go 1.25`. `gofmt` the new files.

## Test approach

Run from `worktree/`:

```sh
go test -count=1 .
go test -race -count=1 .
go vet .
```

Public suite (already written; do not weaken):

- `TestHitReturnsCopy` — rules 1 and 4.
- `TestSingleFlight` — rule 2, 50 goroutines, `testing/synctest`. While the 50 goroutines are parked, the public test uses `t.Errorf`, not `t.Fatalf`, so a failure does not kill the bubble and hide itself as a synctest deadlock. Extra tests that park goroutines must do the same: `t.Errorf` while others are blocked, `t.Fatalf` only after they have been released.
- `TestErrorNotCached` — rule 3.
- `TestLRUEviction` — rules 5 and 6.
- `TestLenBounded` — rule 5.
- `TestClosedReturnsErrClosed` — rule 8 after Close has returned; idempotent Close; `errors.Is`.
- `TestNewPanics` — capacity 0 and nil Loader.

Every extra test uses a 5s context deadline on caller contexts that are not intentionally cancelled. Overlap that must be deterministic uses `testing/synctest`. `synctest.Wait` returns only when every other goroutine in the bubble is durably blocked. Channel send/receive and `WaitGroup.Wait` (after `Add` in the bubble) are durable. A `sync.Mutex` lock, and `sync.Once.Do`'s internal mutex, are not. No extra test calls `synctest.Wait` while a goroutine is blocked on a mutex. Close's second caller blocks on a channel receive, which is durable. That is why Close does not use `sync.Once`.

`TestSingleFlight` note: a fatal failure inside the bubble while goroutines are parked is reported as a bubble deadlock. Extra tests follow the public test's `t.Errorf` rule.

### Unit test of `decide` (single source of truth)

Same package, so it calls unexported `decide`. Setup is the arguments; there is no cache.

| closed | finished | callerErr | assertion |
| --- | --- | --- | --- |
| true | false | nil | `ErrClosed`, nil slice |
| true | false | `context.Canceled` | `ErrClosed`, not `context.Canceled` |
| true | true | `context.Canceled` | `ErrClosed`, even if `result` is non-nil and `loadErr` is nil |
| false | true | `context.Canceled` | clone of `result`, nil error (ready result beats a ctx that is also done) |
| false | true | nil | clone of `result`; mutating the returned slice does not change the input buffer |
| false | true | nil, `loadErr != nil` | nil slice, `loadErr` |
| false | false | `context.Canceled` | `context.Canceled`, nil slice |
| false | false | nil | panics |

This is the test of "close beats caller ctx" and "ready result beats caller ctx". It does not depend on `select` choosing a case. An integration test that cancels the caller and then closes the cache is not used to assert `ErrClosed`, because a correct waiter may observe the cancellation and return `ctx.Err()` before `closed` is set.

### Integration tests

Each item is setup, action, assertion. Lists in the walks are MRU-first: left is head, right is tail.

1. **Orphan success populates.** Setup: capacity >= 1. One `release` channel. The Loader receives on it, then returns `[]byte("v")`. Action: start one Get; `synctest.Wait` until the Loader has entered once and Get is blocked; cancel that caller's ctx; wait until Get returns; close `release`; `synctest.Wait`; Get again with a live ctx. Assertion: the first Get's error is `context.Canceled` and its slice is nil; the second Get returns `v` with a nil error; Loader call count is 1. The second Get is a hit, so the already-closed `release` is not received on again.
2. **Orphan error does not populate.** Setup: capacity >= 1. The Loader's first call receives on `release`, then returns a sentinel error. Later calls return `[]byte("ok"), nil` and do not block. Action: start one Get; `synctest.Wait`; cancel that caller; wait until Get returns; close `release`; `synctest.Wait`; Get with a live ctx. Assertion: the first Get is `context.Canceled` and its slice is nil; the live Get's error is nil and its bytes are `ok`; Loader call count is 2.
3. **Partial cancel.** Setup: Loader blocks on `release`, then returns `[]byte("v")`. Action: two Gets, distinct ctxs; `synctest.Wait`; cancel only the first ctx; wait until the first Get returns; close `release`; wait for the second. Assertion: first error is `ctx.Err()` and the slice is nil; second slice equals `v` and error is nil; call count is 1; a following Get does not call the Loader.
4. **Close joins, discards, and wakes a live waiter.** Setup: Loader blocks on `release` and does not select on its ctx; returns `[]byte("v")` if released. Action: start Get with a ctx that is not cancelled; `synctest.Wait` (Loader entered once, Get blocked); start Close in a goroutine; `synctest.Wait`. Assertion while the Loader is still blocked, using `t.Errorf`: a non-blocking receive on Close's result channel fails (Close has not returned; it is in `wg.Wait`); the Get has returned `ErrClosed` and a nil slice; call count is still 1; `Len() == 0` (clear has run). Action: close `release`. Assertion: Close returns nil; `Len() == 0`; a further Get returns `ErrClosed` and call count stays 1; a second Close returns nil.
5. **Close with no waiters still joins and discards.** Setup: Loader blocks on `release`, ignores ctx, would return `[]byte("v")`. Action: start Get; `synctest.Wait`; cancel the caller; wait until Get returns `context.Canceled`; start Close; `synctest.Wait`. Assertion: a non-blocking receive on Close's result channel fails; call count is 1. Action: close `release`. Assertion: Close returns nil; `Len() == 0`; a further Get on this cache returns `ErrClosed`; call count stays 1. The payload is not observable.
6. **Error with bytes.** Setup: Loader returns `[]byte("nope"), sentinel` on the first call and `[]byte("ok"), nil` on the second. Action: Get twice. Assertion: first returns nil slice and `errors.Is` the sentinel; second returns `ok`; call count is 2.
7. **Nil success and empty success.** Setup: one cache whose Loader returns `nil, nil`; another whose Loader returns `[]byte{}, nil`. Action: Get twice on each. Assertion: nil case returns `b == nil`, `err == nil`, call count 1, `Len() == 1`. Empty case returns `b != nil && len(b) == 0` both times, call count 1, `Len() == 1`.
8. **Capacity 1.** Setup: capacity 1. Loader returns `v:`+key and does not block. Action: Get `a`, Get `b`, Get `a`, Get `b`. Walk, MRU-first: Get `a` admits `[a]` (a=1); Get `b` evicts `a`, admits `[b]` (b=1); Get `a` evicts `b`, admits `[a]` (a=2); Get `b` evicts `a`, admits `[b]` (b=2). Assertion: `a == 2`, `b == 2`, final `Len() == 1`. Both keys are reloaded. `b == 1` would be wrong: step 3 evicts `b`, and step 4 loads it again.
9. **In-flight does not consume capacity and is not evictable.** Setup: capacity 1. Two keys. Before any Get, create one release channel per key. The Loader increments that key's count under a mutex, then receives on that key's channel, then returns `v:`+key. The same channel is used for every load of that key. Do not allocate a new gate inside the Loader. Action: start Get `a` and Get `b`; `synctest.Wait`. Assertion: both Loaders have been entered once (if `b` were refused for capacity, this fails); `Len() == 0`. Action: close `a`'s release; wait until `a`'s Get returns. Assertion: `Len() == 1`; `b`'s Loader has not returned; count `a` is 1. Action: close `b`'s release; wait until `b`'s Get returns. Assertion: `Len() == 1`; count `b` is 1. Action, in this order: Get `b`, then Get `a`. Assertion: Get `b` is a hit, count `b` stays 1, `Len() == 1`; Get `a` calls the Loader again, count `a` becomes 2, count `b` stays 1, `Len() == 1`. `a`'s release is already closed, so that reload's receive returns immediately and the test does not block. Probing `a` first would admit `a` and evict `b`, and the following Get of `b` would reload; that order fails a correct cache. The order above is the proof that `b`'s admission evicted `a`, and that `b` was not dropped while it was only in flight.
10. **Longer LRU sequence.** Setup: capacity 3. Loader returns `v:`+key, counts under a mutex, does not block. Action: Get `a`, `b`, `c`, `a`, `d`, `c`, `e`, in that order. Walk, MRU-first:
    - Get `a`: miss, admit. `[a]`. a=1.
    - Get `b`: miss, admit. `[b, a]`. b=1.
    - Get `c`: miss, admit. `[c, b, a]`. c=1.
    - Get `a`: hit, touch. `[a, c, b]`. a=1.
    - Get `d`: miss, evict tail `b`, admit. `[d, a, c]`. d=1.
    - Get `c`: hit, touch. `[c, d, a]`. c=1.
    - Get `e`: miss, evict tail `a`, admit. `[e, c, d]`. e=1.
    After this action the resident set is `{c, d, e}` and the absent set is `{a, b}`. Counts are all 1. `Len() == 3`. Action: probe only the resident keys, LRU-first, so each is a hit and none is evicted: Get `d`, Get `c`, Get `e`. Assertion: counts still all 1; `Len() == 3`; each returned slice equals `v:`+key. Action: Get `a`, then Get `b`. Do not Get `c`, `d`, or `e` again after Get `a`. Those later Gets would miss, because Get `a` evicts `d` and Get `b` evicts `c`, and the counts would no longer be the survivor counts. Assertion: `a == 2`, `b == 2`, `c == 1`, `d == 1`, `e == 1`, `Len() == 3`.
11. **Distinct backing arrays.** Setup: one successful load. Action: two sequential Gets; mutate index 0 of the first. Assertion: second slice unchanged. Also mutate the second and Get a third time; the third matches the Loader bytes.
12. **Coalesced waiters each get a private copy.** Setup: Loader blocks, then returns `[]byte("payload")`. Action: two concurrent Gets; release; after both return, mutate index 0 of the first result; Get a third time. Assertion: the two concurrent results were equal before mutation; after mutation the second still equals `payload`; the third equals `payload`; call count is 1.
13. **Concurrent Close on an idle cache.** Setup: `New` with a Loader that fails the test if called. Action: several goroutines call Close. Assertion: all return nil; a following Get returns `ErrClosed`; the Loader was not called. This does not by itself prove the second Close blocks during the join; test 16 does. On an idle cache `wg.Wait` returns immediately, so this test must not use `synctest.Wait` to observe a blocked Close.
14. **Get during the join window.** Setup: Loader blocks on `release`, ignores ctx. Action: start one Get so a Loader is in flight; `synctest.Wait`; start Close; `synctest.Wait` so Close is blocked in `wg.Wait` (a WaitGroup wait, which is durable) and the first Get has already been woken with `ErrClosed`; start a second Get with a live ctx while Close has not returned. Assertion: the second Get returns `ErrClosed` and a nil slice before `release` is closed; Loader call count is 1 (the second Get did not start a load); a non-blocking receive shows Close is still blocked; `Len() == 0`. Action: close `release`. Assertion: Close returns nil.
15. **Ready result versus a ctx that becomes done only after resolution** is the `decide` row `finished && callerErr != nil && !closed`, not a racy `select` test. No integration test asserts a single winner when both `ctx.Done()` and a still-blocked Loader are released in the same synctest step: a correct implementation may return either `ctx.Err()` or the payload, and a test that requires `ErrClosed` in that interleaving is wrong. Do not write that test.
16. **Second Close blocks while the first is joining.** Setup: Loader blocks on `release`, ignores ctx. The whole test runs inside `synctest.Test`, so the leader's `wg.Add` happens in the bubble. Action: start a Get so `wg` is non-zero; `synctest.Wait`; start Close #1; `synctest.Wait`. Assertion, with `t.Errorf`: a non-blocking receive shows Close #1 has not returned (it is in `wg.Wait`). Action: start Close #2; `synctest.Wait`. Assertion, with `t.Errorf`: neither Close has returned. Close #1 is durably blocked in `wg.Wait`. Close #2 is durably blocked in `<-joined`, not in `sync.Once.Do`. Action: close `release`. Assertion: blocking receives on both result channels yield nil. Use `t.Errorf` while they are parked. Do not call `synctest.Wait` in a way that requires a goroutine inside `sync.Once` or blocked on `mu`; this test has no such goroutine.
17. **Stress.** Many goroutines, many keys, capacity smaller than the key count, Close racing at the end, `Len` sampled whenever a Get returns. Bound with a `WaitGroup` and the 5s deadline. Assertion: no deadlock; every error is nil, `ErrClosed`, or the Loader's error; every sampled `Len` is `<= capacity`; `-race` is clean. This test does not use synctest.

## Implementation order

1. Write `blockcache.go` with the comments, `New`, `clone`, LRU helpers, `decide`, Get (hit, join, lead), `runLoad` / `publish`, `Len`, `Close`.
2. `go test` the public suite. The likely failures are a WaitGroup Add outside the closed check, or `mu` held across `wg.Wait` or `<-joined`.
3. Add `blockcache_extra_test.go` and run `go test -race` and `go vet`.
4. Do not change the public API or the public test file to make a failing test pass.

## Invariants checklist

- Exported names: `Loader`, `Cache`, `New`, `ErrClosed`, and the three methods. No others. Doc comments match the spec except the deadlock sentences on `Close`.
- Loader context is `loadCtx`, never `Get`'s `ctx`.
- At most one flight and at most one in-flight Loader call per key.
- `wg.Add(1)` only under `mu`, only when `!closed`, in the same critical section as the flight insert, before Unlock, before `go runLoad`.
- `wg.Wait` only from the Close that created `joined`, after `closed` is set and `mu` is released, and before `close(joined)`.
- First Close creates `joined` under `mu` when it is nil, in the same critical section as `closed = true`. Later Closes copy it under `mu`, receive outside `mu`, and return nil. They do not call `loadCancel` or `wg.Wait`. No `sync.Once`.
- `joined` is closed exactly once, by the Close that created it, after `wg.Wait`.
- `len(resident) <= capacity` at every unlock. `resident` is cleared, never niled.
- Flights are not in `resident` and not on the LRU list. No waiter-count field.
- Store clone: once, in `publish`, before `mu`, only when the Loader error is nil. No second placement.
- Publish order under `mu`: delete flight, set `result` / `err` / `finished`, `close(done)`, unlock.
- Cache never retains the Loader's slice.
- Every returned success slice is a fresh clone (or nil). Every error return has a nil slice.
- Non-nil Loader error is not stored. `(non-nil, non-nil)` is an error.
- Close-window `publish` sets `f.err = ErrClosed` and does not admit.
- Waiter outcome goes through `decide` only. No atomic closed flag. No second priority order.
- `decide`'s last branch panics. The message names a non-conforming context.
- No mutex held while calling the Loader, waiting on a channel, or waiting on `wg`.
- `clone` is `bytes.Clone` with no extra nil check.
- Doc comments for the deadlock, the lock set, the join channel, and the publication protocol live in `blockcache.go`.
