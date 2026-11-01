# Implementation plan: bounded concurrent single-flight block cache

## Public API

Implement package `blockcache` with the API from the specification:

- `type Loader func(ctx context.Context, key string) ([]byte, error)`
- `type Cache interface { Get(context.Context, string) ([]byte, error); Len() int; Close() error }`
- `func New(capacity int, load Loader) Cache`
- `var ErrClosed = errors.New("blockcache: closed")`

`New` panics for `capacity < 1` or a nil loader. To avoid starting/registering work before discovering an invalid caller context, `Get(nil, key)` will panic immediately, consistent with Go's context-taking API convention; this is an explicit caller contract, not an accidental panic in a `select` after a load has started.

## Internal data model

Use one mutex to protect lifecycle state, in-flight records, and LRU state:

- `capacity int`, `load Loader`
- a cache-scoped `context.Context` and cancel function, created from `context.Background()`
- `closed bool`, set once under the mutex
- `closeDone chan struct{}`, closed only after all loader goroutines have joined and entries/maps have been cleared; concurrent and later `Close` calls wait on it
- `flights map[string]*flight`, containing only unresolved or just-completing loads, not resident capacity
- resident map from key to `{data []byte, element *list.Element}` plus `container/list` with front = LRU and back = MRU
- a `sync.WaitGroup` for loader goroutines; `Add(1)` occurs while holding the mutex before a flight is made visible, and after `closed` is set no new Adds can occur

Each `flight` has a `done` channel, owned result bytes and error. Its fields are written before closing `done` and read after observing that close (or while holding the cache mutex). Keep an identity pointer so completion deletes the map entry only if it still points to that exact flight.

## Operations and rule mapping

### Get

1. Reject nil context immediately. Lock; if closed, unlock and return `ErrClosed`.
2. If the key is resident, move its list element to the back (MRU), make a fresh copy of stored bytes, unlock, and return it.
3. If a flight exists, retain its pointer and unlock. Otherwise create/register a flight while locked, increment the WaitGroup, unlock, and start exactly one goroutine running the loader with the cache-scoped context. Caller contexts are never passed to the loader.
4. A waiting caller selects among flight completion, cache closure notification, and its own `ctx.Done()`. After any wake, lock and decide in this strict order:
   - if `closed`, return `ErrClosed`;
   - if this flight is still unresolved and `ctx.Err() != nil`, return that caller's context error;
   - if the flight is done, return its error or its own fresh copy of the flight result;
   - otherwise unlock and wait again.

This locked recheck handles simultaneous notifications and implements the rule that caller cancellation wins only while the load is still in flight; once resolved, the load result is used unless the cache has closed. A caller leaving does not cancel or remove a flight, so orphaned loads continue and can populate the cache.

### Loader completion

The loader goroutine calls the configured loader with the cache-scoped context. On success, copy its bytes into cache-owned storage; preserve nil exactly (`nil` remains nil, including on successful nil payloads). On error, discard any accompanying bytes. Under the mutex, if the cache is not closed and the cache-scoped context has not been canceled, publish successful bytes to the resident LRU: evict the front entry first when at capacity, insert at the back as MRU, and ensure resident count never exceeds capacity. Errors are never admitted. If closure/cancellation has begun, discard the result and do not repopulate entries.

On every terminal path, including a recovered loader panic, publish the flight's terminal result, delete that exact flight from `flights`, and close `done` exactly once. If panic recovery is implemented (planned here to protect lifecycle guarantees), convert the panic to an internal error usable by all current waiters; a panic is treated as a terminal loader failure, not allowed to strand the WaitGroup or flight. Arrange cleanup so the WaitGroup `Done` always runs, including if result handling itself exits unexpectedly. Normal loader errors are returned unchanged.

### Len and LRU

`Len` locks and returns the resident map length only. Since admission and eviction happen together under the same lock and admission evicts before insertion, the value is always bounded. Hits and successful admissions move or insert the key at the MRU end. In-flight loads do not appear in the resident map/list and consume no capacity.

### Close

Under the mutex, the first caller sets `closed`, closes a closure notification channel used by waiters, and cancels the cache-scoped context; it then unlocks and waits for all registered loader goroutines. Later callers observe `closed`, unlock, and wait for the same `closeDone`. After the WaitGroup joins, the first caller locks, clears residents and the LRU list, clears the flights map, unlocks, then closes `closeDone`. Completion checks closed/canceled state before admission, so a loader returning after cancellation cannot repopulate the cache. Waiters check `closed` first after every wake and therefore return `ErrClosed`. Gets that begin after the closed check return `ErrClosed` without invoking the loader. `Close` returns nil and is idempotent.

The loader contract requires prompt return after its context is canceled; Close necessarily waits for that return. The map clear after the join also ensures shutdown leaves no retained flight records, including defensive cleanup for terminal paths.

## Test approach

Run the shipped public tests as the acceptance baseline, including `go test -race` and `go vet` for the package. Add focused tests for the rules not fully asserted by the public suite:

- Loader receives a cache-scoped context; cancellation of one/all waiting callers does not cancel the loader, and its eventual success becomes a hit.
- Caller cancellation while unresolved returns that caller's `ctx.Err()`; if the cache closes, `ErrClosed` takes priority; if completion is already observed and the caller context is also done, the resolved result wins.
- Loader returning non-nil bytes with an error returns the error and does not cache bytes; a later Get reloads.
- Nil successful payload remains nil on the first result, a coalesced waiter result, and subsequent resident hits; non-nil results are independently mutable copies.
- Close during a blocked load wakes all waiters with `ErrClosed`, cancels the loader context, waits for loader return, discards late success, clears residents, and concurrent Close calls wait for the same join.
- Loader panic (if recovered as planned) terminates the flight, releases Close, and allows a later retry without retaining a stale map entry.
- Concurrent Len polling during successful admissions never exceeds capacity; stress concurrent hits, misses, cancellation, and Close under the race detector.

Use channels/barriers to force ordering in concurrency tests rather than timing assumptions; bound test waits and ensure every test closes the cache.
