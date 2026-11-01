# Task: Bounded Concurrent Single-Flight Block Cache (Go)

> Complete, public task specification handed verbatim to the implementor. Everything required to
> produce a correct implementation is stated here.

Package `blockcache`: a bounded, concurrent, single-flight block cache in front of a slow backing
store. Everything required is stated here.

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

**Exact semantics.**
1. **Hit** — key resident: return a **copy** of its bytes, mark MRU, no Loader call.
2. **Single-flight miss** — concurrent Gets for the same absent key: exactly one Loader call; all
   waiters receive the same result, **each as its own copy** (rule 4). On success the bytes become resident.
3. **Error** — Loader returns non-nil error: every waiter gets it; the key is not stored; a later Get
   re-loads. If a Loader returns `(non-nil bytes, non-nil err)`, the error wins and the bytes are
   discarded.
4. **Ownership / copy** — the cache copies the Loader's returned bytes into its own storage and does
   not retain the Loader's slice; **every Get returns a fresh copy**. Callers may mutate what Get
   returns without affecting the cache or any other caller.
5. **Capacity / LRU** — at most `capacity` resident entries; admitting a new entry when full evicts the
   least-recently-used resident entry first (ties: any least-recent one). A not-yet-resolved load does
   not count toward capacity and is not evictable; it becomes resident, and evictable, only once it
   resolves successfully. `Len()` is resident-only, may lag a completion by one critical section, and
   is never above capacity even under a concurrent poll.
6. **Recency** — a hit and a successful load each mark the key most-recently-used.
7. **Caller context & orphaned loads** — the Loader runs under a cache-scoped context, never a single
   caller's. A load **continues even if all its waiters leave**, and its success populates the cache;
   an in-flight load is therefore not a leak. A waiting Get returns, in priority: (a) `ErrClosed` if the
   cache closes; else (b) `ctx.Err()` if its own ctx is Done while the load is still in flight; else
   (c) the load's `([]byte, error)` once resolved.
8. **Close** — cancels the cache-scoped context, **waits for every in-flight Loader call to return**,
   wakes every current waiter with `ErrClosed`, drops all entries, returns nil; idempotent (a second
   Close arriving while the first is still joining waits for that same join and returns nil). A Loader
   that returns after the cache-scoped context is cancelled has its result **discarded, not stored**
   (this covers the zero-waiter and the close-window cases, so "drops all entries" holds). A Get that
   has not yet passed the closed check returns `ErrClosed` and does not call the Loader.
9. **Concurrency** — all methods are safe for concurrent use; the implementation is free of data races
   (graded under `go test -race`) and must not deadlock.

**Deliverables.** The `blockcache` package; builds, `go vet`-clean, passes the public suite incl under
`-race`; idiomatic, maintainable Go.
