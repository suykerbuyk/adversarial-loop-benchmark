// Package blockcache provides a bounded, concurrent, single-flight LRU cache of
// byte blocks in front of a slow backing store.
package blockcache

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// Loader fetches the block for key. The cache calls it at most once per in-flight
// miss for a key (single-flight). It is given a cache-scoped context, not any
// single caller's. A Loader MUST return promptly when its ctx is Done: Close, and
// any Get caught by a closing cache, wait for every in-flight Loader to return.
//
// A Loader that panics or calls runtime.Goexit resolves its flight with an error;
// every waiter of that flight receives it and the key is not stored. A panic's
// error message includes the panic value and the Loader goroutine's stack trace,
// and if the panic value is an error it is reachable via errors.Is/As. The
// Goexit error is deliberately opaque (unexported); treat it like any other
// load failure.
//
// A Loader may Get other keys from its own cache (e.g. for composite blocks),
// but it must pass its ctx (or a ctx derived from it) so that the nested Get
// returns ErrClosed promptly when the cache closes. A nested Get under an
// unrelated, never-cancelled ctx would keep the Loader from returning and so
// stall Close. A Loader must not call Close on its own cache (Close would wait
// for itself and deadlock), and should not Get its own key (it would wait on
// itself until its ctx is Done).
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent, single-flight LRU block cache. All methods are
// safe for concurrent use.
type Cache interface {
	// Get returns a COPY of the block for key, loading on a miss. Concurrent Gets
	// for the same absent key cause exactly one Loader call; all receive its result.
	//
	// A hit never blocks, so ctx is not consulted. On a miss, a waiting Get
	// returns, in priority: ErrClosed if the cache closes; else ctx.Err() if ctx is
	// Done while the load is still in flight; else the load's result. A miss with
	// an already-cancelled ctx still starts (or joins) the load, which warms the
	// cache as an orphan load. Once Close has begun, a waiting Get returns
	// ErrClosed; it is normally woken only after Close has joined every Loader,
	// but if its own ctx is (or becomes) Done during that join it returns
	// ErrClosed at once, so a caller's deadline is never held hostage by slow
	// Loaders. A Get that reaches the closed check after Close has begun returns
	// ErrClosed at once without calling the Loader.
	//
	// If the Loader returns (nil, nil), an empty block is stored and Get returns a
	// non-nil, zero-length slice. Get panics if ctx is nil.
	Get(ctx context.Context, key string) ([]byte, error)
	// Len is the number of resident entries. Resident-only; it may lag a completion
	// by one critical section and is never above capacity.
	Len() int
	// Close cancels in-flight loads, waits for their Loader calls to return, wakes
	// every waiter with ErrClosed, drops all entries, and returns nil. Idempotent:
	// a Close that arrives while another is still joining waits for that join.
	Close() error
}

// ErrClosed is returned by Get once the cache has been closed.
var ErrClosed = errors.New("blockcache: closed")

// errLoaderExited resolves a flight whose Loader called runtime.Goexit. It is
// deliberately unexported (opaque): the public API is fixed by the spec, and
// callers should treat it as an ordinary load failure.
var errLoaderExited = errors.New("blockcache: loader exited without returning")

// cache is the Cache implementation.
//
// Invariants:
//   - I1 len(items) == lru.Len() <= capacity at the end of every critical section.
//   - I2 A key is never in both items and inflight.
//   - I3 entry.val and call.val are never written after publication, so copying
//     out of them without mu is race-free (happens-before via mu or close(done)).
//   - I4 closed.Store(true) and cancel() happen in one critical section; afterwards no
//     loads.Add runs and no entry is inserted. "Loader returned after the cache
//     ctx was cancelled" is therefore exactly "resolve observes closed under mu".
//   - I5 loads.Add(1) only runs under mu with closed == false, so every Add
//     happens-before Close's loads.Wait().
//   - I6 closedCh is closed only after loads.Wait() has returned, so observing it
//     closed implies every Loader call has returned.
type cache struct {
	capacity int
	load     Loader

	ctx    context.Context // cache-scoped; every Loader call gets this
	cancel context.CancelFunc

	loads    sync.WaitGroup // one count per running loader goroutine
	closedCh chan struct{}  // closed once, after the join; wakes waiters and later Closes

	// closed is set when Close begins. It is written only under mu (so I4 holds)
	// but is atomic so that wait can read it without re-taking mu.
	closed atomic.Bool

	mu       sync.Mutex // guards every field below
	lru      *list.List // front = MRU, back = LRU; Element.Value is *entry
	items    map[string]*list.Element
	inflight map[string]*call // unresolved loads only; not resident, not counted
}

type entry struct {
	key string
	val []byte // cache-owned; never written after creation
}

// call is one single-flight load.
type call struct {
	done chan struct{} // closed exactly once, when the load resolves
	val  []byte        // cache-owned copy; written before close(done)
	err  error         // written before close(done)
}

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
//
// In-flight loads are not bounded: each distinct cold key being loaded costs one
// goroutine (and one bookkeeping entry) until its Loader returns, so a burst of
// distinct cold keys produces a matching burst of concurrent Loader calls.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity must be >= 1")
	}
	if load == nil {
		panic("blockcache: nil Loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity: capacity,
		load:     load,
		ctx:      ctx,
		cancel:   cancel,
		closedCh: make(chan struct{}),
		lru:      list.New(),
		items:    make(map[string]*list.Element),
		inflight: make(map[string]*call),
	}
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	if ctx == nil {
		panic("blockcache: nil Context")
	}
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el, ok := c.items[key]; ok {
		c.lru.MoveToFront(el)
		v := el.Value.(*entry).val
		c.mu.Unlock()
		return clone(v), nil
	}
	cl := c.inflight[key]
	if cl == nil {
		cl = &call{done: make(chan struct{})}
		c.inflight[key] = cl
		c.loads.Add(1) // under mu with closed == false (I5)
		go c.run(key, cl)
	}
	c.mu.Unlock()
	return c.wait(ctx, cl)
}

// wait blocks until the cache closes, the load resolves, or ctx is Done, then
// settles the outcome by rule-7 priority rather than by select's random choice.
func (c *cache) wait(ctx context.Context, cl *call) ([]byte, error) {
	select {
	case <-c.closedCh:
	case <-cl.done:
	case <-ctx.Done():
	}

	// closed is stored before cancel(), before any resolve that observes it
	// closes done, and before closedCh is closed, so every wake caused by Close
	// sees it here.
	if c.closed.Load() {
		// (a) The cache is closing or closed. Normally ErrClosed is handed out
		// only after the join (I6). But if the caller's ctx is Done we return at
		// once: that keeps the caller's deadline independent of other Loaders,
		// and it breaks the cycle where a Loader Gets another in-flight key
		// with its (now cancelled) cache ctx and Close's join waits on it.
		select {
		case <-c.closedCh:
		case <-ctx.Done():
		}
		return nil, ErrClosed
	}

	select {
	case <-cl.done:
		// (c) Resolved and not closing; a resolved result beats a Done ctx.
		if cl.err != nil {
			return nil, cl.err
		}
		return clone(cl.val), nil
	default:
	}
	// (b) ctx is Done while the load is still in flight.
	return nil, ctx.Err()
}

// run performs the single Loader call of a flight and always resolves it exactly
// once, even if the Loader panics or calls runtime.Goexit.
func (c *cache) run(key string, cl *call) {
	defer c.loads.Done() // runs last, so the join also covers resolve
	var (
		b        []byte
		err      error
		returned bool
	)
	defer func() {
		if !returned {
			if r := recover(); r != nil {
				err = &panicError{value: r, stack: debug.Stack()}
			} else {
				err = errLoaderExited
			}
			b = nil
		}
		c.resolve(key, cl, b, err)
	}()
	b, err = c.load(c.ctx, key) // cache-scoped ctx, never a caller's
	returned = true
}

// resolve publishes a flight's outcome. Inflight removal, admission and
// close(done) share one critical section, so no Get can join a resolved call.
func (c *cache) resolve(key string, cl *call, b []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, key)
	switch {
	case c.closed.Load():
		// Returned after the cache ctx was cancelled: discard (I4). wait always
		// takes its closed branch first, so this value is never handed out; it is
		// set so the call never looks like a successful empty load.
		cl.err = ErrClosed
	case err != nil:
		cl.err = err // error wins; bytes discarded, nothing stored
	default:
		owned := clone(b) // never retain the Loader's slice
		cl.val = owned
		c.admit(key, owned)
	}
	close(cl.done)
}

// admit inserts key as MRU, evicting LRU entries first. Caller holds mu.
func (c *cache) admit(key string, val []byte) {
	if _, ok := c.items[key]; ok {
		panic("blockcache: invariant violated: admitting a resident key") // I2
	}
	for c.lru.Len() >= c.capacity {
		back := c.lru.Back()
		c.lru.Remove(back)
		delete(c.items, back.Value.(*entry).key)
	}
	c.items[key] = c.lru.PushFront(&entry{key: key, val: val})
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// Close follows the order cancel -> join -> wake/drop.
func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		<-c.closedCh // share the first Close's join
		return nil
	}
	c.closed.Store(true) // from here Gets return ErrClosed and no load starts (I4, I5)
	c.cancel()
	c.mu.Unlock()

	c.loads.Wait() // every in-flight Loader call has returned

	c.mu.Lock()
	c.lru.Init()
	clear(c.items)
	close(c.closedCh) // wake every waiter and any concurrent Close (I6)
	c.mu.Unlock()
	return nil
}

// clone returns a fresh, non-nil copy of b.
func clone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// panicError resolves a flight whose Loader panicked. It keeps the panic value
// and the Loader goroutine's stack so the bug stays diagnosable.
type panicError struct {
	value any
	stack []byte
}

func (e *panicError) Error() string {
	return fmt.Sprintf("blockcache: loader panicked: %v\n\n%s", e.value, e.stack)
}

// Unwrap exposes the panic value when it is an error.
func (e *panicError) Unwrap() error {
	if err, ok := e.value.(error); ok {
		return err
	}
	return nil
}
