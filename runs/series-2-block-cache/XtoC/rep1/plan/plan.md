# Implementation Plan: `blockcache`

## Public API

Implement package `blockcache` with the exact exported surface from the spec:

- `type Loader func(ctx context.Context, key string) ([]byte, error)`
- `type Cache interface { Get(context.Context, string) ([]byte, error); Len() int; Close() error }`
- `func New(capacity int, load Loader) Cache`
- `var ErrClosed = errors.New("blockcache: closed")`

`New` panics for capacity below one or a nil loader. The concrete implementation remains unexported.

## Internal data model

One concrete cache owns a mutex, immutable capacity and loader, a cache-scoped context and cancel function, a `closed` flag and close notification channel, and two maps/state structures:

1. Resident entries are indexed by key and ordered by `container/list` from least to most recently used. Each list element holds a key and the cache-owned byte slice. The map provides O(1) lookup and the list supports O(1) recency updates and eviction.
2. In-flight loads are indexed by key. Each flight holds a completion channel, a resolved flag, a cache-owned result slice and error. Its Loader runs in its own goroutine, independent of any caller context. A wait group tracks Loader goroutines.

The mutex protects all shared state, including flight resolution, closed state, resident membership and LRU order. Flight result bytes are immutable after publication. Every return path that supplies successful bytes copies them for that caller.

## Rule-to-design mapping

1. **Hit and copy:** Under the mutex, check closed state and then resident membership. Move a resident element to the MRU end and return a copy of its bytes. A hit does not invoke the Loader.
2. **Single-flight miss:** For an absent key, join its existing flight or create exactly one flight under the mutex. Starting a new flight and incrementing the wait group occur while holding that mutex, after the closed check. The goroutine invokes the Loader with the cache-scoped context. Waiters share its immutable outcome, but each copies successful bytes before returning.
3. **Errors:** On Loader error, store the error in the flight and do not admit bytes, including when the Loader returned both bytes and an error. Remove the flight from the in-flight map when resolved so a later Get starts a fresh load. All waiters already holding the flight observe the same error.
4. **Ownership:** Copy Loader bytes immediately after it returns and before publishing them. Do not retain its slice. Store only the copied slice; copy again on each successful Get return so mutations cannot cross callers or alter the resident value.
5. **Capacity and resident-only Len:** An unresolved flight is absent from the resident map and list. On successful resolution, under the mutex, admit the result only if the cache remains open and its cache context is not canceled. If at capacity, remove the LRU element before inserting the new MRU entry. Consequently Len (the resident-map length read under the mutex) is never above capacity and excludes flights.
6. **Recency:** Move hits to MRU. Successful admissions enter at MRU. Error results do not affect resident recency.
7. **Caller cancellation and orphaned work:** A caller waiting on a flight selects among its own context, flight completion and the cache close notification. Cancellation only ends that caller's wait; it never cancels the shared load. The load continues with the cache context and may populate the cache even if all callers leave. When a waiter wakes, it reacquires the mutex and applies the defined priority: closed cache returns `ErrClosed`; otherwise, if the flight remains unresolved and the caller context is done, return `ctx.Err()`; otherwise return the resolved flight outcome. This state check also handles simultaneous channel readiness deterministically.
8. **Close:** Under the mutex, the first Close marks the cache closed, cancels the cache context, closes the close notification channel, and removes all resident entries. All Gets not yet past the closed check then return `ErrClosed`. The first Close releases the mutex and waits for the Loader wait group. Concurrent Close calls observe closed state and wait on the same wait group, so none returns before all Loader calls have returned. Loader completion checks closed state/context cancellation under the mutex and discards the result instead of admitting it. All current waiters are woken by the close notification and return `ErrClosed`, regardless of any simultaneously completed flight. Repeated Close calls return nil.
9. **Concurrency and lifecycle:** A mutex serializes state transitions; no Loader call runs while holding it. Starting flights under the same mutex used to mark closed prevents a `WaitGroup.Add` from racing with Close's wait after shutdown begins. Resolving a flight sets its outcome and closes its completion channel while holding the mutex, then removes it from the active-flight map. No path waits for a Loader while holding the mutex, avoiding lock/join deadlock.

## Test approach

Run the supplied public suite, then run it with the race detector and run `go vet` as required by the deliverables. Add focused tests for semantics not directly covered by the public suite:

- Loader `(bytes, error)` discards bytes, and a later Get retries.
- Distinct returned slices for concurrent waiters and for repeated hits; mutating any returned slice leaves cache data unchanged.
- Caller cancellation returns that caller's context error while another waiter remains, and the shared load still populates the cache after every waiter cancels.
- Close wakes blocked waiters with `ErrClosed`, cancels the Loader context, waits until a blocked Loader returns, discards its late success, clears resident entries, and remains safe for concurrent/repeated Close calls.
- Cancellation, resolution, and Close races obey the documented priority and preserve capacity; concurrent Len polling never observes a value above capacity.
- Invalid constructor arguments panic.

Use channel barriers to control Loader entry/release and bounded contexts for waits. Repeat concurrency-sensitive cases with `go test -race`; use the public `testing/synctest` overlap test as the deterministic single-flight check.
