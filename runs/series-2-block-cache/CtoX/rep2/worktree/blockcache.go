// Package blockcache provides a bounded, concurrent, single-flight block cache
// in front of a slow backing store.
package blockcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// ErrClosed is returned by Get once the cache has been closed.
var ErrClosed = errors.New("blockcache: closed")

// Loader fetches the block for key. The cache calls it at most once per
// in-flight miss for a key (single-flight). It is given a cache-scoped context,
// not any single caller's. A Loader MUST return promptly when its ctx is Done.
//
// A Loader may call Get (for other keys) and Len on the cache it serves. It
// must not call Close on that cache, directly or by blocking on a goroutine
// that does: Close waits for every in-flight Loader call to return, so such a
// Loader never returns promptly after its ctx is Done.
//
// A Loader must not panic. A panic is not recovered by the cache and
// terminates the program like any other unrecovered goroutine panic.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded LRU cache of byte blocks with single-flight loading.
type Cache interface {
	// Get returns a COPY of the block for key, loading on a miss. Concurrent
	// Gets for the same absent key cause exactly one Loader call; all receive
	// its result. On error the returned bytes are nil. A waiting Get returns,
	// in priority: ErrClosed if the cache closes; else ctx.Err() if ctx is
	// Done while the load is still in flight; else the load's result.
	Get(ctx context.Context, key string) ([]byte, error)
	// Len is the number of resident entries. Resident-only; it may lag a
	// completion by one critical section and is never above capacity.
	Len() int
	// Close cancels in-flight loads, waits for their Loader calls to return,
	// wakes every waiter with ErrClosed, drops all entries, and returns nil.
	// Idempotent; a concurrent second Close waits for the same join. Close
	// must not be called from inside a Loader of the same cache.
	Close() error
}

// New returns a Cache holding at most capacity entries. It panics if
// capacity < 1 or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: New called with capacity < 1")
	}
	if load == nil {
		panic("blockcache: New called with nil Loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		load:     load,
		capacity: capacity,
		ctx:      ctx,
		cancel:   cancel,
		closing:  make(chan struct{}),
		entries:  make(map[string]*list.Element),
		lru:      list.New(),
		inflight: make(map[string]*call),
	}
}

type cache struct {
	load     Loader
	capacity int

	ctx    context.Context // cache-scoped; every Loader call receives it
	cancel context.CancelFunc

	// loads counts running Loader goroutines. Add is called only under mu
	// while !closed, and Close calls Wait only after setting closed under mu.
	loads sync.WaitGroup

	mu       sync.Mutex // guards every field below
	closed   bool
	closing  chan struct{}            // closed under mu when Close first runs
	entries  map[string]*list.Element // resident entries; Value is *entry
	lru      *list.List               // front = MRU, back = LRU
	inflight map[string]*call         // unresolved loads; not resident
}

type entry struct {
	key string
	val []byte // cache-owned and never mutated
}

// call is one in-flight load shared by all of its waiters.
type call struct {
	done     chan struct{} // closed under mu after the fields below are set
	resolved bool
	val      []byte // cache-owned, never mutated
	err      error
}

func clone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el, ok := c.entries[key]; ok {
		c.lru.MoveToFront(el)
		v := el.Value.(*entry).val
		c.mu.Unlock()
		return clone(v), nil
	}
	cl, ok := c.inflight[key]
	if !ok {
		cl = &call{done: make(chan struct{})}
		c.inflight[key] = cl
		c.loads.Add(1)
		go c.run(key, cl)
	}
	c.mu.Unlock()
	return c.wait(ctx, cl)
}

// wait blocks until the load resolves, ctx is Done, or the cache closes, then
// decides the outcome from a single snapshot taken under mu.
func (c *cache) wait(ctx context.Context, cl *call) ([]byte, error) {
	select {
	case <-cl.done:
	case <-ctx.Done():
	case <-c.closing:
	}
	c.mu.Lock()
	closed, resolved, val, err := c.closed, cl.resolved, cl.val, cl.err
	c.mu.Unlock()
	switch {
	case closed:
		return nil, ErrClosed
	case !resolved:
		return nil, ctx.Err()
	case err != nil:
		return nil, err
	default:
		return clone(val), nil
	}
}

// run performs the single Loader call for key and publishes its result.
func (c *cache) run(key string, cl *call) {
	defer c.loads.Done()
	val, err := c.load(c.ctx, key)
	var owned []byte
	if err == nil {
		owned = clone(val)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, key)
	switch {
	case c.closed:
		// The cache-scoped context was cancelled; discard the result.
		cl.err = ErrClosed
	case err != nil:
		cl.err = err
	default:
		c.admit(key, owned)
		cl.val = owned
	}
	cl.resolved = true
	close(cl.done)
}

// admit stores val as the MRU entry for key, evicting the LRU entry if the
// cache is full. Must be called with mu held.
func (c *cache) admit(key string, val []byte) {
	if el, ok := c.entries[key]; ok {
		el.Value.(*entry).val = val
		c.lru.MoveToFront(el)
		return
	}
	if c.lru.Len() >= c.capacity {
		back := c.lru.Back()
		c.lru.Remove(back)
		delete(c.entries, back.Value.(*entry).key)
	}
	c.entries[key] = c.lru.PushFront(&entry{key: key, val: val})
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

func (c *cache) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.cancel()
		close(c.closing)
		clear(c.entries)
		c.lru.Init()
	}
	c.mu.Unlock()
	c.loads.Wait()
	return nil
}
