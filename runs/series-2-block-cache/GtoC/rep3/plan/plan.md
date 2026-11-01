# Implementation plan: `blockcache`

Package `blockcache` is a bounded, concurrent, single-flight block cache in front of a slow backing store. This plan is the implementation contract. No code is written in this step.

Module: `blockcache` (Go 1.25), files under the worktree. Public tests live in `instrument/resolve_public_test.go` and are copied into the package as `blockcache_public_test.go` at grade time. The implementation must compile against that file with no signature changes.

There is one wait algorithm, one load-goroutine shape, and one Close sequence. Do not keep a second sketch.

## Public API

Ship exactly the surface in the spec. Do not export anything else. In `cache.go`, the spec doc comments appear verbatim, then the appended paragraphs below. Do not drop or rewrite the spec sentences.

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
	Close() error
}

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache

var ErrClosed = errors.New("blockcache: closed")
```

Append after the verbatim `Loader` comment, without changing the spec sentences:

> A Loader must not call Get, Len, or Close on the same Cache. Re-entry deadlocks: Close waits for the Loader goroutine, and Get for the in-flight key waits on the flight that Loader is still running. This is a package contract, not a supported mode. Do not "fix" re-entry by dropping the WaitGroup wait or by skipping single-flight.

Append after the verbatim `Get` comment, without changing the spec sentences:

> Implementation choice, not a spec requirement: if ctx is already Done and the key is neither resident nor in flight, Get returns ctx.Err() and does not call Loader. The spec does not settle this case. An in-flight load is still joined. Do not flip this choice as a bug fix.

`New` panics if `capacity < 1` or `load == nil`. The panic value is unspecified; tests only require a panic. `Close` always returns `nil`. Callers detect a closed cache with `errors.Is(err, ErrClosed)`. Wrapping `ErrClosed` is legal; the implementation returns the sentinel itself.

`Get` never returns the cache's own slice. On any Loader error, including a recovered panic or a Loader that exits without returning, `Get` returns a nil byte slice and that error. `TestErrorNotCached` requires nil bytes when the Loader returns `(nil, err)`. The same nil-bytes rule applies when the Loader returns non-nil bytes together with a non-nil error: the error wins and the bytes are discarded.

A nil `Get` context is the caller's bug. The implementation does not rewrite it and does not document a nil-ctx mode. Public tests always pass a non-nil context. A nil context panics in the standard library when `Done` or `Err` is used. The wait loop relies on the context contract that a closed `Done` channel means `Err` is non-nil.

## Internal data model

One unexported concrete type, `*cache`, is the only `Cache` implementation. `New` returns it as the interface. A compile-time check `var _ Cache = (*cache)(nil)` is fine and is not exported.

```go
type cache struct {
	mu       sync.Mutex
	capacity int
	load     Loader

	// Cache lifetime. Derived from context.Background() inside New.
	// Cancelled only by the first Close, and only after closed is set.
	// Never a caller's context, and never cancelled because waiters left.
	ctx    context.Context
	cancel context.CancelFunc

	// Set in the same critical section as cancel and the entry drop, by the
	// first Close only. One flag is enough: it means "closed or closing".
	// There is no separate joining flag.
	closed bool
	// Closed exactly once, by the Close that performed the join, after every
	// in-flight Loader has returned. Named joined so it is not confused with
	// flight.done.
	joined chan struct{}

	// Incremented under mu when a flight is inserted, before the load goroutine
	// starts. Decremented by that goroutine's outer defer after the finisher.
	// Close waits on this after cancel, without holding mu.
	wg sync.WaitGroup

	// Resident entries only. len(entries) <= capacity always, under mu.
	// Never pre-sized to capacity. See New.
	entries map[string]*entry
	// Doubly linked LRU list. head is MRU, tail is LRU.
	// head == nil iff tail == nil iff len(entries) == 0.
	head, tail *entry

	// In-flight misses. Not resident, not counted by Len, not evictable.
	// Disjoint from entries.
	inflight map[string]*flight
}

type entry struct {
	key  string
	data []byte // cache-owned copy; never returned to callers
	prev *entry // toward MRU; nil at head
	next *entry // toward LRU; nil at tail
}

type flight struct {
	key string
	// Closed after data/err are published and the flight is removed from inflight.
	// Closed exactly once, under mu, in the same critical section that sets published.
	done chan struct{}
	// Under mu. True only after data/err are stored and immediately before close(done).
	// Readers do not use this flag. It exists so the finisher can retry publish
	// without closing done twice.
	published bool
	// Written by the load goroutine before close(done), under mu.
	// Read only after the reader has observed done closed.
	// That receive is the happens-before edge. Do not add a parallel resolved
	// flag that is read without that edge or without mu.
	data []byte
	err  error
}
```

`New`:

- Panic if `capacity < 1` or `load == nil`.
- `ctx, cancel := context.WithCancel(context.Background())`.
- `joined: make(chan struct{})`.
- `entries: make(map[string]*entry)` with **no size hint**.
- `inflight: make(map[string]*flight)` with **no size hint**.
- Do not start a background goroutine.

`New(math.MaxInt, load)` is valid. `make(map[string]*entry, capacity)` would allocate a huge bucket table or panic. No capacity hint, and no `min(capacity, N)` hint either. The maps start empty.

### Ownership of bytes

`clone` preserves nil versus empty and returns a slice whose cap equals its len, so a caller `append` cannot overwrite cache storage:

```go
func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
```

Decisions:

- A nil successful payload is stored as nil. `Get` returns `nil, nil`. It still occupies one resident entry.
- A non-nil payload, including a non-nil empty slice, is stored as a new backing array of the same length. `Get` returns another fresh copy of that length. Two concurrent hits do not share a backing array.
- On Loader error, discard any accompanying bytes before they are stored. Do not store an entry. Publish `err` and a nil `data` on the flight so every waiter returns `(nil, err)`.
- Immediately after `load` returns, the load goroutine either drops the Loader slice (error) or `clone`s it (success) before taking `mu` and before any other cache work. `publish` stores that already-cloned slice, or nil. No field retains the Loader's array. A local variable that dies when the load goroutine returns is not retention.
- `flight.data` and `entry.data` may alias each other. Both are cache-owned. Neither is returned.
- Hit path: under `mu`, copy the slice header out of the entry, unlock, then `clone`. Entry bytes are immutable after publish, and the header read is synchronized by `mu` with the publish that wrote them, so the clone outside the lock is race-free. Cloning outside the lock also means an allocation failure cannot leave `mu` held.

### LRU list

Intrusive doubly linked list, guarded by `mu`. Helpers are nil-safe and do not panic.

- `touch(e)`: if `e` is already `head`, return. Otherwise unlink `e` and insert it at `head` (MRU). If it was not linked (`prev`/`next` nil and `e != head`), unlink is a no-op. Used on hit and on successful admit.
- `evictLRU()`: if `tail == nil`, return. Otherwise unlink `tail` and `delete(entries, tail.key)`. Called only when `len(entries) == capacity` and a new successful entry must be admitted. Ties: the tail is one least-recent entry; any least-recent one is acceptable. The list is a total order, so the tail is well-defined.
- An in-flight key is absent from `entries` and from the list, so it cannot be the eviction victim.
- Eviction of key `K` does not cancel a later in-flight reload of `K`. Resident and in-flight are different map slots. A key is never in both maps at unlock.

List invariant, restored before every unlock: `head == nil` iff `tail == nil` iff `len(entries) == 0`; every `entries` value is on the list once; walking `next` from `head` reaches `tail`.

### Single-flight record

`inflight[key]` is created under `mu` by the `Get` that observes a miss, no existing flight, and a caller ctx that is not already Done. That `Get` starts exactly one cache-owned goroutine and then waits the same way as every follower. The goroutine is the only caller of `load`. No `Get` goroutine runs the Loader.

`wg.Add(1)` happens under `mu`, in the same critical section as the `inflight` insert, before the goroutine is started. The goroutine's first action is not `Add`. `Close` cannot pass `wg.Wait` before the new goroutine has accounted for itself.

The load goroutine runs `load(c.ctx, key)` outside `mu`. It must not hold `mu` across the Loader call.

### Publication

`publish` is called only from the load goroutine. It does not allocate. The success slice passed in is already a clone, or nil.

```go
func (c *cache) publish(f *flight, data []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.published {
		return
	}
	delete(c.inflight, f.key)
	if c.closed || err != nil {
		f.data = nil
		f.err = err // nil data; bytes already discarded by the caller
		// closed: do not insert. Waiters that observe closed return ErrClosed
		// and do not read f.err. A consistent snapshot cannot see
		// resolved && !closed when this branch discarded because of closed.
	} else if e, ok := c.entries[f.key]; ok {
		// Retry after a panic that inserted but did not set published.
		// Do not evict again.
		c.touch(e)
		f.data = e.data
		f.err = nil
	} else {
		if len(c.entries) == c.capacity {
			c.evictLRU()
		}
		e := &entry{key: f.key, data: data}
		c.entries[f.key] = e
		c.touch(e)
		f.data = data
		f.err = nil
	}
	f.published = true
	close(f.done)
}
```

`defer mu.Unlock()` is mandatory. A panic inside `publish` must not leave `mu` held, or the finisher's second `publish` would lock `mu` on the same goroutine and deadlock every later `Get`, `Len`, and `Close`.

The entry allocation above can in theory panic. That is why the retry branch reuses an entry that a previous attempt already inserted, and why `evictLRU` / `touch` / `unlink` do not themselves panic. Straight-line pointer and map writes between evict and `published = true` are not expected to panic. `published = true` and `close(f.done)` are adjacent and are the commit. A second `publish` sees `published` and does not close `done` again.

Removal from `inflight` and `close(done)` happen in the same critical section, before unlock. A new `Get` either joins the still-present flight or observes the resident entry (success) or a fresh miss (error or discarded). There is no gap in which the key is neither in-flight nor resolved.

Do not call `Get`, `Len`, or `Close` from `publish`.

### Load goroutine (one shape)

This is the only goroutine shape. Do not also leave a two-defer recover sketch in the implementation.

```go
func (c *cache) startLoad(f *flight) {
	go func() {
		defer c.wg.Done()
		defer func() {
			rec := recover()
			if !c.isPublished(f) {
				c.publish(f, nil, newLoaderFailure(rec))
			}
		}()
		data, err := c.load(c.ctx, f.key)
		if err != nil {
			c.publish(f, nil, err) // drop the Loader slice; do not store it
			return
		}
		cloned := clone(data)
		c.publish(f, cloned, nil)
	}()
}
```

`wg.Done` is the outer defer (registered first, so it runs last). It runs if the finisher's `publish` panics, so `Close` cannot stick on the WaitGroup because the failure path itself failed. The inner defer is the only finisher.

`isPublished` reads `f.published` under `mu`. Only this goroutine publishes, so the check-then-publish gap is not a logic race. The lock is there so the race detector sees the flag.

Finisher behavior:

- Normal return: `publish` has set `published`. `recover()` returns nil. Finisher does not publish again. `wg.Done` runs.
- Loader panic: `recover()` returns the value. `published` is false. Finisher publishes `(nil, loaderFailure)`. `wg.Done` runs. The process does not die.
- `runtime.Goexit` (including `t.FailNow` / `t.Fatal` inside a Loader): deferred functions still run, and `recover()` returns nil. `published` is false, so the finisher publishes `(nil, loaderFailure)` with kind "exited without returning", closes `done`, removes `inflight[key]`, then `wg.Done` runs. The flight does not stick. Later `Get` reloads. `Close`'s post-wait inflight check is not failed by Goexit.
- Panic inside `publish` before `published` is set: `publish`'s `defer Unlock` runs, the finisher recovers, and a second `publish` delivers the panic error. This is why unlock is deferred.
- Re-panic is rejected. It would kill the process, and rule 9 forbids a deadlock from one bad Loader. The spec does not require propagating the panic.

`newLoaderFailure` builds an unexported error. Do not use `fmt.Errorf("%v", rec)` alone.

```go
type loaderFailure struct {
	exited bool // true if recover() was nil: Goexit or return without publish
	val    any  // panic value; nil on exit-without-return
	stack  []byte
}

func (e *loaderFailure) Error() string {
	if e.exited {
		return fmt.Sprintf("blockcache: loader exited without returning\n%s", e.stack)
	}
	return fmt.Sprintf("blockcache: loader panic: %v\n%s", e.val, e.stack)
}
```

`debug.Stack()` is captured in the finisher and stored in `stack`. The panic value is stored in `val`. Both appear in `Error()` so a swallowed Loader panic can be diagnosed. The type is not a sentinel and is not exported. `Unwrap` is omitted so `errors.Is` does not accidentally match a value inside the panic. Waiters only require a non-nil error and nil bytes. A later `Get` misses and calls `load` again.

Imports used by this path: `fmt`, `runtime/debug`. No third-party packages. Do not import `golang.org/x/sync/singleflight`.

### Closed state and join

`joined` is closed exactly once, by the `Close` that performs the join, after every load goroutine that was accounted in the WaitGroup has returned.

Normative Close sequence:

1. Lock `mu`.
2. If `closed` is already true, unlock and wait on `joined`, return nil. This is a second `Close` during the join and a second `Close` after the join (`joined` already closed, so the wait returns immediately). It does not call `wg.Wait` itself and does not close `joined`.
3. Set `closed = true`.
4. Drop entries in this same critical section: `entries = make(map[string]*entry)` (no size hint), `head = nil`, `tail = nil`. `Len` becomes 0 as soon as this section unlocks, including while Loaders are still running. "Drops all entries" holds from this moment, not only after `wg.Wait`.
5. `cancel()` the cache context. This is after `closed` is set, still under `mu`. Waiters blocked in `select` on `c.ctx.Done()` wake. The Loader is required to return promptly once `c.ctx` is Done.
6. Unlock. Do not hold `mu` across the wait. `cancel` does not call back into the cache. A Loader unblocked by `cancel` must not re-enter this cache; if it publishes, it blocks on `mu` until step 6, then sees `closed` and discards.
7. `wg.Wait()` until every load goroutine has finished its finisher and `wg.Done()`.
8. Lock `mu`. Invariant: `inflight` is empty and `entries` is empty, because every finisher publishes (which deletes the flight) before `wg.Done`, and `publish` does not insert when `closed` is set. Action if the invariant is broken, so the check is not a comment with no consequence: do not panic and do not block. For each remaining flight, if `!published`, set nil data, nil err, `published = true`, and `close(done)` so a waiter cannot stick; then `inflight = make(map[string]*flight)` and again set `entries` empty, `head` and `tail` nil. This branch is unreachable if the finisher contract holds. It runs only after `wg.Wait`, so the load goroutine is not concurrently closing `done`.
9. Unlock.
10. `close(joined)`.
11. Return nil.

`publish` after step 3 always takes the discard branch, so nothing repopulates `entries` during `wg.Wait`. Clearing at step 4 is the chosen order. Do not leave entries in place until after `wg.Wait`.

There is no waiter list. Waking waiters is `c.ctx` cancel plus `close(flight.done)`. A waiter in `select` wakes on `<-c.ctx.Done()` even if its Loader has not returned yet, then observes `closed` and returns `ErrClosed`. `Close` waits for Loaders, not for waiter goroutines to be scheduled. Tests that want waiter results must join those goroutines themselves.

`Len` reads `len(entries)` under `mu` and returns. It does not consult `closed`. It never observes a value above `capacity` because admission evicts under the same lock before insert, and readers take that lock. It may miss an admission that has not yet taken the lock, which is the allowed one-critical-section lag. In-flight keys are not in `entries`. After Close's first critical section, `Len` is 0.

## Get control flow

There is one wait algorithm. It is the recheck loop below. Do not use a `select` whose case identity decides the result. Do not add a `default` case.

```text
Get(ctx, key):
  lock mu
  if closed:
    unlock
    return nil, ErrClosed          // has not passed the closed check; no Loader
  if e, ok := entries[key]:
    touch(e)                       // hit marks MRU
    header = e.data               // header only
    unlock
    return clone(header), nil      // no Loader
  if f, ok := inflight[key]:
    unlock
    return wait(ctx, f)            // follower, including an already-Done caller
  if err := ctx.Err(); err != nil:
    unlock
    return nil, err                // choice: cold key, already Done, do not call Loader
  f = &flight{key: key, done: make(chan struct{})}
  inflight[key] = f
  wg.Add(1)
  unlock
  startLoad(f)                     // cache-owned goroutine
  return wait(ctx, f)              // starter uses the same wait path
```

The closed check, the already-Done cold check, `wg.Add`, and the `inflight` insert are one critical section. `Close` cannot set `closed` between the check and the insert. The goroutine starts only after that section unlocks. `Close` may run between unlock and `go`, but `wg.Add` has already happened, so `wg.Wait` cannot return until that goroutine finishes. `publish` then sees `closed` and discards.

### Already-done caller context

Implementation choice, documented on `Get`. The spec's "a load continues even if all its waiters leave" does not settle a Get that was already cancelled before it became a waiter. This package chooses:

- If `ctx` is already Done and the key is neither resident nor in flight, `Get` returns `ctx.Err()` and does not call the Loader. A cancelled cold Get does not inflate Loader call counts.
- If `ctx` is already Done and a flight exists, `Get` joins that flight and `wait` applies the priority below. It does not start a second Loader call. Usual result: `ctx.Err()`, unless `Close` has won, in which case `ErrClosed`.
- If `ctx` becomes Done after the caller has entered `wait`, and the load is still in flight, `wait` returns `ctx.Err()` (subject to the priority recheck). The load goroutine keeps running. Success still populates the cache when `closed` is false. The `Get` that started the flight is a waiter, not the thread running `load`.

Do not describe this cold-key choice as a spec requirement.

### Waiter priority (rule 7)

A waiting `Get` returns, in priority:

1. `ErrClosed` if the cache is closed.
2. Else the load's `([]byte, error)` once resolved, each success as a fresh clone. This is rule 7(c). It beats 7(b) because 7(b) applies only while the load is still in flight.
3. Else `ctx.Err()` if its own ctx is Done while the load is still in flight. This branch returns. It does not `select` again.

`select` chooses randomly when several channels are ready. Case identity is not the decision. After every wake, re-read state. The normative loop, including the exit that the previous sketch omitted:

```text
wait(ctx, f):
  for {
    closedNow, resolved := snapshot(f)
    if closedNow:
      return nil, ErrClosed
    if resolved:
      return flightResult(f)          // 7(c); happens-before via done
    if err := ctx.Err(); err != nil:
      // Required exit. Without it, <-ctx.Done() is always ready, select
      // returns immediately, and this loop spins, taking mu forever.
      // One re-snapshot so a Close or resolve that landed after the first
      // snapshot still wins (7(a) and 7(c) over 7(b)). Then return.
      // This path always returns. It does not loop.
      closedNow, resolved = snapshot(f)
      if closedNow:
        return nil, ErrClosed
      if resolved:
        return flightResult(f)
      return nil, err
    select {                          // reached only when not closed,
    case <-ctx.Done():                // not resolved, and ctx.Err() == nil
    case <-f.done:                    // no default
    case <-c.ctx.Done():
    }
  }

flightResult(f):                      // caller has observed f.done closed
  if f.err != nil:
    return nil, f.err
  return clone(f.data), nil
```

`snapshot` holds `mu` for the whole observation. It copies `c.closed` and reports whether `f.done` is closed via a non-blocking receive. It does not read `f.data` or `f.err`.

Sampling `closed` and `done` together is required. `publish` closes `done` under `mu` after deciding to discard because `closed` is set. If `snapshot` copied `closed == false` and then observed `done` outside the lock, it could report resolved and not closed after a discard, and `wait` would return `(nil, nil)` instead of `ErrClosed`.

```text
snapshot(f):
  lock mu
  closedNow = c.closed
  select {
  case <-f.done:
    resolved = true
  default:
  }
  unlock
  return closedNow, resolved
```

A non-blocking receive on a `chan struct{}` that has been closed can be repeated. It is not a one-value handoff. Holding `mu` during that receive does not deadlock: `publish` closes `done` while holding `mu` and does not wait for waiters, and `snapshot` does not block.

`flightResult` reads `f.data` and `f.err` only after `snapshot` has observed `done` closed in this goroutine. That receive synchronizes-with `close(done)`, which happens after the stores. Do not introduce an atomic resolved flag.

Outcomes:

- Close during wait returns `ErrClosed`, even if the caller ctx is also cancelled, even if the Loader later succeeds. The result is discarded. `closed` forces `ErrClosed` even when `f.err` is nil.
- Caller ctx done, load still in flight, cache still open after the re-snapshot: return `ctx.Err()`. The flight stays in `inflight` until the load goroutine publishes. Success still populates the cache. The goroutine does not spin and does not take `mu` again.
- Load resolved, cache open: each waiter gets its own copy, or the same error and nil bytes.
- Load resolved and cache closed, observed in one snapshot: `ErrClosed` wins. The publish path also refused to insert, or Close's first critical section already dropped the entry.

Do not unregister a follower on caller cancel. The load goroutine is the only party that removes `inflight[key]`. Followers and the starter hold only the `*flight` pointer.

The `select` is reached only when the cache is open, the flight is unresolved, and `ctx.Err()` is nil. A standard context therefore does not make `<-ctx.Done()` ready on that iteration. `<-c.ctx.Done()` is ready only after `cancel`, which happens after `closed` is set, so the next loop returns `ErrClosed` instead of spinning.

### Error with bytes (rule 3)

If `load` returns a non-nil error, the goroutine passes nil data into `publish` and drops the Loader slice. No entry. A later `Get` finds neither `entries` nor `inflight` and starts a new Loader call.

### Orphaned load (rule 7)

If every waiter, including the starter, returns `ctx.Err()` while the load is in flight, the cache-owned goroutine is still inside `load` or `publish`. On success with `closed == false`, `publish` inserts the entry and removes the `inflight` slot. That goroutine then exits. The in-flight record is not a leak. A later `Get` hits and does not call `load` again.

The starter can leave as soon as its own ctx is Done. It is not stuck inside `load`.

### Second Close

`closed == true` covers both "join in progress" and "join finished". The second `Close` waits on `joined` and returns nil. `Close` never returns a non-nil error. Concurrent `Close` callers: one sets `closed` and joins; the others wait on the same `joined`.

### Len lag

`Len` takes `mu` and returns `len(entries)`. A completion that has not entered `publish` is invisible. A completion inside `publish` is visible only after insert, still under `mu`, and only after eviction, so `Len` cannot observe `capacity+1`. An in-flight key is absent from `entries`. After the first Close critical section, the count is 0 even if a Loader is still running.

## How each stated rule is satisfied

1. **Hit.** `entries` lookup under `mu`, before the `inflight` lookup. `touch` moves the entry to MRU. Return `clone` of the header copied under `mu`. Do not call `load`.

2. **Single-flight miss.** The first eligible `Get` inserts `inflight[key]` and `wg.Add(1)` before releasing `mu`, then starts one goroutine. Others wait on that flight's `done` channel. Exactly one `load` call. Each waiter clones the published bytes. Success path in `publish` inserts the resident entry before `close(done)`, so a `Get` that arrives after resolution hits.

3. **Error.** Non-nil `err` skips insert, publishes the error and nil data, closes `done`. Bytes accompanying the error are dropped. Next `Get` misses again and calls `load`. A recovered panic and a Goexit follow this same path with a `loaderFailure`.

4. **Ownership / copy.** The load goroutine clones before `publish` on success, and passes nil data on error. `Get` returns a new clone. The Loader's slice is not stored.

5. **Capacity / LRU.** Insert only in `publish` on success, only while `closed` is false. If `len(entries) == capacity`, unlink and delete `tail` first. In-flight flights are not in `entries`. `Len` is `len(entries)` under `mu`, and is 0 once Close has dropped the map.

6. **Recency.** `touch` on hit and on successful admit (new entry inserted at head). Completion order is recency order: the later successful `publish` is MRU.

7. **Caller context and orphans.** `load` is invoked as `load(c.ctx, key)` on a cache-owned goroutine. Caller `ctx` is used only in `wait` and in the already-done cold check. Cancelled waiters, including the starter, return without removing the flight and without aborting `load`. The load goroutine always reaches the finisher, which publishes if `publish` has not already committed. Priority is the one recheck loop: closed, else resolved result, else `ctx.Err()` if the caller is Done (after one re-snapshot), else `select` and loop. The `ctx.Err()` arm returns. A caller that is already Done does not busy-loop on `<-ctx.Done()`.

8. **Close.** First `Close` sets `closed`, drops `entries`, cancels `c.ctx`, waits for the WaitGroup, defensively clears `inflight` if needed, closes `joined`, returns nil. Load goroutines that observe `closed` in `publish` discard. Gets that see `closed` before becoming a starter or follower return `ErrClosed` and do not call `load`. A second `Close` waits on the same `joined` and returns nil, including when the first join is still in progress. `Len` is 0 as soon as the first critical section unlocks.

9. **Concurrency.** All map, list, and flag mutations happen under `mu`. `load` runs outside `mu`. Waiters block on channels, not on `mu`, except for the short `snapshot` critical section. No lock is held while waiting for a Loader, for `wg.Wait`, or for another `Get`. `flight` result fields are written before `close(done)` and read only after `done` is observed closed. `publish` unlocks with `defer`, so a panic cannot stick `mu`. A Loader panic or `Goexit` still `wg.Done`s. This is race-free under `go test -race`. `Close` does not deadlock against a Loader that returns, panics, or `Goexit`s on `c.ctx` cancellation, because `Close` waits without holding `mu` and the finisher always completes the flight. A Loader that ignores `c.ctx` and blocks forever will stall `Close`; the spec requires Loaders to return promptly. That stall is the Loader breaking its contract, not an internal deadlock. Waiters still wake on `c.ctx.Done()` and return `ErrClosed`.

## Edge cases the implementation must get right

- **Capacity 1.** Admit of a second key evicts the only resident entry. Hit on the sole entry refreshes MRU (no-op position).
- **Reload after eviction.** A key cannot be both resident and in-flight. Eviction only removes resident keys. A reload after eviction is a new flight.
- **Duplicate key during publish.** Not expected. The retry branch inside `publish` exists only so a panic after insert and before `published` does not insert a second list node. Do not start a second flight for a key that is still in `inflight`.
- **Empty and nil values.** `[]byte{}` success is cached and `Get` returns a fresh non-nil empty slice. `nil, nil` is cached as nil and `Get` returns `nil, nil`.
- **Empty string key.** A normal key. No special case.
- **Large concurrent distinct keys.** Different keys do not share flights. Capacity eviction serializes on `mu` inside `publish` only.
- **Close with no in-flight loads.** WaitGroup is already zero. Entries are already dropped in the first critical section. Close `joined`, return nil.
- **Close with waiters.** Cancel wakes `select`. Load goroutines return, `publish` discards, closes `flight.done`. Waiters take the closed branch and return `ErrClosed`. `Len` is already 0 while the Loader is still returning.
- **Get after Close.** `closed` check, `ErrClosed`, Loader call count unchanged.
- **Idempotent Close under concurrency.** Many `Close` callers: one sets `closed`, the others wait on `joined`. All return nil.
- **Hit during an unrelated in-flight load.** Lookup `entries` before `inflight`. A resident key is a hit even if another key is in flight.
- **Context values and deadlines.** The Loader receives `c.ctx`, which has no caller values and no caller deadline. Do not derive `c.ctx` from a caller and do not merge deadlines. Cancelling the caller does not cancel the Loader.
- **Re-entrancy.** Unsupported. Documented on `Loader`. Do not "fix" it.
- **math.MaxInt capacity.** Stored as an int and compared with `len(entries)`. The maps are not hinted. Eviction does not run until the map actually reaches that length.

## Files to write (implementation phase, not now)

- `worktree/cache.go` — package comment, verbatim spec comments plus the two appended paragraphs, `ErrClosed`, `New`, `cache`, `Get`, `Len`, `Close`, LRU helpers, `clone`, `publish`, `wait`, `snapshot`, `startLoad`, `loaderFailure`.
- `worktree/cache_test.go` — the private tests listed below. Required, not optional. Package `blockcache`.
- Do not edit the public test file except as the harness already does.
- `go.mod` already declares `module blockcache` and `go 1.25`. Keep it.

Imports in `cache.go`: `context`, `errors`, `fmt`, `runtime/debug`, `sync`. No third-party dependencies.

Package comment in `cache.go`, in addition to the `Get` paragraph:

> A Get whose context is already canceled, on a key that is neither resident nor in flight, returns ctx.Err() and does not call Loader. That is an implementation choice. The spec does not require it. An in-flight load is still joined. Do not change this choice as a bug fix.

## Test approach

Public suite (must pass, including `go test -race`):

| Test | What it locks |
| --- | --- |
| `TestHitReturnsCopy` | One Loader call, second Get is a hit, mutation of the first result does not change the second. |
| `TestSingleFlight` | 50 overlapped Gets, exactly one Loader entry, all equal bytes. Uses `testing/synctest`. The Loader blocks on a channel; overlap is observed before release. |
| `TestErrorNotCached` | First error (`errors.Is`), nil bytes, second Get reloads and succeeds, call count 2. |
| `TestLRUEviction` | Capacity 2: load a, load b, hit a, load c evicts b, hit a, reload b. Counts 1, 2, 1. |
| `TestLenBounded` | After each of 5 inserts into capacity 3, `Len() <= 3`; final `Len() == 3`. |
| `TestClosedReturnsErrClosed` | `Close` nil, Get is `errors.Is(..., ErrClosed)`, second `Close` nil. |
| `TestNewPanics` | `New(0, load)` and `New(1, nil)` panic. |

Run from the worktree, with the public test file present:

```text
go test -count=1 .
go test -race -count=1 .
go vet .
```

### Private test naming

`cache_test.go` is package `blockcache`, the same package as the public suite and as any hidden tests added beside it. Every private test function is named `TestImpl_...`. Every private helper is named `impl...`. Do not declare `testCtx`, `mustGet`, `assertPanics`, `waitBlocked`, or any other name the public file already declares. Do not declare a test whose name does not start with `TestImpl_`. A colliding `Test*` or helper is a compile error that fails the public suite.

Use `testing/synctest` for overlap. Block the Loader on a channel, start waiters, `synctest.Wait` until they are durably blocked, then assert and release. Do not use `time.Sleep` for ordering. Inside a synctest bubble, a failed assertion that must not kill the bubble root while goroutines are parked uses `t.Errorf`, matching the public single-flight test. Join waiter goroutines in the test. Do not treat `Close`'s return as proof that those goroutines have already observed `ErrClosed`.

Required private tests:

1. `TestImpl_StarterCancelWhileLoading`. Start one `Get`, wait until the Loader has entered and is blocked, cancel that caller's context, and assert that `Get` returns `ctx.Err()` before the Loader is released. Release the Loader with success. A later `Get` of the same key hits and the Loader call count stays 1. This rejects a design where the starter runs `load` on its own goroutine, and it rejects a wait loop that spins until the Loader returns.
2. `TestImpl_FollowerSurvivesStarterCancel`. Two Gets, one flight. Cancel only the starter. The follower receives the success bytes. A third `Get` after that hits. Call count is 1.
3. `TestImpl_AllWaitersCancelOrphanPopulates`. Every waiting `Get` returns `ctx.Err()`. The Loader still runs. After it succeeds, a subsequent `Get` does not call the Loader.
4. `TestImpl_CloseWhileLoading`. The Loader blocks until its context is Done, then sets a returned flag and returns success. Waiting Gets return `ErrClosed`. After `cancel` is observable inside the Loader, and before the Loader returns, `Len() == 0` and a new `Get` returns `ErrClosed`. `Close` returns only after the returned flag is set. The result is not resident. A second `Close` returns nil.
5. `TestImpl_ConcurrentClose`. Several goroutines call `Close`. All return nil. A following `Get` returns `ErrClosed`.
6. `TestImpl_SecondCloseDuringJoin`. One Loader blocks on `c.ctx.Done()`. One `Close` is in progress. A second `Close` waits for the same join and returns nil. Neither deadlocks.
7. `TestImpl_ErrorWithBytes`. Loader returns `([]byte("nope"), err)`. Waiter gets nil bytes and `errors.Is` the error. Key is not resident. A later `Get` calls the Loader again.
8. `TestImpl_PerWaiterCopies`. Two overlapped Gets receive equal non-empty bytes. Mutating one result leaves the other and a later hit unchanged.
9. `TestImpl_InFlightExcludedFromLen`. Capacity 1. One in-flight miss of a cold key. `Len() == 0` until the load succeeds, then `Len() == 1`. No other key.
10. `TestImpl_EvictWhileOtherKeyInFlight`. Capacity 2. Load `a`, load `b` so `a` is LRU. Start a load of `c` and leave it blocked. `Len() == 2`. Hit `b` so `b` stays MRU and `a` stays LRU. Release `c`. The completion evicts `a`. `b` and `c` hit afterwards. `a` reloads. `Len` did not include `c` before completion.
11. `TestImpl_DistinctKeysInFlight`. Capacity 2. Start loads of `a` and `b`. Release `b` first, then `a`. Both Loader calls happen. `a`'s later completion is MRU, so a following load of `c` evicts `b` and leaves `a` resident.
12. `TestImpl_NilAndEmptyPayload`. `nil, nil` round-trips as `nil, nil`. Non-nil empty round-trips as non-nil empty. A later hit does not call the Loader.
13. `TestImpl_AlreadyDoneCold`. `Get` with an already cancelled context, on a key with no entry and no flight, does not call the Loader and returns `ctx.Err()`. This locks the implementation choice.
14. `TestImpl_AlreadyDoneJoinsFlight`. A flight is in progress. A `Get` whose ctx is already cancelled returns `ctx.Err()` (cache still open) and does not increment the Loader call count. After release, a normal `Get` hits.
15. `TestImpl_LoaderPanic`. Loader panics. The waiting `Get` returns a non-nil error and nil bytes. A later `Get` calls the Loader again and can succeed. `Close` returns. The error string contains the panic value.
16. `TestImpl_LoaderGoexit`. Loader calls `runtime.Goexit`. The waiting `Get` returns a non-nil error and nil bytes. `inflight` does not stick: a later `Get` calls the Loader again and can succeed. `Close` returns.
17. `TestImpl_LoaderUsesCacheContext`. The caller's context carries a value and is cancellable. The Loader observes that value as absent, blocks until released by the test (not by the caller's cancel), and still completes successfully after the caller is cancelled. Call count for that key stays 1 on a later hit.

## Invariants

- `load` is called only with `c.ctx`, never with a `Get` ctx, and only from a cache-owned goroutine.
- `wg.Add(1)` happens under `mu` before that goroutine starts.
- `mu` is not held during `load`, during channel waits in `wait`, or during `WaitGroup.Wait` in `Close`.
- `publish` unlocks with `defer`.
- `len(entries) <= capacity` at every unlock.
- `inflight` keys and `entries` keys are disjoint at every unlock.
- `close(flight.done)` happens after the flight's `data`/`err` stores, under `mu`, in the load goroutine, and exactly once. `published` is set in that same critical section immediately before the close.
- `f.data` and `f.err` are read only after the reader has observed `f.done` closed.
- `snapshot` reads `closed` and the `done` state under the same `mu` hold, and does not read `f.data` or `f.err`.
- `close(joined)` happens exactly once, after the WaitGroup is zero.
- The first Close critical section sets `closed`, replaces `entries` with an empty map, clears `head` and `tail`, and cancels, in that order, before `wg.Wait`.
- After that critical section, `publish` does not insert.
- No path stores the Loader's slice.
- No path returns `entry.data` or `flight.data` without `clone`.
- A non-nil Loader error never inserts.
- `Get` after `closed` never calls `load`.
- A `Get` whose ctx is already Done, on a key with no entry and no flight, never calls `load`.
- A Loader panic or `Goexit` still closes `flight.done`, removes `inflight[key]`, and calls `wg.Done()`.
- `c.ctx` is cancelled only from `Close`, and only after `closed` is set.
- `entries` and `inflight` are created with no capacity hint.
- `wait` returns on `ctx.Err()` when the cache is open and the flight is unresolved. It does not `select` on an already-done caller context.
