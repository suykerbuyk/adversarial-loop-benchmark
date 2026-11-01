# Implementation plan: bounded concurrent single-flight block cache

## Public API

Implement package `blockcache` with the specified API:

- `type Loader func(ctx context.Context, key string) ([]byte, error)`.
- `type Cache interface` exposing `Get(context.Context, string) ([]byte, error)`, `Len() int`, and `Close() error`.
- `var ErrClosed = errors.New("blockcache: closed")`.
- `New(capacity int, load Loader) Cache`, panicking when `capacity < 1` or `load == nil`.

Use an unexported concrete implementation. `Get` returns `ErrClosed` when the cache has begun closing or is closed; `errors.Is` works for this sentinel.

## Internal data model and synchronization

One `sync.Mutex` protects the closed state, resident entries and LRU order, in-flight map, and every flight's result and resolved state. A cache-scoped context is created from `context.Background()` with a cancel function; it is never derived from a caller context.

The cache holds its capacity and loader, a `container/list` with MRU at the front and LRU at the back, a map from resident key to list element, and a map from key to `flight`. Each resident entry contains its key and cache-owned bytes. Each flight contains a `done` channel, result bytes, result error, and resolved flag. Flight result fields are immutable after resolution and are published by closing `done`.

Use two cache-wide lifecycle channels with distinct meanings:

- `closeStarted` closes immediately when the first `Close` marks the cache closed and cancels the cache context. Gets select on this channel so current waiters wake without waiting for loaders to return.
- `closeDone` closes only after all loader calls have returned and the resident entries have been dropped. Concurrent and later `Close` calls wait on this channel, ensuring they observe completion of the same join.

Use a `sync.WaitGroup` for loader goroutines. Under the mutex, Get checks closed state, looks up or creates a flight, and increments the wait group before starting a new loader goroutine. This serialization ensures no `Add` can race with the first Close's `Wait`: once Close marks closed under the same mutex, no later Get can add a loader. Do not hold the mutex while running a loader or waiting.

## Operations

### Get

Lock and first check the closed state. If closed, unlock and return `ErrClosed`. If the key is resident, move its list element to MRU, make a byte copy while protected, unlock, and return it. Otherwise join the existing flight or create and register a new flight, increment the wait group while still holding the mutex, then start the loader goroutine if this caller created it.

Wait outside the lock for the flight's `done`, `closeStarted`, or the caller's context cancellation. After any wake, lock and apply the specified priority: if the cache is closed, return `ErrClosed`; else, if the flight remains unresolved and the caller context is done, return `ctx.Err()`; else, if the flight is resolved, return its error or a fresh copy of its successful bytes. If a select wakes on caller cancellation but the flight has resolved by the locked check, return the load result. A canceled waiter does not remove or cancel the flight; therefore a load with no remaining waiters continues and can populate the cache.

### Loader completion

Call the loader with the cache-scoped context. When it returns, lock the cache. If closing has begun (including the cache context having been canceled), discard its bytes and resolve the flight with `ErrClosed`; it must not enter the resident cache. Otherwise, for a non-nil loader error discard any accompanying bytes and resolve with that error without admission. On success, copy the loader slice into cache-owned storage, evict the LRU entry first if the resident count equals capacity, insert the new entry at MRU, and save the successful result for waiters. Remove the flight from the in-flight map, set its result and resolved state, close `done`, and unlock. The loader goroutine decrements the wait group only after loader invocation and completion processing have returned.

Because flight result bytes are not exposed directly, every waiter copies them separately when returning. The resident data and flight result may share the cache-owned immutable copy; callers only receive fresh copies.

### Len

Lock and return the resident map size. Only resolved successful loads become resident. Admission and eviction occur under the same lock, so Len cannot observe a count above capacity.

### Close

The first caller locks, marks the cache closed, closes `closeStarted`, cancels the cache-scoped context, and unlocks. It then waits for all loader goroutines. Once they have returned, it locks, clears the resident map and LRU, unlocks, closes `closeDone`, and returns nil. A caller that finds the cache already closed unlocks and waits on `closeDone`, then returns nil. Since Get checks closed under the same lock before creating flights and incrementing the wait group, no loader can be added after joining starts. The loader completion path checks closed under that lock and cannot admit a late result.

Current Get waiters wake on `closeStarted` and check the closed state under the mutex, returning `ErrClosed` promptly even if a loader is slow. Close itself still waits for every loader as required. A loader must honor its documented obligation to return promptly after context cancellation for Close to finish.

## Rule-by-rule behavior

1. **Hit:** protected resident lookup, MRU promotion, and a returned byte copy; no loader invocation.
2. **Single-flight:** one flight per absent key in the in-flight map; only its creator starts a loader. All successful waiters return copies of the same loaded value.
3. **Errors:** all waiters see the loader error, accompanying bytes are discarded, no resident is created, and removal of the failed flight permits a later retry.
4. **Ownership:** loader bytes are copied into cache-owned memory. Get returns a fresh slice for every hit and every successful flight result, so caller mutation cannot affect shared state.
5. **Capacity and Len:** pending loads are outside the LRU. A successful resolution evicts the tail before insertion when full. Len counts residents only and remains at or below capacity under the mutex.
6. **Recency:** resident hits and successful admissions move the entry to the MRU front.
7. **Contexts and orphaning:** loaders use only the cache context. Caller cancellation ends only that caller's wait; its precedence is after closure and before an unresolved load result. Flights continue with zero waiters and successful orphaned loads are admitted.
8. **Close:** the first Close cancels loader contexts and broadcasts close start immediately; this wakes current waiters with `ErrClosed`. Close joins every loader before dropping entries and signaling join completion. Concurrent Close calls wait for the same completion. Gets after close begins return `ErrClosed` without starting work. Loader outputs after cancellation are discarded.
9. **Concurrency:** the mutex protects all shared mutable state and admission decisions; channels publish flight and lifecycle transitions. User loader code and waits execute outside the mutex, avoiding data races and lock-based deadlocks.

## Test approach

Use the supplied `instrument/resolve_public_test.go` acceptance suite, including its deterministic `testing/synctest` overlap check. The implementation validation should run the package tests normally and with `-race`, plus `go vet`, as required by the deliverable.

Add focused tests for specified edges not covered by the public suite:

- Loader returns bytes and an error: callers get the error and nil bytes, no entry is admitted, and a later Get retries.
- Concurrent waiters each receive distinct slices; mutating one does not affect another waiter or a later hit.
- Cancel one waiter while another remains, then cancel all waiters in a separate case; verify cancellation does not stop the load and a successful orphaned result is cached.
- Close during a blocked loader: current waiters promptly return `ErrClosed` as soon as close starts; loader context is canceled; Close remains blocked until the loader returns; late bytes are not cached; entries are dropped; post-close Get does not invoke the loader; and concurrent Close calls return only after the shared join completes.
- Verify cancellation-versus-completion ordering using channel handshakes: closure wins; caller cancellation wins only while the flight remains unresolved; a resolved flight result is returned if completion won before the locked state check.
- Poll Len during concurrent successful completions and evictions and assert it never exceeds capacity.

Use channels and explicit handshakes to control overlap and close windows rather than timing sleeps, with bounded contexts for tests that wait. No implementation is included in this plan.
