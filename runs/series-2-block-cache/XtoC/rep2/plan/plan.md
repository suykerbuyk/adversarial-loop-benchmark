# Implementation plan: bounded concurrent single-flight block cache

## Public API

Implement package `blockcache` with the API fixed by the specification:

- `type Loader func(ctx context.Context, key string) ([]byte, error)`
- `type Cache interface { Get(context.Context, string) ([]byte, error); Len() int; Close() error }`
- `func New(capacity int, load Loader) Cache`
- `var ErrClosed = errors.New("blockcache: closed")`

`New` panics for a capacity below one or a nil loader. `Get` returns byte slices owned by the caller. `Close` is idempotent and always returns nil.

## Internal data model and synchronization

Use a concrete unexported `cache` guarded by one `sync.Mutex`. Its fields are:

- immutable `capacity` and `load`;
- `entries map[string]*list.Element` and an LRU `list.List`; each list value stores a key and the cache-owned byte slice;
- `inflight map[string]*flight`; a flight stores a completion channel, result bytes, and error;
- a cache-scoped `context.Context` and its cancel function;
- `closed bool`, a `closeDone` channel, and a `sync.WaitGroup` for loader goroutines.

The mutex protects all maps, list operations, flight result publication, `closed`, and wait-group admission. Loader calls happen outside the mutex. A new flight increments the wait group while holding the mutex, before releasing it, so `Close` cannot begin `Wait` before all accepted loader work has been registered. Each loader goroutine publishes its flight outcome under the mutex, removes the matching in-flight map entry, then closes the flight completion channel and calls `Done`. The completion channel provides a happens-before edge to waiters reading the published result.

The cache context is created once in `New`, never derived from a caller context, and shared by every loader. Flights are independent of waiter counts: caller cancellation never cancels a flight.

## Operation behavior and specification rules

### `Get`

Under the mutex, first reject a closed cache with `ErrClosed`. If the key is resident, move its LRU element to the front and return a fresh copy of its bytes. If a flight exists, retain it as this caller's flight. Otherwise create and register a flight and start exactly one loader goroutine after registering its wait-group work.

Wait for the flight completion, the cache-wide close notification, or the caller context. After any wakeup, reacquire the mutex and resolve outcomes in required priority order:

1. `closed` means return `ErrClosed`;
2. if the caller context is done and the flight has not completed, return `ctx.Err()`;
3. otherwise return the completed flight's error or a fresh copy of its result bytes.

This locked recheck handles simultaneous channel readiness and makes the stated priority deterministic. Every successful load is installed as resident before its completion is published, unless the cache was closed or its cache context was cancelled. Thus even if every waiter leaves, the loader completes and a successful value can populate the cache. Successful resolution updates recency by admitting the key at the MRU end.

### Loader completion, errors, ownership, and capacity

Invoke the loader with the cache-scoped context. On any non-nil error, discard returned bytes, publish the error to all current flight waiters, and leave the key absent so a later `Get` retries. On success, clone the loader's slice before retaining it; never retain the loader-owned backing array. Empty and nil successful byte slices are valid values. Before admission, check under the mutex that the cache remains open and the cache context has not been cancelled. If it has been cancelled, publish `ErrClosed` for current waiters and discard the result.

Admit only resolved successful values. If the key is not already resident and the list is at capacity, remove the back (least-recent) element from both LRU list and map, then insert the new entry at the front. This keeps `Len` at or below capacity at every mutex release. A hit moves the element to the front; successful admission also makes the key MRU. `Len` locks and returns the resident map size only; flights do not contribute.

### `Close`

The first caller takes the mutex, marks the cache closed, cancels the cache context, clears the entries map and LRU list, and closes the cache-wide notification channel; it then releases the mutex, waits for all registered loader goroutines, and closes `closeDone`. Concurrent or later callers that see `closed` release the mutex and wait on `closeDone`, so all return only after the same loader join has finished. Loader completion checks both `closed` and the cancelled cache context before storing, preventing a completion racing with close from repopulating the cache. Gets that begin after the closed flag is set fail before hitting, joining, or creating a flight.

## Rule-to-design mapping

1. **Hit:** lookup under mutex, promote to MRU, return a clone, and do not invoke the loader.
2. **Single flight:** one map entry per absent key while a load is active; all overlapping Gets join its completion channel and clone its shared result independently.
3. **Error:** publish the same loader error to all joined callers, discard any accompanying bytes, and remove the failed flight so a later Get retries.
4. **Copy/ownership:** clone loader output before cache retention and clone again for every successful Get, including each flight waiter.
5. **Capacity/LRU:** only resolved success enters the LRU; evict the list tail before inserting when full. The mutex makes the resident count bound true during concurrent `Len` calls.
6. **Recency:** hits and successful admissions move the key to the LRU front.
7. **Caller contexts/orphans:** loaders use only the cache context. Caller cancellation only terminates that caller's wait. Outcome rechecks enforce closed, then caller cancellation while unresolved, then result.
8. **Close:** one close transition cancels, notifies, clears entries, and joins every accepted loader; all close callers wait on one completion channel. Completion after cancellation cannot be admitted. Closed checks prevent new work.
9. **Concurrency:** a single mutex protects mutable cache state; channels publish immutable flight outcomes; wait-group additions happen before releasing the mutex and before any close can wait. No loader runs while holding the mutex.

## Test approach

Run the supplied public suite, including `TestHitReturnsCopy`, `TestSingleFlight`, `TestErrorNotCached`, `TestLRUEviction`, `TestLenBounded`, `TestClosedReturnsErrClosed`, and `TestNewPanics`. Run it under the race detector as required by the task, and run `go vet` on the package.

Add focused tests for semantics not fully covered by the public suite:

- loader returns bytes and error together; verify error wins and a later call retries;
- mutate loader-owned bytes after return and mutate one caller's result; verify neither affects cached data or another caller;
- in-flight loads do not affect `Len`, and successful resolution under pressure evicts the true LRU without ever exceeding capacity;
- cancel one waiter and then all waiters; verify each canceled caller gets its own context error while the load continues and eventually caches its result;
- close with blocked loaders and multiple waiters; verify waiters receive `ErrClosed`, loader contexts are canceled, Close waits for loader return, late success is discarded, entries are cleared, and simultaneous/repeated Close calls all wait for completion;
- exercise close-versus-completion and caller-cancel-versus-completion schedules repeatedly under `-race` to validate the specified priority and absence of repopulation.

Use channels and explicit barriers for deterministic interleavings, with bounded test contexts/timeouts to expose deadlocks. Keep tests within package `blockcache` where necessary to assert internal state only when behavior cannot be established through the public API.
