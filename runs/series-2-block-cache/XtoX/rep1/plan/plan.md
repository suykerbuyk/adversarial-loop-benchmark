# Implementation plan: bounded concurrent single-flight block cache

## Public API

Implement package `blockcache` with the exact API in the spec:

- `type Loader func(ctx context.Context, key string) ([]byte, error)`
- `type Cache interface { Get(context.Context, string) ([]byte, error); Len() int; Close() error }`
- `func New(capacity int, load Loader) Cache`
- `var ErrClosed = errors.New("blockcache: closed")`

`New` panics for capacity below one and for a nil loader. Use an unexported concrete cache type to implement the interface. Do not add exported API.

## Internal data model

The concrete cache owns one mutex protecting all mutable state, a fixed positive capacity, the Loader, resident entries, LRU order, in-flight loads, a cache-scoped context and cancel function, a closed flag, and a wait group for Loader goroutines.

- Resident entries map keys to immutable cache-owned byte slices and their LRU list elements. A `container/list` can hold keys in MRU-to-LRU order; map/list changes happen only under the mutex.
- In-flight loads map each absent key to a load record containing a `done` channel plus the eventual immutable copied bytes and error. Load record fields are written before closing `done`; waiters read them after channel synchronization. In-flight records are distinct from resident entries and consume no capacity.
- The cache-scoped context is created from `context.Background()` in `New`; no caller context is its parent. Close cancels it.
- The wait group tracks every launched Loader goroutine. Under the mutex, a successful miss registers its goroutine before it can be observed by Close; Close prevents new starts by setting `closed` under that same mutex before it waits.

## Operations and rule mapping

### `Get(ctx, key)`

Under the mutex, check `closed` first and return `ErrClosed` if set. On a resident hit, move the entry to MRU, make a fresh copy of its bytes, unlock, and return it without calling the Loader. If there is already a load for the key, retain its record, unlock, and wait. Otherwise create/register a load record, add it to the in-flight map, account for it in the wait group while still locked, unlock, and start one Loader goroutine.

The Loader goroutine calls the Loader with only the cache-scoped context. On success it copies the Loader slice immediately so the cache does not retain caller-owned Loader memory. It then locks the cache, checks whether the cache-scoped context has been cancelled/Close has begun, and either discards the result or admits it into the resident LRU. Admission evicts the current LRU entry first when at capacity. A successful admission marks the key MRU. Errors (including a non-nil byte slice with an error) are published without admission; normalize the result bytes to nil when error is non-nil. Remove the in-flight map entry and publish the record's bytes/error before closing `done`, then unlock and finish the wait-group task. Locking publication and removal prevents duplicate in-flight loads and ensures every waiter observes one completed result.

Both the initiating caller and joined callers use the same waiting path. Wait for `done`, caller-context cancellation, or cache closure. Preserve the specified priority when events race: first observe closed state and return `ErrClosed`; next, if this caller's context is done while this load is unresolved, return `ctx.Err()`; otherwise, once resolved return the load result, with a new copy for this particular Get. Recheck state under the mutex when selecting among concurrently ready events so selection order does not depend on Go `select` randomness. A caller leaving due to its own context does not cancel or remove the load; it continues to completion and may populate the cache unless Close has cancelled the cache context.

### `Len()`

Lock, read the resident map length, unlock. In-flight loads are excluded. Since admission and eviction are atomic under the same lock and admission evicts before inserting, Len never exceeds capacity. A concurrent poll may observe the count immediately before or after completion, consistent with the stated allowance.

### `Close()`

Under the mutex, the first caller sets `closed`, cancels the cache-scoped context, and clears resident map/list state. It leaves in-flight records available so current waiters can be awakened with the required closed result. All callers then wait on the same wait group, so concurrent and later Close calls do not return before the first close's Loader join is complete. The first close path does not hold the mutex while waiting. Loaders are required by contract to return promptly when their cache context is cancelled. Once all Loader calls return, Close returns nil. Every Get checks `closed` before starting or joining work and every waiter prioritizes the closed flag, so current waiters wake with `ErrClosed` and later Gets do not invoke the Loader. A Loader finishing after cancellation publishes no resident value, including when no waiters remain or completion races with Close.

## Test approach

Use the supplied public acceptance suite as the baseline and run it normally and under `go test -race`; run `go vet` as required by the deliverable. Add focused package tests as needed for uncovered exact semantics:

- many overlapping Gets for one key cause one Loader call, with independent returned slices;
- Loader error wins over accompanying bytes, is shared by current waiters, and is not cached;
- loader-owned slice mutation after return cannot change resident data;
- LRU hit and successful-load recency, capacity bounds, and in-flight loads excluded from Len/eviction;
- caller cancellation returns that caller's context error while the shared load continues and later populates the cache;
- Close wakes current waiters with `ErrClosed`, cancels and joins loaders, discards a completion racing with Close, clears residents, rejects later Gets, and concurrent Close calls all await the same join;
- race-heavy concurrent Get/Len/Close activity to validate synchronization and deadlock freedom.

Tests involving coordination should use channels and bounded contexts (and `testing/synctest` where available) rather than timing sleeps. Check errors with `errors.Is` where wrapping is permitted.
