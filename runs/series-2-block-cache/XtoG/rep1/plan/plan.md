# Implementation plan: bounded concurrent single-flight block cache

## Public API

Implement package `blockcache` with the exact exported surface in the spec:

- `Loader func(ctx context.Context, key string) ([]byte, error)`
- `Cache` interface: `Get(context.Context, string) ([]byte, error)`, `Len() int`, `Close() error`
- `New(capacity int, load Loader) Cache`
- `ErrClosed = errors.New("blockcache: closed")`

`New` panics when capacity is below one or `load` is nil. All successful Gets return independent byte slices; on any error Get returns nil bytes.

## Internal data model

Use a concrete cache with one mutex guarding the closed state, resident map and LRU list, in-flight map, flight completion and results, and shutdown state. Keep immutable capacity, loader, a cache-scoped context and cancel function, a loader `sync.WaitGroup`, and a `shutdownDone` channel created by `New`. Store residents in a map plus a doubly linked list ordered LRU-to-MRU.

A flight stores its key, `done` channel, completed flag, cache-owned immutable result bytes, and error. Do not keep a per-Get waiter registry: each Get unregisters by simply dropping its local reference before it returns, independently of loader lifetime. Close wakes Gets through flight completion/closure and the closed-state recheck. Loader calls are joined by the WaitGroup, independently of waiting Gets.

## Operations and rule mapping

### Get

1. Lock and check `closed` first. If set, unlock and return `ErrClosed` without touching the loader.
2. For a resident hit, move the entry to MRU, retain its immutable cache-owned slice reference, unlock, then copy and return it. Copying outside the lock keeps the critical section independent of block size.
3. On a miss, join the existing flight or create and publish exactly one flight under the lock. For a new flight, increment the loader WaitGroup before unlocking. Since Close changes `closed` under the same mutex before it starts waiting, no WaitGroup Add can race with Wait.
4. Wait only on the flight's `done` channel. Do not select on, derive from, or register an AfterFunc callback on the caller context. This ensures the wait remains durably blockable in `testing/synctest`, including when the caller context was made outside the bubble.
5. Once flight `done` closes, lock and apply result priority: closed cache returns `ErrClosed`; otherwise a completed load returns its result (copying successful bytes after unlocking); if the caller context is canceled while the flight is still unresolved, return `ctx.Err()`. Caller exit does not cancel or remove the shared flight, so zero-waiter work continues and successful results can populate the cache.

The cancellation state is checked on every Get return path, and it does not affect another caller or the loader context. The implementation must keep the caller's context available only for this final `ctx.Err()` check; the flight wait itself has no dependency on its Done channel.

### Loader completion

Run exactly one goroutine per newly published flight, calling the loader with the cache-scoped context created from `context.Background()` in `New`. If the loader returns a non-nil error, discard any bytes. On success, immediately copy the loader's slice into cache-owned storage; never retain or expose the loader's slice.

After the loader returns, acquire the cache mutex, then call WaitGroup.Done before releasing the mutex. This ordering ensures Close cannot finish its join until publication is complete, and because Close waits without holding the mutex there is no lock/join cycle. Under the mutex, if the cache remains open and the load succeeded, evict the LRU resident first when full, then insert the new key at MRU. A pending flight never consumes resident capacity. If closed, discard the result and do not admit it. Publish immutable result/error and completion state, remove the flight from the map if it is still the matching flight, and close `done` exactly once before unlocking.

### Len

Lock and return the resident count. Pending flights are excluded. Evict-before-insert under the same lock ensures Len never exceeds capacity.

### Close

The first caller locks, marks the cache closed, cancels the cache-scoped context, and closes each unresolved flight's `done` channel exactly once so all current waiters wake. It unlocks before waiting for loader completion. After all Loader calls have returned and published/discarded their results, it locks, clears resident map and LRU list, unlocks, and closes `shutdownDone` exactly once.

A later or concurrent Close that sees `closed` unlocks and waits on the already initialized `shutdownDone`, then returns nil. Thus all callers wait for the same join and cleanup. Get awakened by Close checks `closed` under the mutex first and returns `ErrClosed`. New Gets after closure return `ErrClosed`. Loader results that return after cancellation are discarded; no completion can insert after the closed transition. Close necessarily waits for the Loader contract that it returns promptly on cancellation.

### Ownership and concurrency

All resident and flight result byte slices are immutable after publication. Copying takes place outside the mutex while a local reference keeps the immutable slice alive. All shared maps, LRU links, flight states, and close transitions are mutex-protected; channel close is exactly once under that mutex. The mutex, flight channels, and WaitGroup provide synchronization for all concurrent operations.

## Test approach

Run the supplied public suite unchanged, plus `go test -race` and `go vet`. Add focused, bounded channel-driven tests for bytes-plus-error precedence; distinct copies for coalesced callers; loader-slice alias prevention; orphaned load population; active-load Close wakeup and result discard; simultaneous Close callers; pending loads excluded from Len/capacity; and concurrent LRU admission/completion. Use deterministic barriers rather than sleeps. In particular, retain the supplied synctest single-flight test as the check that Gets park on the flight signal without depending on an external caller-context channel.
