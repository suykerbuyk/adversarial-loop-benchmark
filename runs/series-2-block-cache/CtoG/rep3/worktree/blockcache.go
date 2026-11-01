// Package blockcache provides a bounded, concurrent, single-flight block cache
// in front of a slow backing store.
//
// Get returns a fresh copy of the block that the caller owns and may mutate.
// A successful load of a nil or empty block is returned as a non-nil,
// zero-length slice. On any error Get returns a nil slice, so callers must use
// the error, not a nil check on the bytes, to detect failure.
//
// Loaders run under a cache-scoped context and must return promptly once it is
// Done; Close waits for every in-flight Loader call to return. Calling Close
// from inside a Loader, or a Loader that Gets its own key while ignoring its
// context, is out of contract and may block forever. A panicking Loader is also
// out of contract; the panic is recovered and reported to that load's waiters
// as an error whose message is not a stable API. A Loader that calls
// runtime.Goexit is likewise reported as an error. A nil caller context is not
// supported.
package blockcache

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
)

// Loader fetches the block for key. The cache calls it at most once per in-flight
// miss for a key (single-flight). It is given a cache-scoped context, not any
// single caller's. A Loader MUST return promptly when its ctx is Done.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent, single-flight block cache.
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

// ErrClosed is returned by Get once the cache has been closed.
var ErrClosed = errors.New("blockcache: closed")

// entry is a resident block. It is never mutated after being pushed onto the
// LRU list, and val is never written after it is stored.
type entry struct {
	key string
	val []byte
}

// call is one in-flight load. done is closed exactly once, when the call
// resolves; val and err are read-only after that.
type call struct {
	done chan struct{}
	val  []byte
	err  error
}

type cache struct {
	capacity int
	load     Loader

	ctx    context.Context // cache-scoped; every Loader runs under it
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	ll       *list.List               // *entry; Front = MRU, Back = LRU
	items    map[string]*list.Element // resident entries only
	inflight map[string]*call         // unresolved loads only

	closing   chan struct{}  // closed by the first Close; wakes all waiters
	loads     sync.WaitGroup // one count per running Loader goroutine
	closeDone chan struct{}  // closed once the first Close has finished
}

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic(fmt.Sprintf("blockcache: capacity must be >= 1, got %d", capacity))
	}
	if load == nil {
		panic("blockcache: nil Loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity:  capacity,
		load:      load,
		ctx:       ctx,
		cancel:    cancel,
		ll:        list.New(),
		items:     make(map[string]*list.Element),
		inflight:  make(map[string]*call),
		closing:   make(chan struct{}),
		closeDone: make(chan struct{}),
	}
}

// clone returns a fresh, non-nil copy of b.
func clone(b []byte) []byte {
	return append(make([]byte, 0, len(b)), b...)
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	v, hit, cl, err := c.lookup(ctx, key)
	switch {
	case err != nil:
		return nil, err
	case hit:
		return clone(v), nil // stored bytes are immutable, so copying outside mu is safe
	default:
		return c.wait(ctx, cl)
	}
}

// lookup is Get's single critical section: the closed check, the hit lookup,
// and on a miss joining or starting the load. The deferred unlock keeps mu
// from staying locked if anything in here panics (for example ctx.Err on an
// unsupported nil context).
func (c *cache) lookup(ctx context.Context, key string) (val []byte, hit bool, cl *call, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false, nil, ErrClosed
	}
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*entry).val, true, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, nil, err // caller already gone: do not start or join a load
	}
	cl, ok := c.inflight[key]
	if !ok {
		cl = &call{done: make(chan struct{})}
		c.inflight[key] = cl
		c.loads.Add(1) // only under mu while open, so it never races loads.Wait
		go c.run(key, cl)
	}
	return nil, false, cl, nil
}

// wait blocks until the cache closes, ctx is Done, or cl resolves, then answers
// in the spec's priority order: ErrClosed if the cache has closed, else cl's
// result if it has resolved, else ctx.Err() while cl is still in flight.
//
// The decision is made from one snapshot of closed and cl's resolution taken
// under mu, the same lock under which Close sets closed and resolve closes
// cl.done. So the decision is atomic with respect to both: a Close that has
// started by the snapshot always yields ErrClosed (even if cl resolved
// successfully just before it), and a Close that starts after the snapshot
// began after this Get's linearization point. ctx.Err() is read before taking
// mu because it is caller code; a Done context stays Done, so a non-nil error
// read earlier still holds at the snapshot.
func (c *cache) wait(ctx context.Context, cl *call) ([]byte, error) {
	for {
		select {
		case <-c.closing:
		case <-ctx.Done():
		case <-cl.done:
		}
		ctxErr := ctx.Err()
		c.mu.Lock()
		closed, resolved := c.closed, isDone(cl.done)
		c.mu.Unlock()
		switch {
		case closed:
			return nil, ErrClosed
		case resolved:
			if cl.err != nil {
				return nil, cl.err
			}
			return clone(cl.val), nil // stored bytes are immutable; copy outside mu
		case ctxErr != nil:
			return nil, ctxErr
		}
		// Nothing decisive (only possible for a context whose Done fired before
		// Err was set); wait again rather than return (nil, nil).
	}
}

// isDone reports, without blocking, whether ch is closed.
func isDone(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// run performs the single Loader invocation for cl and publishes its result.
// The call is resolved from a deferred function, so it is resolved on every
// exit path: a normal return, a panic, or runtime.Goexit inside the Loader.
func (c *cache) run(key string, cl *call) {
	defer c.loads.Done() // registered first, so it runs after the call is resolved

	var (
		b        []byte
		err      error
		returned bool
	)
	defer func() {
		if r := recover(); r != nil {
			b, err = nil, &loaderPanicError{value: r}
		} else if !returned {
			b, err = nil, errLoaderExited
		}
		c.resolve(key, cl, b, err)
	}()
	b, err = c.load(c.ctx, key) // cache-scoped ctx, never a caller's
	returned = true
}

// resolve publishes the outcome of cl's Loader call and removes it from
// inflight.
func (c *cache) resolve(key string, cl *call, b []byte, err error) {
	var owned []byte
	if err == nil {
		owned = clone(b) // never retain the Loader's slice
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, key)
	switch {
	case c.closed:
		cl.err = ErrClosed // resolved after Close: discard, do not store
	case err != nil:
		cl.err = err // not stored; a later Get re-loads
	default:
		c.admit(key, owned)
		cl.val = owned
	}
	close(cl.done)
}

// admit stores val as the MRU entry for key, evicting the LRU entry first if
// the cache is full. c.mu must be held and key must not be resident.
func (c *cache) admit(key string, val []byte) {
	if c.ll.Len() >= c.capacity {
		back := c.ll.Back()
		c.ll.Remove(back)
		delete(c.items, back.Value.(*entry).key)
	}
	c.items[key] = c.ll.PushFront(&entry{key: key, val: val})
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone // wait for the first Close's join
		return nil
	}
	c.closed = true
	c.cancel()
	close(c.closing)
	c.ll.Init()
	clear(c.items)
	c.mu.Unlock()

	c.loads.Wait() // runs need mu to resolve, so it must not be held here

	c.mu.Lock()
	c.ll.Init()
	clear(c.items)
	clear(c.inflight)
	c.mu.Unlock()
	close(c.closeDone)
	return nil
}

// errLoaderExited is reported to waiters when the Loader goroutine exits via
// runtime.Goexit instead of returning (out of contract).
var errLoaderExited = errors.New("blockcache: loader exited without returning")

// loaderPanicError reports a recovered Loader panic. Its message names only
// the panic value's type so formatting never calls into user code.
type loaderPanicError struct{ value any }

func (e *loaderPanicError) Error() string {
	return fmt.Sprintf("blockcache: loader panicked (panic value of type %T)", e.value)
}
