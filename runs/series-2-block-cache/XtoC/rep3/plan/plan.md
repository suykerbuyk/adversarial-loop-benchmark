# Implementation plan: bounded concurrent single-flight block cache

## Public API

Implement package `blockcache` with the specified exported API:

- `type Loader func(ctx context.Context, key string) ([]byte, error)`
- `type Cache interface { Get(context.Context, string) ([]byte, error); Len() int; Close() error }`
- `func New(capacity int, load Loader) Cache`
- `var ErrClosed = errors.New("blockcache: closed")`

`New` panics if capacity < 1 or load is nil. `Get` returns nil bytes with an error. Successful empty results are copied with `make([]byte, len(src))` and `copy`, so even a nil or zero-length Loader result produces a non-nil empty slice. Every successful Get receives a distinct slice.

Document on `Loader` that it must promptly observe its cache-scoped context; calling `Close` from inside a Loader deadlocks because Close joins that Loader, and recursively calling `Get` for the same key waits on its own flight and may block until the caller context ends.

## Internal data model and invariants

Use an unexported implementation with one mutex protecting lifecycle state, resident map/list state, and flight-map membership:

- immutable capacity and Loader;
- cache-scoped context and cancel function shared by all Loader invocations;
- `closed` flag, `closedCh` closed exactly once when shutdown begins, and `closeDone` closed once all Loader invocations have joined;
- resident map keyed by string; each entry owns bytes and a list element;
- `container/list` ordered LRU at front and MRU at back;
- in-flight map keyed by string; each flight has `done`, result bytes, and error;
- WaitGroup counting started Loader invocations.

Resident bytes and a successful flight's result bytes are immutable after publication. They may share one cache-owned slice; all hits and waiters copy it before returning, and no internal path mutates it in place. Loader-owned bytes are copied before retention or publication. Flight result fields are set before closing `done` and never changed afterward; channel close synchronizes readers. A flight is never resident capacity until successful resolution.

All flight creation and WaitGroup `Add` operations occur under the state mutex, and no Add is permitted after `closed` becomes true. The first Close sets `closed` under that mutex before it begins `Wait`, preventing Add/Wait misuse.

## `Get`

1. Lock. If closed, unlock and return `ErrClosed` without starting or joining work. This is the admission check. If the key is resident, move it to MRU, copy the stored bytes while locked, unlock, and return the copy.
2. If a flight exists, keep its pointer; otherwise create and publish one and increment the WaitGroup while still locked. Unlock. The creator starts a dedicated Loader goroutine; all callers, including the creator, use the same wait procedure.
3. Wait on exactly three events: `flight.done`, the caller's `ctx.Done()`, and `closedCh`. The wait case for shutdown is `closedCh`, not the cache-scoped Loader context; caller cancellation only abandons that caller and never cancels the shared load.
4. On any wake, apply the required priority. First check shutdown via `closedCh` (a non-blocking receive; it is closed under the same mutex transition that sets `closed`). If closed, return `ErrClosed`. Otherwise non-blockingly check `flight.done`; if resolved, return its result (successful bytes copied freshly, or nil and the Loader error). Only if it remains unresolved, return `ctx.Err()` when the caller context is done; otherwise loop/wait again. Thus a completed flight beats a later caller cancellation, while cancellation wins only while the flight remains in flight. A close racing the check can only make the closure check linearize before closure; subsequent wake/check observes it.

This ordering implements rule 7: close has highest priority; caller cancellation applies only while unresolved; a resolved result wins over cancellation. The caller's initial closed check ensures Gets admitted before Close may later receive `ErrClosed` from the shutdown wake.

## Loader execution and completion

Invoke Loader only in a dedicated goroutine with the cache-scoped context, never a caller context. Put completion and WaitGroup `Done` in a defer so abnormal goroutine exit cannot strand the flight or make Close wait forever. Normal return records `(bytes, err)`. A non-nil error wins: discard accompanying bytes, do not cache, and publish nil bytes plus the original error. On success, copy the Loader slice immediately, including to detach from Loader-owned storage.

Completion bookkeeping under the mutex checks that shutdown has not started and the cache-scoped context is not canceled. If open, successful bytes are admitted: when full, remove the LRU resident first; insert as MRU. Error results are never admitted. Regardless of shutdown, publish the terminal flight result, remove the flight from the in-flight map, and close its `done`, then unlock. If shutdown has begun, discard successful bytes from residency; current waiters receive `ErrClosed` by priority. Call `WaitGroup.Done` in the deferred completion path.

If Loader panics, re-panic after deferred bookkeeping so the runtime's normal panic behavior is preserved; a deferred recovery is used only to guarantee the flight is terminal and Done runs, not to silently convert programmer panics into Loader errors. If Loader invokes `runtime.Goexit`, deferred cleanup still runs; publish a terminal internal error for waiters so they do not strand on a flight with no result. Keep this exceptional behavior internal and ensure it is documented in implementation comments.

Only one flight exists per absent key, including while other waiters cancel. Therefore orphaned work continues and a successful open-cache result populates residency.

## `Len`

Lock, return the resident map length, and unlock. Insertions and evictions are performed under that same lock, so Len is resident-only and never above capacity; it may observe state immediately before or after completion admission.

## `Close`

Under the mutex, the first caller sets `closed`, cancels the cache-scoped context, closes `closedCh` to signal shutdown, and clears all resident entries and the LRU list. It then unlocks and waits for the Loader WaitGroup. Only after all Loader calls return does it close `closeDone`. Concurrent/subsequent Close callers observe shutdown, wait for the same `closeDone`, and return nil. This follows the specified cancel, join, waiter wake, drop/empty-on-return lifecycle: waiters are signaled as soon as shutdown begins so they can return `ErrClosed`, while Close itself does not return until all Loaders have exited. Since residents are cleared at shutdown start and completion rejects admission whenever closed or the cache context is canceled, late results cannot repopulate the cache; Len is zero after Close returns.

The WaitGroup count is established before releasing the mutex for each started Loader, and no new count can be added after shutdown begins, so concurrent Close calls safely join the same work.

## Test approach

Use the supplied public suite as the baseline. Add deterministic channel-barrier tests for requirements it does not cover:

- error plus non-nil bytes discards bytes and a subsequent Get retries;
- each simultaneous waiter gets an independent slice; mutating Loader-owned storage after return cannot affect cache or callers;
- nil and empty successful results have the documented non-nil empty representation;
- one canceled waiter leaves the shared Loader running; all waiters may cancel and a later Get hits the orphaned successful result;
- exact rule 7 boundary: when `flight.done` is closed before caller cancellation, result wins; when cancellation is observed while the flight remains unresolved, `ctx.Err()` wins; closure beats both;
- Close cancels Loader context, wakes waiters with `ErrClosed`, and does not return while a barrier-held Loader remains active; after releasing a Loader that ignores cancellation and returns success, Close returns with `Len()==0`;
- concurrent Close callers all wait for the same join; Gets racing shutdown cannot start a post-close Loader or repopulate residency;
- capacity-one and access sequences validate LRU and bounded Len.

Use channels/barriers (and `testing/synctest` where useful), not timing sleeps, for ordering assertions. Run `go test ./...`, `go test -race ./...`, and `go vet ./...` after implementation, alongside the public suite.
