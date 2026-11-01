// Package blockcache provides a bounded, concurrent, single-flight block cache
// in front of a slow backing store.
//
// Concurrent Gets for the same absent key share one Loader call. Loads run under
// a cache-scoped context rather than any caller's, so a load continues (and
// populates the cache) even if every waiter gives up. Resident entries are
// bounded by capacity and evicted in least-recently-used order.
//
// Re-entrancy is unsupported: a Loader must not call Close on its own cache
// (Close would wait for that very Loader), and a Loader that calls Get for its
// own key waits on itself until the caller's ctx or Close ends the wait.
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
		closedCh:  make(chan struct{}),
		closeDone: make(chan struct{}),
	}
}

type cache struct {
	capacity int
	load     Loader

	ctx    context.Context // cache-scoped; every Loader call gets this
	cancel context.CancelFunc

	mu       sync.Mutex               // guards every field below
	ll       *list.List               // resident entries; Front = MRU; Value is *entry
	items    map[string]*list.Element // resident index into ll
	inflight map[string]*call         // unresolved loads; not counted, not evictable
	closed   bool                     // set once, together with cancel() and close(closedCh)

	closedCh  chan struct{}  // closed when Close begins; wakes waiters with ErrClosed
	closeDone chan struct{}  // closed once the first Close has joined all Loader calls
	loads     sync.WaitGroup // one count per running Loader goroutine; Add only under mu while open
}

type entry struct {
	key string
	val []byte // cache-owned; never mutated after creation
}

// call is one in-flight load. val and err are written exactly once, before done
// is closed; waiters read them only after observing done closed.
type call struct {
	done chan struct{}
	val  []byte
	err  error
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		out := clone(el.Value.(*entry).val)
		c.mu.Unlock()
		return out, nil
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

// wait blocks until cl resolves, ctx is Done, or the cache closes, and applies
// the priority ErrClosed > ctx.Err() (while in flight) > load result. Because
// select picks randomly among ready cases, the re-checks enforce the order.
func (c *cache) wait(ctx context.Context, cl *call) ([]byte, error) {
	select {
	case <-c.closedCh:
		return nil, ErrClosed
	case <-cl.done:
	case <-ctx.Done():
		if isClosed(c.closedCh) {
			return nil, ErrClosed
		}
		if !isClosed(cl.done) {
			return nil, ctx.Err()
		}
		// The load already resolved; fall through to its result.
	}
	if isClosed(c.closedCh) {
		return nil, ErrClosed
	}
	if cl.err != nil {
		return nil, cl.err
	}
	return clone(cl.val), nil
}

// run executes the Loader for key and publishes the result to cl.
func (c *cache) run(key string, cl *call) {
	defer c.loads.Done()
	val, err := c.safeLoad(key)

	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, key)
	switch {
	case c.closed:
		// Returned after the cache-scoped ctx was cancelled: discard.
		cl.err = ErrClosed
	case err != nil:
		// Error wins over any bytes; nothing is stored.
		cl.err = err
	default:
		own := clone(val)
		cl.val = own
		c.admit(key, own)
	}
	close(cl.done)
}

// safeLoad calls the Loader, converting a panic into an error so that the
// in-flight bookkeeping is always completed and no waiter is stranded.
func (c *cache) safeLoad(key string) (val []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			val, err = nil, fmt.Errorf("blockcache: loader panic for key %q: %v", key, r)
		}
	}()
	return c.load(c.ctx, key)
}

// admit stores val as the MRU entry for key, evicting the LRU entry if full.
// c.mu must be held.
func (c *cache) admit(key string, val []byte) {
	if el, ok := c.items[key]; ok {
		el.Value.(*entry).val = val
		c.ll.MoveToFront(el)
		return
	}
	if c.ll.Len() >= c.capacity {
		if back := c.ll.Back(); back != nil {
			c.ll.Remove(back)
			delete(c.items, back.Value.(*entry).key)
		}
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
		<-c.closeDone
		return nil
	}
	c.closed = true
	c.cancel()
	close(c.closedCh)
	c.items = make(map[string]*list.Element)
	c.ll.Init()
	c.mu.Unlock()

	c.loads.Wait()
	close(c.closeDone)
	return nil
}

func clone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// isClosed reports whether ch has been closed, without blocking.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
