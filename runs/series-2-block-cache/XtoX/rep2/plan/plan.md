# Implementation plan: bounded concurrent single-flight block cache

## Scope and package API

Implement package `blockcache` in the Go module (`go 1.25`). The exported API will match the spec:

- `type Loader func(ctx context.Context, key string) ([]byte, error)`
- `type Cache interface` with `Get(context.Context, string) ([]byte, error)`, `Len() int`, and `Close() error`
- `func New(capacity int, load Loader) Cache`
- `var ErrClosed = errors.New("blockcache: closed")`

`New` panics for capacity below one or a nil loader. `Get` and `Close` return `ErrClosed` as specified; callers may use `errors.Is`.

## Internal data model

Use one concrete, unexported cache type guarded by a single `sync.Mutex`. The cache owns:

- immutable `capacity` and the `Loader`;
- a resident map from key to an entry, plus a `container/list` ordered least- to most-recently-used. Each entry stores key, owned byte slice, and its list element;
- an `inflight` map from key to a flight record. A flight has a completion channel and final owned result bytes/error. Flights are separate from residents, so unresolved loads consume no capacity;
- a cache-scoped context and cancel function, created from `context.Background()`;
- a `closed` flag, a `closing` signal closed as soon as shutdown begins, a `closeDone` signal closed only after all loaders join, and a `sync.WaitGroup` tracking loader goroutines.

All changes to `closed`, resident/LRU state, flight map/state, and result publication occur under the mutex. A loader goroutine calls the user loader without holding the mutex. It then copies successful bytes into cache-owned memory, reacquires the mutex, and either publishes the result and admits it to the resident LRU (if still open), or discards the bytes (if closing/closed). It records the final flight result, removes the flight from the map, closes the flight completion channel, unlocks, and returns. A successful result retained in a flight is owned by the cache; every recipient copies it before returning.

## Operation and rule mapping

### Construction

Validate inputs before allocating state. Initialize the resident and flight maps, LRU list, cache context, and lifecycle channels. This enforces the constructor preconditions and gives every loader the same cache-scoped cancellation context (rules 7 and 8).

### Get

Under the mutex, check `closed` first and return `ErrClosed` if set. Then check the resident map: on hit, move the entry to the LRU tail and return a fresh copy. Otherwise, join the existing flight for the key or create one, add it to the in-flight map, increment the wait group, and launch exactly one loader goroutine. No loader runs under the caller's context.

Wait outside the mutex on the flight completion, this caller's `ctx.Done()`, or the cache's `closing` signal. After any wake, reacquire the mutex and make the decision in the required order: closed means `ErrClosed`; otherwise, if this caller's context is done and the flight remains unresolved, return `ctx.Err()`; otherwise, return the resolved flight error or a fresh copy of its result bytes. If cancellation and completion race, mutex-protected flight state determines whether the load was still in flight. A caller leaving does not cancel or remove the flight, so orphaned loads continue and may populate the cache (rule 7).

### Loader completion and errors

Treat any non-nil loader error as authoritative: publish the error, publish no bytes, and do not add a resident entry, including when the loader returned bytes with the error. A later Get therefore starts a new flight. On success, copy the loader's slice before retaining it. Publish that cache-owned result to all flight waiters; each waiter makes a distinct copy. If shutdown has started before completion commits, publish a discarded result to waiters and do not store the key. This also ensures Close cannot be followed by a late cache insertion (rules 2–4 and 8).

### Capacity and recency

Only successful, open-cache completions enter the resident map/LRU. At admission, if the cache is full, remove the list head and its map entry, then append the new entry at the tail. Hits and successful admissions both move/add keys to the MRU tail. Since insertion and eviction happen in one mutex critical section and every insertion evicts first when full, `Len` cannot exceed capacity. `Len` takes the mutex and reports resident-map size only; flights do not contribute (rules 5 and 6).

### Close

The first caller to Close takes the mutex, marks the cache closed, closes `closing` to wake current waiters, cancels the cache context, clears the resident map and LRU, then unlocks and waits for the loader wait group. It closes `closeDone` after the join. A concurrent or later Close sees the closed flag, unlocks, waits on the same `closeDone`, and returns nil. Thus Gets observe closure promptly, late loader completions cannot store data, entries are dropped, and every Close waits for all loader calls (rule 8). The Loader contract requires prompt return after cancellation; Close's join relies on that contract.

## Test approach

Use the supplied public suite as the acceptance baseline and run it in the module with ordinary `go test ./...`, `go test -race ./...`, and `go vet ./...` once implementation work begins. The suite already checks copy isolation on hits, deterministic same-key single-flight overlap, error retry behavior, LRU eviction, resident length bounds, constructor panics, and basic closed/idempotent behavior.

Add focused tests for specification cases not fully covered by the public suite:

1. Two concurrent waiters get equal but non-aliasing byte slices; mutating one result does not affect the other or a later hit. Verify loader-owned input is copied.
2. A loader returning both bytes and an error returns nil bytes/error to all waiters and is called again on a later Get.
3. Cancel one waiter while another remains; the canceled Get returns its own context error, the loader remains active, and the remaining waiter plus a later hit receive the successful value.
4. Cancel all waiters; confirm the orphaned load continues and successful completion becomes resident.
5. Close during an in-flight load: all current waiters get `ErrClosed`, loader context is canceled, Close does not return until the loader exits, the completion is not cached, and subsequent Gets do not invoke the loader.
6. Race multiple Close calls while a loader is joining; all calls return only after that loader exits. Also test close-versus-completion and close-versus-caller-cancellation priority.
7. Keep a load blocked while calling `Len`; it must not count as resident. Resolve enough distinct keys under concurrent access and assert the bound is never exceeded.

Use channels and explicit barriers for these lifecycle tests rather than timing sleeps. Run race detection to validate shared result publication, waiter behavior, and Close coordination.
