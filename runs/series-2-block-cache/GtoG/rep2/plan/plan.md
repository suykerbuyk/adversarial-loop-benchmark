# Implementation plan: `blockcache`

Bounded concurrent single-flight block cache. This plan is the implementor's contract. No code is written here.

Public tests live in `instrument/resolve_public_test.go` and are copied into the worktree as `blockcache_public_test.go`. Module is `blockcache`, Go 1.25 (`testing/synctest` is available; `bytes.Clone` and the Go 1.21 `panic(nil)` recover rule are both in force). This revision replaces the previous waiter loop and the previous copy helper. Those two sections contradicted the rules they claimed to implement. The point-by-point response is in `plan/plan_rebuttal.md`.

## Public API

Ship exactly the surface in the spec. Do not export helpers, options, or metrics. Exported comments match the spec.

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

`New` panics with a non-nil string: `"blockcache: capacity < 1"` or `"blockcache: nil Loader"`. The string is the reason. On Go 1.21 and later, including this module's `go 1.25`, `recover()` of `panic(nil)` returns a non-nil `*runtime.PanicNilError`, so `TestNewPanics` would pass either way. Do not cite the pre-1.21 `recover() == nil` rule.

`errors.Is` is the contract for `ErrClosed`. The implementation returns the sentinel itself. It does not wrap it, and it does not substitute `ErrClosed` for a Loader error while the cache is still open. `Close` always returns `nil`.

Compile-time check, unexported: `var _ Cache = (*cache)(nil)`.

Files (worktree root is the module root):

- `cache.go` — types, `New`, `Get`, `Len`, `Close`, `cloneBytes`.
- `blockcache_public_test.go` — already present; do not edit.
- `cache_test.go` — private tests listed below. They lock rule 4's nil-versus-empty header and rule 7(b), which the public suite does not exercise.

## Internal data model

One concrete type, unexported, returned as `Cache`. All methods use a pointer receiver.

```go
type cache struct {
	capacity int
	load     Loader

	root   context.Context    // cache-scoped parent of every Loader call
	cancel context.CancelFunc // first Close only; CancelFunc is idempotent

	mu       sync.Mutex
	closed   bool
	resident map[string]*entry // successful admits only; len <= capacity whenever mu is released
	inflight map[string]*flight
	order    *list.List        // front = MRU, back = LRU; elements are *entry
	joining  sync.WaitGroup    // one Add per flight whose loader goroutine was launched, or whose launch failed
	waiters  sync.WaitGroup    // one Add per Get that observed !closed
	closeWG  sync.WaitGroup    // 1 until the first Close finishes join + waiter drain + drop
}

type entry struct {
	key  string
	data []byte        // cache-owned clone; never the Loader's slice, never a caller's slice
	elem *list.Element // position in order
}

type flight struct {
	done chan struct{} // closed exactly once, under mu, after data/err are published
	data []byte        // cache-owned clone on published success; nil on error or discard
	err  error         // Loader error, or the spawn-failure error; never ErrClosed
}
```

`container/list` is not safe for concurrent use. Every list operation (`PushFront`, `MoveToFront`, `Back`, `Remove`, `Init`) runs under `mu`, including `Close`'s `order.Init()`. Maps, `closed`, flight publish, and flight reads run under `mu`.

`closeWG.Add(1)` runs in `New`, before `New` returns. `Done` runs once, on the owner path of the first `Close`, after Loaders have returned, after every Get that observed `!closed` has returned, and after entries are dropped. A second `Close` waits on `closeWG` and returns nil. It does not call `Done`. Only the first `Close` calls `cancel`. A second `cancel` would be harmless; the plan still calls it once.

`joining` counts loader goroutines (and a failed `go` that never created one). `waiters` counts every `Get` that passed the closed check and has not yet returned. `Close` waits for both. No other locks. `mu` is never held across `load(...)`, across a wait on `flight.done` / caller ctx / `root`, or across `joining.Wait` / `waiters.Wait` / `closeWG.Wait`.

`ctx.Err()` and `ctx.Done()` run under `mu` inside the snapshot. A caller context must not call back into `Cache` from `Err` or `Done`. Standard library contexts do not.

### Copies

`cloneBytes` is the only copy. It is used for the stored bytes and for every successful `Get` return.

```go
// cloneBytes copies src.
// A nil src stays nil. A non-nil src, including length 0, is a new slice of the same length.
// make returns a non-nil slice when the length argument is 0, so a non-nil empty payload
// stays non-nil and does not alias src.
func cloneBytes(src []byte) []byte {
	if src == nil {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}
```

`bytes.Clone` (Go 1.20+) has the same nil-versus-empty result and may replace this body. `append([]byte(nil), src...)` must not. `append` returns the destination unchanged when the source length is 0, and a nil destination therefore stays nil for `nil`, `[]byte{}`, and `make([]byte, 0)`. That collapses a non-nil empty Loader result to nil. Rule 4 requires a fresh copy that does not retain the Loader's slice; the slice header's nil bit is part of that copy. Public tests compare with `bytes.Equal` and string conversion, which treat nil and empty as equal, so they do not catch the collapse. `cache_test.go` checks the header.

Ownership:

- On Loader success, `runLoad` calls `cloneBytes(raw)` before `finish`, then drops `raw`. `finish` stores that clone in `f.data` and `entry.data` (one cache-owned backing array, two headers). It does not store `raw`.
- On Loader error, `runLoad` passes `nil` bytes into `finish`. Accompanying bytes are discarded. Waiters receive `(nil, err)`. The Loader error is not wrapped. It is not replaced with `ErrClosed` while the cache is open.
- Every successful `Get` returns `cloneBytes` of the stored slice. Callers may mutate the result. A nil success stays nil. A non-nil empty success stays non-nil empty.
- If `finish` observes `closed`, it does not store the clone and does not publish success bytes. It sets `f.data` and `f.err` to nil, closes `f.done`, and lets the snapshot return `ErrClosed`.

### Recency and capacity

`container/list`: `MoveToFront` on hit, `PushFront` on successful admit, both before unlock. Evict with `order.Back()` then `Remove`, under `mu`. Every admit and hit moves to front, so LRU ties do not arise. If they did, `Back()` is a valid least-recent element (spec: any least-recent one).

`len(resident)` is the resident count. Flights are absent from `resident` and from `order` until a successful admit, so they do not consume capacity and cannot be evicted. Admit runs only in `finish`, only when `err == nil` and `!closed`. While `len(resident) >= capacity`, evict one LRU. If `Back()` is nil, stop, so a broken list cannot spin under `mu`. Then insert. Evict before insert, in the same critical section, so `len(resident)` is never above `capacity` when `mu` is released, including under a concurrent `Len`. The `>=` test also repairs a map that a bug left over capacity; with the invariant held it evicts exactly one.

`Len` may miss an admit that has not yet taken `mu`. That is the allowed one-critical-section lag. After `Close` returns, `resident` is empty, so `Len` is 0. While `Close` is still inside its join, `Len` may still report pre-close residents. That lag is allowed. `Close` drops them before it returns. `finish` does not admit after `closed` is set, so `Len` cannot grow past the drop.

`inflight` and `resident` are disjoint. The hit path does not create a flight for a resident key. `finish` deletes the flight before insert. A second flight for a resident key is not a supported path.

## Concurrency protocol

The code blocks in this section are the protocol. Prose does not add a second return path.

### `New`

```go
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity < 1")
	}
	if load == nil {
		panic("blockcache: nil Loader")
	}
	root, cancel := context.WithCancel(context.Background())
	c := &cache{
		capacity: capacity,
		load:     load,
		root:     root,
		cancel:   cancel,
		resident: make(map[string]*entry),
		inflight: make(map[string]*flight),
		order:    list.New(),
	}
	c.closeWG.Add(1)
	return c
}
```

### Locked snapshot

Rule 7 priority is a property of one lock hold, not of which `select` case woke the goroutine. `select` only wakes. The only function that may return `ErrClosed`, `ctx.Err()`, or a load result for a waiter is `snapshot`. It holds `mu` for the whole decision.

```go
// snapshot reports the waiter result.
// waiting == true means the load is still in flight and the caller is still waiting;
// the caller must then block in select and call snapshot again.
// waiting == false means data/err is the return value; the caller must not select again.
func (c *cache) snapshot(ctx context.Context, f *flight) (data []byte, err error, waiting bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed, false
	}
	select {
	case <-f.done:
		// Resolved in this same hold as the closed check.
		if f.err != nil {
			return nil, f.err, false
		}
		return cloneBytes(f.data), nil, false
	default:
	}
	// f.done is still open. Priority (b) is decided here, before the caller waits again.
	if err = ctx.Err(); err != nil {
		return nil, err, false
	}
	return nil, nil, true
}
```

Order inside that hold:

1. `closed` → `ErrClosed`. This beats a resolved flight, a cancelled caller, and a Loader error. A Get that overlaps Close fails `errors.Is(err, context.Canceled)` and passes `errors.Is(err, ErrClosed)`.
2. Else if `f.done` is still open and `ctx.Err() != nil` → that `ctx.Err()`. Do not cancel the Loader. Do not delete the flight. Do not read `f.err`. Return to the caller. Do not `select` again.
3. Else if `f.done` is closed → the published result: `(nil, f.err)` when `f.err != nil`, otherwise `cloneBytes(f.data)`. A resolved load beats a caller ctx that is also done. Step 1 already ruled out `closed`.
4. Else the load is in flight and the caller is still waiting. `waiting == true`. The caller unlocks (via the defer) and blocks in `select`.

`closed` and `f.done` are sampled in this one hold. `Close` sets `closed` only while holding `mu`, so it cannot flip the flag between the closed check and the receive on `f.done`. A success return happens only when `closed` was false in the hold that copied the result. After that unlock the Get has left the waiting state. `Close` still waits for it on `waiters` before dropping entries and returning.

`close(f.done)` is a wake and a publish signal. It is not permission to return success. `root.Done()` is a wake. It is not a return path. There is no `root.Err()` arm.

### Waiter loop

Leader and joiners both call `wait`. The leader does not run `load` on the Get goroutine.

```go
func (c *cache) wait(ctx context.Context, f *flight) ([]byte, error) {
	for {
		data, err, waiting := c.snapshot(ctx, f)
		if !waiting {
			return data, err
		}
		// Durable block. This select has no return.
		if done := ctx.Done(); done == nil {
			select {
			case <-f.done:
			case <-c.root.Done():
			}
		} else {
			select {
			case <-f.done:
			case <-done:
			case <-c.root.Done():
			}
		}
	}
}
```

An already-done caller hits step 2 of `snapshot` on the first iteration and returns `ctx.Err()` before any `select`. A caller whose ctx becomes done while blocked wakes from `select`, calls `snapshot` again, and returns `ctx.Err()` if `f.done` is still open. It does not `select` on the already-closed `ctx.Done()` again.

Why this does not busy-spin when the protocol is followed:

- `snapshot` returns `waiting == true` only when `!closed`, `f.done` is open, and `ctx.Err() == nil`.
- `cancel` runs only after `closed` is set, so `root.Done()` is still open in that state.
- `f.done` is closed only by `finish`, under `mu`, and `snapshot` would have observed that close.
- A non-nil `ctx.Done()` is open when `ctx.Err() == nil` (context contract).
- The following `select` therefore blocks. `testing/synctest` treats that as a durable wait.

A tight `select` on an already-closed channel is not a durable wait. `synctest.Wait` would not return, the 5s deadline would fire, and rule 7(b) would never be observed. That is why step 2 returns before `select`.

### `Get`

`waiters.Add` runs under `mu` in the same hold that observed `!closed`, before unlock. `defer waiters.Done()` is the next statement, so every return and panic after the `Add` balances it. `Close` sets `closed` under the same mutex before `waiters.Wait`, so a Get that has not yet passed the check cannot `Add` after the wait has begun, and a Get that already passed it is included in the wait.

```go
func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.waiters.Add(1)
	defer c.waiters.Done()

	if ent, ok := c.resident[key]; ok {
		c.order.MoveToFront(ent.elem)
		data := cloneBytes(ent.data)
		c.mu.Unlock()
		return data, nil
	}
	if f, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		return c.wait(ctx, f) // join even when ctx.Err() != nil
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err // cold miss, already done: do not insert, do not call Loader
	}
	f := &flight{done: make(chan struct{})}
	c.inflight[key] = f
	c.joining.Add(1)
	c.mu.Unlock()

	c.spawn(key, f)
	return c.wait(ctx, f)
}
```

Path notes:

- A Get that observes `closed` never calls `Loader` and never `Add`s to `joining` or `waiters`.
- A hit ignores caller ctx. Rule 1 requires the copy. Rule 7's priority list applies to a Get that is waiting on an in-flight load. There is no in-flight load on a hit.
- Joining an existing flight is mandatory even when `ctx.Err() != nil`. The cancelled caller becomes a waiter and takes priority (b) in `snapshot` if the load is still in flight, or priority (c) if it has resolved, or priority (a) if the cache has closed. It must not start a second Loader.
- The cold-miss skip is one locked predicate: `!closed && not resident && not inflight && ctx.Err() != nil`. It is not "any ctx is done." `root` being cancelled is not this case: `root` is cancelled only after `closed` is set, and the closed check already returned. A load that has started keeps running after every waiter leaves, and `finish` admits on success when `!closed`. Caller cancel does not discard a started load.
- Empty key is a normal key.

Flight insert, `joining.Add`, and the miss check share one hold, before unlock. Two Gets cannot both observe a miss for the same key. `Close` cannot pass `joining.Wait` ahead of this `Add`: `Wait` starts only after `closed` is set and `mu` is released, and `Add` happens before that flag under the same mutex.

### Spawn pairing

`joining.Done` lives in the loader goroutine. If the `go` statement panics on the Get stack, that goroutine does not exist, and `Done` inside it never runs. `Close` would then block in `joining.Wait` after setting `closed`. Rule 9 forbids that deadlock. `waiters.Done` still runs via `Get`'s defer; that does not balance `joining`.

`spawn` balances `joining` on the Get side when `go` does not return. `Add` stays in `Get`, under `mu`, before unlock. Moving `Add` into the new goroutine, and publishing the flight only after that goroutine runs, races either single-flight (two misses both start a load) or `WaitGroup` (`Close` reaches `Wait` before `Add`). Those races are why the count is added in `Get` and repaired in `spawn` if `go` does not start.

```go
// errLoaderFailed is unexported. It is published only when go panics before the
// loader goroutine exists. It is not cached. It is not ErrClosed.
var errLoaderFailed = errors.New("blockcache: loader failed to start")

func (c *cache) spawn(key string, f *flight) {
	started := false
	defer func() {
		if started {
			return
		}
		// go did not return. runLoad will not run and will not Done.
		c.finish(key, f, nil, errLoaderFailed)
		c.joining.Done()
	}()
	go c.runLoad(key, f)
	started = true
}
```

`started = true` is the statement immediately after `go`. If `go` panics, it does not run, the defer calls `finish` (joiners unblock, nothing is admitted) and `joining.Done`, then the panic continues. `Get`'s `waiters.Done` runs on the way out. `Close` from a test cleanup can finish. If `go` returns, the new goroutine exists and owns `joining.Done`; the defer sees `started` and does nothing. An unrecoverable runtime throw kills the process; a `Close` deadlock is then moot. The recoverable panic is the case this pairing closes.

`runLoad` never holds `mu` across `load`:

```go
func (c *cache) runLoad(key string, f *flight) {
	defer c.joining.Done()
	defer func() {
		if rec := recover(); rec != nil {
			c.finish(key, f, nil, errLoaderFailed)
			panic(rec)
		}
	}()
	raw, err := c.load(c.root, key) // cache-scoped ctx, never the caller's
	if err != nil {
		c.finish(key, f, nil, err)
		return
	}
	c.finish(key, f, cloneBytes(raw), nil)
}
```

Defer order: the recover defer is registered after `joining.Done`, so on a Loader panic it runs first, calls `finish`, re-panics, and then `joining.Done` runs. Spec does not require surviving a Loader panic. The recover exists so `f.done` still closes and `Close` is not stuck. The re-panic keeps the process failure visible. `finish` is idempotent, and the panic path and the normal path are alternatives, so `finish` runs once.

### `finish`

`finish` holds `mu` from the idempotence check through delete, the closed check, the optional admit, the publish, and `close(f.done)`. It does not release `mu` between those steps. It does not close `f.done` before the closed check.

```go
func (c *cache) finish(key string, f *flight, owned []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-f.done:
		return // already published
	default:
	}
	if c.inflight[key] == f {
		delete(c.inflight, key)
	}
	if err != nil || c.closed {
		// Error wins over accompanying bytes. Closed discards success too.
		// Do not store. Do not publish success bytes.
		// When closed, leave f.err nil so a waiter cannot observe a Loader
		// context.Canceled; the snapshot returns ErrClosed first anyway.
		if err != nil && !c.closed {
			f.err = err
		}
		f.data = nil
		close(f.done)
		return
	}
	// Success and cache still open.
	// closed, set before cancel, is the only discard bit.
	// Caller-ctx cancellation must not discard: this load is orphaned-but-valid under rule 7.
	f.data = owned
	f.err = nil
	for len(c.resident) >= c.capacity {
		elem := c.order.Back()
		if elem == nil {
			break
		}
		old := elem.Value.(*entry)
		delete(c.resident, old.key)
		c.order.Remove(elem)
	}
	ent := &entry{key: key, data: owned}
	ent.elem = c.order.PushFront(ent)
	c.resident[key] = ent
	close(f.done)
}
```

`owned` is already `cloneBytes(raw)` or nil. `entry.data` and `f.data` share it. Each `Get` clones again on the way out. `root.Err()` is not a store condition. A `finish` that takes `mu` after `closed` is set discards. A `finish` that admits before the flag is removed by `Close`'s mandatory clear of `resident` after both waits.

`Close` does not close `f.done`. `finish` owns that close, exactly once, under `mu`. A second close would panic.

### `Len`

```go
func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.resident)
}
```

Never walks `inflight`. Never above `capacity`.

### `Close`

```go
func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.closeWG.Wait()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	c.cancel()       // after the flag; wakes Loaders via root and blocked waiters via root.Done
	c.joining.Wait() // every loader goroutine has returned, or a failed spawn has Done
	c.waiters.Wait() // every Get that observed !closed has returned

	c.mu.Lock()
	c.resident = make(map[string]*entry)
	c.order.Init()
	c.inflight = make(map[string]*flight) // finish already deleted its flight; clear anyway
	c.mu.Unlock()

	c.closeWG.Done()
	return nil
}
```

Order:

1. `closed = true` under `mu` before `cancel` and before either `Wait`. A `finish` that takes `mu` after the flag discards. A `Get` that takes `mu` after the flag returns `ErrClosed` and does not `Add`.
2. `cancel` before `joining.Wait`, so a Loader that honors ctx returns. The spec requires the Loader to return promptly when its ctx is Done. `cancel` is not a waiter return path. Waiters wake, then `snapshot` sees `closed`.
3. `joining.Wait` does not hold `mu`. `finish` takes `mu` and returns, then the goroutine's `defer joining.Done()` runs. `Close` is not holding `mu`, so `finish` cannot deadlock against `Wait`.
4. `waiters.Wait` after `joining.Wait`. Every flight is published and `closed` is set, so every registered Get can take `snapshot` and return. A Get blocked in `select` is woken by `root.Done()` or by an already-closed `f.done`. A Get between iterations sees `closed` on the next `snapshot`. `Close` does not return while a Get that observed `!closed` is still inside `Get`.
5. Drop `resident` after both waits, under `mu`. This is mandatory even when every `finish` skipped its admit. A success that `finish` admitted before the flag is removed here. A success that arrives after the flag is not admitted.
6. `closeWG.Done` last, so a second `Close` unblocks only after the drop.

`waiters` accounting:

- `Add` under `mu` immediately after observing `!closed`, before unlock, before `load`, and before any wait.
- `Done` on every return after that `Add` (`defer`).
- `Close` sets `closed` under the same `mu` before `waiters.Wait`.
- A Get that has not yet locked sees `closed` and does not `Add`.
- There is no `Add` after `Wait` has begun.

A Get that copied its result and unlocked before `closed` was set has left the waiting state and may return success. `waiters.Wait` includes that Get, so when `Close` returns, that Get has already returned. It does not return success after observing `closed`.

## How each rule is satisfied

1. **Hit.** Under `mu`, `resident` lookup, `MoveToFront`, `cloneBytes(ent.data)`, unlock, return. No `load`. Caller ctx is ignored. `TestHitReturnsCopy`: second Get does not increment the loader counter; mutating the first result does not change the second.

2. **Single-flight miss.** The first miss inserts `inflight[key]` under `mu` before unlock and before `load`. Later Gets see the flight and call `wait`. Exactly one `load`, invoked as `c.load(c.root, key)`. Each waiter gets `cloneBytes(f.data)` from `snapshot` after `f.done` is closed in that same hold. `TestSingleFlight`: 50 Gets block in `select` (durable under synctest), the loader goroutine is blocked inside `load` and does not hold `mu`, `calls == 1` while they are parked, and after release all 50 results equal `block:k`.

3. **Error.** `finish` stores only when `err == nil` and `!closed`. Non-nil err with non-nil bytes: bytes discarded, `f.data` stays nil, `f.err` is the Loader error, and that publish happens only when `!closed`. Waiters get `(nil, err)` from `snapshot`. The flight is removed in that same section, so a later Get misses and loads again. `TestErrorNotCached`: `errors.Is(err, boom)`, nil bytes, then a second successful load, call count 2. If `closed` is set, waiters get `ErrClosed` instead of the Loader error.

4. **Ownership.** `cloneBytes` into storage at the `load` return, and `cloneBytes` out of the cache on every successful Get. The cache never aliases the Loader slice or a returned slice. Nil stays nil. Non-nil empty stays non-nil empty. `append([]byte(nil), src...)` is not used.

5. **Capacity / LRU.** Admit evicts `order.Back()` while `len(resident) >= capacity`, then `PushFront`, under `mu`, before unlock. In-flight keys are not in `resident` or `order`. `Len` is `len(resident)` under `mu`. `TestLRUEviction`: capacity 2, sequence a, b, hit a, c, hit a, reload b, per-key call counts 1, 2, 1. `TestLenBounded`: five inserts, capacity 3, `Len` always ≤ 3 and finally 3.

6. **Recency.** Hit calls `MoveToFront` before unlock. Successful admit inserts at front. That makes b the LRU in `TestLRUEviction` after a is touched. Two finishes: the later lock holder is MRU.

7. **Caller context and orphans.** `load` receives `c.root`, never the caller's ctx. A caller that leaves returns from that Get only; the flight and goroutine stay. Success still admits if `!closed` at `finish`. Waiter priority is only `snapshot`: `closed` → `ErrClosed`; else `f.done` still open and caller ctx Done → `ctx.Err()` before any further `select`; else `f.done` closed → the load result or the Loader error. A resolved load beats a cancelled caller. `closed` beats both, including when `f.done` is already closed. The only skip of `Loader` is the locked cold-miss predicate. Joining an existing flight is mandatory even if the caller ctx is already done. Caller cancel does not discard a load that already started.

8. **Close.** First Close sets `closed` under `mu`, cancels `root`, waits for every Loader (`joining`), waits for every Get that passed the closed check (`waiters`), clears resident and inflight under `mu`, `closeWG.Done()`, returns nil. Waiters wake on `root.Done()` or on an already-closed `f.done`, then `snapshot` returns `ErrClosed`. A Loader return after `closed` is discarded inside `finish` and is not readable as success. A Get that has not passed the closed check sees `closed` and does not call Loader. Second Close waits on `closeWG` and returns nil. `TestClosedReturnsErrClosed` covers post-close Get and idempotent Close. `Close` does not return while a current waiter is still inside `Get`.

9. **Concurrency.** One mutex. No lock across Loader, channel waits, or any `Wait`. `joining.Add` before unlock, paired with `Done` in `runLoad` or in `spawn` if `go` does not return. `waiters.Add` before unlock, paired with `defer Done`. `go test -race` on the public suite and on `cache_test.go`. No loader leak on Close: every `runLoad` exits after `load` returns or after the panic-path `finish`, and Close waits for that. No waiter leak on Close: `waiters.Wait` covers every Get that observed `!closed`.

## Invariants

- `len(resident) <= capacity` whenever `mu` is released.
- `inflight` and `resident` are disjoint. `len(order) == len(resident)`. Every entry's `elem` is in `order`.
- `order` is touched only under `mu`.
- `load` is called only with `c.root`, at most once per flight, never under `mu`.
- `f.done` is closed at most once, under `mu`, after `f.err` / `f.data` are set, in the same critical section as the admit-or-discard decision.
- A waiting Get returns `ErrClosed` if `closed` is true in `snapshot`, even when `f.done` is already closed.
- A waiting Get returns `ctx.Err()` from `snapshot` when `f.done` is still open and the caller ctx is Done, and does not `select` again.
- `snapshot` is the only waiter return path. `select` does not return a value. `root.Err()` is not consulted.
- A Get creates a flight only when `!closed && ctx.Err() == nil && not resident && not inflight`.
- A Get joins an existing flight even when its own ctx is already done.
- `cancel` runs only after `closed` is set, and only from the first `Close`.
- After `Close` returns: `resident` is empty, no Loader is still running, every Get that observed `!closed` has returned, further Gets return `ErrClosed` without calling Loader.
- `joining.Add` is paired with exactly one `Done`, either in `runLoad` or in `spawn`'s defer when `go` does not return.
- `closeWG.Done` runs exactly once.
- No mutex is held across `load` or across `joining.Wait` / `waiters.Wait` / `closeWG.Wait`.

## Constraints that keep the previous defects out

These are part of the protocol. A later edit that puts them back fails a stated rule.

- Waiter results come from `snapshot` only. The loop returns when `waiting == false`. An already-done ctx returns `ctx.Err()` inside that hold, before `select`.
- `closed` is sampled in the same hold as the receive on `f.done` and as the copy of `f.data` / `f.err`.
- There is no return arm on `root.Err()`. `root.Done()` only wakes `select`. A later edit that cancels `root` without setting `closed` would busy-spin; the fix for that edit is to set `closed` first, not to return `ErrClosed` from `root.Err()`. Returning `ErrClosed` from `root.Err()` would skip a load result that rule 7 requires while the cache is still open.
- Copies go through `cloneBytes` (`make` + `copy`, or `bytes.Clone`). `append([]byte(nil), src...)` collapses a non-nil empty slice to nil.
- `joining.Add` in `Get` is paired with `spawn`'s defer when `started` is still false. Do not assume `go` cannot fail. Do not move `Add` into `runLoad` unless the flight is still published in `Get`'s critical section and `Close` cannot `Wait` before that `Add`.
- `New`'s panic values are non-nil strings because the message is the API. Do not justify that with the pre-1.21 `recover() == nil` behavior.

## Edge cases

- **Nil successful payload.** Store nil, return nil, `err == nil`. Nil bytes are not an error.
- **Empty non-nil payload.** `cloneBytes([]byte{})` is non-nil and length 0. A later Get returns non-nil empty. The stored header is not the Loader's header.
- **`(non-nil, err)` from Loader.** Bytes discarded. Waiters get `(nil, err)` when the cache is open. Key is not resident.
- **Leader's own ctx cancelled after it started the load.** Leader is also a waiter. `snapshot` returns `ctx.Err()` if `f.done` is still open. The goroutine still finishes and may admit. Other waiters still receive the result if the cache stays open.
- **All waiters leave.** Flight remains until `finish`. Success populates the cache if `!closed`. The goroutine exits. The entry is a normal resident. The cold-miss skip never applies once a flight exists.
- **Close with zero in-flight loads.** `joining.Wait` returns immediately. `waiters.Wait` covers any Get still copying a hit. Entries dropped. Returns nil.
- **Close with waiters and an in-flight load.** Flag, cancel, Loaders return, `finish` discards because `closed`, waiters' `snapshot` returns `ErrClosed`, `waiters.Wait` returns, entries dropped, Close returns nil.
- **Load already resolved, then Close, waiter has not copied.** Next `snapshot` sees `closed` in the same hold as `f.done` and returns `ErrClosed`. If the waiter is blocked in `select`, `f.done` or `root.Done` wakes it, then `snapshot` sees `closed`. If it already passed `snapshot` with `!closed` and copied, it has left the waiting state; `waiters.Wait` still waits for that return.
- **`finish` publishes success, unlocks, Close sets `closed`, waiter locks.** `snapshot` sees `closed` and returns `ErrClosed`. The admitted entry is dropped before Close returns.
- **Get arrives after close began but before join finished.** Sees `closed`, returns `ErrClosed`, does not call Loader, does not `Add` (cannot stall Close).
- **Single-flight under capacity pressure.** N distinct in-flight keys do not evict each other. When they complete, admits evict LRU residents until `len(resident) <= capacity`. The key being admitted is not resident yet, so it is not the eviction victim. Never evict it in the same section.
- **Re-load after error.** Waiters of the failed flight observe `f.err`. A new flight is created only by a Get that runs after `finish` deletes `inflight`.
- **Loader returns `context.Canceled` because it observed `root` after cancel.** `finish` sees `closed` (flag is set before cancel) and does not publish that error. Waiters get `ErrClosed`. The error is not cached. A later Get is `ErrClosed` and does not reload.
- **Caller cancel is not a Loader result.** Caller cancel never reaches `load`.
- **Panic in Loader.** `finish` with a non-nil error, `joining.Done` via defer, re-panic. Hidden tests are unlikely to panic the Loader. The re-panic fails the test process.
- **`go` panics before the goroutine exists.** `spawn` publishes `errLoaderFailed`, closes `f.done`, `joining.Done`, re-panics. Joiners do not block. `Close` does not block in `joining.Wait`.
- **synctest bubble.** The public single-flight test blocks the Loader on a channel. The Gets block in `select` on `f.done`, caller `Done`, and `root.Done`. `mu` is not held across that wait. There is no busy-spin and no `time.Sleep`.

## Test approach

Public suite (do not modify). Run from the worktree:

```
go test -count=1 .
go test -count=1 -race .
go vet .
```

| Test | Rules |
| --- | --- |
| `TestHitReturnsCopy` | 1, 4 (mutation; not the nil-versus-empty header) |
| `TestSingleFlight` | 2, 9 (overlap). Passes only if the 50 Gets are durably blocked in `select` and the loader does not hold `mu` |
| `TestErrorNotCached` | 3 |
| `TestLRUEviction` | 5, 6 |
| `TestLenBounded` | 5 |
| `TestClosedReturnsErrClosed` | 8 (post-close Get, idempotent Close) |
| `TestNewPanics` | `recover() != nil` on `New(0, load)` and `New(1, nil)` |

Private tests in `cache_test.go`, same package, run with `-race` before the work is called done. Delete a private test that is flaky rather than weakening the cache. Use `synctest` and a 5s deadline, matching the public suite, so a busy-spin fails the deadline instead of hanging.

Required, because the public suite does not catch the two majors:

- **Nil versus empty.** `cloneBytes(nil)` is nil. `cloneBytes([]byte{})` is non-nil and length 0, and is not the input header. A Loader that returns `[]byte{}` and nil error: `Get` returns non-nil empty, a second `Get` does not call Loader and also returns non-nil empty. A Loader that returns `(nil, nil)`: `Get` returns `(nil, nil)` and a second `Get` hits. A Loader that returns `([]byte("x"), err)`: waiter gets `(nil, err)` and the next `Get` calls Loader again.
- **Rule 7(b), already done, load still in flight.** Inside `synctest`: Loader blocks on a channel and counts calls. One Get starts the load. `synctest.Wait` until that load is blocked. A second `Get` with an already-cancelled ctx returns `context.Canceled`, nil bytes, while the Loader is still blocked and `calls == 1`. It must return without waiting for the load. Then release the Loader; a later `Get` hits and `calls` stays 1. A busy-spin fails this test: `synctest` does not treat a tight `select` on an already-closed channel as blocked, so the cancelled `Get` never returns and the 5s deadline fires.
- **Rule 7(b), ctx becomes done while blocked.** A Get blocks in `select` on an in-flight load. Cancel its ctx. That `Get` returns `ctx.Err()` before the Loader returns. A sibling with a live ctx still receives the bytes after release. Call count stays 1.
- **Already-cancelled cold key.** Does not call Loader. Returns `ctx.Err()`.
- **Already-cancelled resident key.** Returns the copy. Does not return `ctx.Err()`. Does not call Loader.
- **Distinct copies.** Two successful results with `len > 0` have different backing arrays. Mutating one does not change the other or a later hit.
- **Orphan.** Sole waiter cancels, Loader then succeeds, a subsequent `Get` hits without a second load.
- **Close during in-flight load.** Loader blocks on `ctx.Done()`. `Close` returns nil within the deadline. Blocked Gets return `ErrClosed`, not `context.Canceled` and not the load bytes. A follow-up `Get` returns `ErrClosed` and does not call Loader. `Len() == 0`. A second `Close` concurrent with the first both return nil.
- **Close overlaps a hit.** Either the hit returns a copy (it passed the closed check) or `ErrClosed`. No deadlock. `Len()` after `Close` returns is 0.
- **Capacity 1.** Get a, get b evicts a, get a reloads.
- **Many keys in flight above capacity, then all succeed.** `Len() == capacity`. No race, no deadlock.
- **`Len` during an in-flight miss.** Stays at the previous resident count.
- **Empty string key.** Works as a normal key.

`TestNewPanics` does not justify `panic(nil)`. The private suite does not need to assert the panic string. The strings in `New` are for the caller who hits the precondition.

## Implementation order

1. Types, `cloneBytes`, `New` (non-nil panic strings, maps, list, root ctx, `closeWG.Add(1)`), `ErrClosed`.
2. `Get` hit path, `Len`, `waiters` registration.
3. `snapshot` and `wait`, with no return except `snapshot`.
4. Single-flight miss, `spawn` pairing, `runLoad`, `finish` as one critical section, error path (no store).
5. LRU admit / evict (`len >= capacity`, stop if `Back()` is nil) and MRU on hit before unlock.
6. Cold-miss skip only under the locked predicate. Join an existing flight even when ctx is done.
7. `Close`: flag before cancel, `joining.Wait`, `waiters.Wait`, drop under `mu`, `closeWG`, idempotent second Close.
8. Panic-path `finish` so `f.done` always closes.
9. `go test`, `go test -race`, and `go vet` on the public suite, then the private tests above, especially rule 7(b) under `synctest` and the nil-versus-empty header.
