# Implementation Plan: Bounded Concurrent Single-Flight Block Cache

## Public API

Implement package `blockcache` with the specified exports:

- `type Loader func(ctx context.Context, key string) ([]byte, error)`
- `type Cache interface { Get(context.Context, string) ([]byte, error); Len() int; Close() error }`
- `func New(capacity int, load Loader) Cache`
- `var ErrClosed = errors.New("blockcache: closed")`

`New` panics for `capacity < 1` or a nil loader. `Get` returns nil bytes with any error. `Close` returns nil and is idempotent, including concurrent calls.

## Internal data model

Use an unexported concrete cache. One mutex protects the closed flag, resident LRU/map, and in-flight map. The cache also holds its capacity, loader, a cache-scoped context created from `context.Background()` and its cancel function, a shutdown channel, and a `sync.WaitGroup` for loader goroutines.

Represent residents with `container/list`: the front is MRU, the back is LRU, and a map points keys to list elements. Each resident owns a byte slice copied from the loader result. Represent each in-flight key with a flight record containing a completion channel, `resolved` flag, owned result bytes, and error. Flight records are independent of the resident set and do not consume capacity. A Get that joins a flight retains that flight pointer throughout its wait and never looks it up again by key after waking.

Every field read or written by more than one goroutine is protected by the mutex, except channel state and WaitGroup operations, which use their documented synchronization. Increment the WaitGroup while holding the mutex before starting a loader. Close sets `closed` under that same mutex before any caller invokes `Wait`, so no later `Add` can race with or follow the join.

## Operation design and rule mapping

### Construction

Validate arguments and panic on invalid input. Initialize the scoped context, shutdown channel, empty LRU/map structures, and flight map. This implements the constructor contract.

### Get

Under the mutex, check `closed` first and return `ErrClosed` if set. For a resident key, move its element to the front, copy its bytes while locked, then return the copy (hit, ownership, recency, and closed-check rules).

For an existing flight, save its pointer, unlock, and wait for flight completion, shutdown, or the caller context. Regardless of which select case wakes it, reacquire the mutex and decide from current state, in this exact order: return `ErrClosed` if `closed`; otherwise return `ctx.Err()` only if the flight is still unresolved and the caller context is done; otherwise use the resolved flight's error or return a fresh copy of its owned bytes. Thus the selected wakeup case alone never determines whether cancellation wins. Copy resolved bytes while holding the mutex. The retained flight pointer ensures that an already joined waiter receives the outcome of that flight even if it has been removed from the in-flight map.

For an absent key with no flight, create and publish a flight under the mutex, `Add(1)` to the WaitGroup, unlock, and start one loader goroutine using only the cache-scoped context. The initiating Get then follows the same flight-wait path as joined callers. Caller cancellation exits only that Get; it neither cancels nor removes the shared load. These rules provide single-flight behavior, cache-scoped loading, and orphan-load continuation.

### Loader completion

Call the Loader without holding the mutex. If it returns a non-nil error, discard any bytes. On success, immediately copy the Loader's slice into cache-owned memory before publishing it to the flight.

Lock the cache and publish the result/error to the flight. On successful load, admit the owned bytes only if `closed` is false. If the key is already resident, replace that entry's bytes and move the existing list element to the MRU front; do not create a duplicate list element. If absent, push one new entry at the front. Evict from the LRU tail as needed to keep the resident count at or below capacity. The closed flag is authoritative for admission, so no completion can repopulate the cache after shutdown begins, including the interval before context cancellation is observed. Mark the flight resolved, remove its map entry only if it still points to this flight, close its completion channel while locked, then unlock and call `Done`.

The flight's owned bytes remain immutable after publication until all callers that retained its pointer have completed their copies; completion does not clear or reuse them. Waiters copy while holding the mutex. This provides independent result slices and avoids races between publication, waiter copies, eviction, and shutdown. Errors are shared and never cached; a later Get can start a new flight. Successful loads become resident and MRU atomically with resolution. Unresolved loads remain outside the LRU and cannot be evicted.

### Len

Lock, read the resident count (list length or resident-map length), and unlock. Insertions and evictions occur in one critical section, making Len race-free, resident-only, and never greater than capacity. It may observe either side of a completion critical section.

### Close

Under the mutex, the first caller sets `closed`, cancels the cache-scoped context, closes the shutdown channel, and clears the resident map and LRU. It then unlocks and waits for all loader goroutines. A concurrent or later caller observes `closed`, unlocks, and also waits on the same WaitGroup; it must not return merely because the shutdown channel is closed. After Wait returns, lock once more, clear residents again, and unlock before returning nil. Completion checks `closed` under this same mutex, so late results are discarded; the final clear establishes an empty resident set at the completion of every Close. No Add can occur after shutdown is marked because new Gets check `closed` under the mutex. Current waiters wake on shutdown and apply the required closed-first priority. This implements cancellation, joining, waiter wakeup, empty cache on return, and concurrent idempotence.

## Test approach

Use the supplied public acceptance suite as baseline. The implementation should be validated with the public suite under ordinary and race-enabled Go test runs, and with `go vet`. Add focused tests for requirements not fully exercised by that suite:

- Verify the Loader receives a cache-scoped context, and canceling every caller does not stop the load or prevent successful caching.
- Verify simultaneous waiters receive equal, distinct slices; mutations do not leak between callers or into the resident value.
- Verify `(bytes, error)` discards bytes and a later Get retries.
- Verify unresolved loads do not count toward capacity; completion admits with correct LRU behavior and concurrent Len polling remains bounded.
- Force caller cancellation and flight completion to race, checking under the mutex's resulting state that cancellation wins only while unresolved; check closed always has priority.
- Verify Close wakes waiters, cancels the loader context, waits until the loader exits, discards post-cancellation results, leaves Len zero, and concurrent Close callers all wait for the same loader join.
- Exercise Gets racing with Close to verify no load begins after the closed check and all calls that have not passed that check receive `ErrClosed`.

Use barriers/channels to force concurrency orderings and bounded contexts for deadlock detection rather than timing assumptions.
