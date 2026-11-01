// Package blockcache is a bounded, concurrent, single-flight block cache in
// front of a slow backing store.
//
// This is the chair's reference (correct) implementation of the instrument
// specification. It is not shipped to the cell.
package blockcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// ErrClosed is returned by Get after the cache has been closed, and to every
// waiter woken by Close.
var ErrClosed = errors.New("blockcache: closed")

// Loader fetches the block for key. The cache calls it at most once per
// in-flight miss for a key (single-flight). It is given a cache-scoped context,
// not any single caller's. A Loader MUST return promptly when its ctx is Done.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent, single-flight block cache.
type Cache interface {
	// Get returns a COPY of the block for key, loading on a miss. Concurrent
	// Gets for the same absent key cause exactly one Loader call; all receive
	// its result.
	Get(ctx context.Context, key string) ([]byte, error)
	// Len is the number of resident entries. Resident-only; it may lag a
	// completion by one critical section and is never above capacity.
	Len() int
	// Close cancels in-flight loads, waits for their Loader calls to return,
	// wakes every waiter with ErrClosed, drops all entries, and returns nil.
	// Idempotent.
	Close() error
}

// New returns a Cache holding at most capacity entries. It panics if
// capacity < 1 or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: New requires capacity >= 1")
	}
	if load == nil {
		panic("blockcache: New requires a non-nil Loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity: capacity,
		load:     load,
		ctx:      ctx,
		cancel:   cancel,
		entries:  make(map[string]*list.Element),
		order:    list.New(),
		inflight: make(map[string]*call),
	}
}

// entry is a resident cache entry. data is owned by the cache and never aliased
// with a Loader's returned slice or with anything a caller receives.
type entry struct {
	key  string
	data []byte
}

// call tracks one in-flight single-flight load for a key.
type call struct {
	done chan struct{} // closed when val/err are final
	val  []byte        // cache-owned copy of a successful result, else nil
	err  error         // non-nil on load error
}

type cache struct {
	capacity int
	load     Loader

	// ctx is the cache-scoped context handed to every Loader call. Close
	// cancels it. cancel is retained so Close can trigger cancellation.
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	entries  map[string]*list.Element // key -> element in order; Value is *entry
	order    *list.List               // front = most-recently-used, back = LRU
	inflight map[string]*call         // key -> in-flight load
	closed   bool

	// wg tracks in-flight Loader goroutines so Close can join them.
	wg sync.WaitGroup
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	// Hit: return a fresh copy and mark the key most-recently-used.
	if el, ok := c.entries[key]; ok {
		c.order.MoveToFront(el)
		cp := cloneBytes(el.Value.(*entry).data)
		c.mu.Unlock()
		return cp, nil
	}
	// Miss: join an in-flight load, or start one. The Loader runs under the
	// cache-scoped context in its own goroutine so the load continues even if
	// every waiter leaves (orphaned loads are not leaks).
	cl, ok := c.inflight[key]
	if !ok {
		cl = &call{done: make(chan struct{})}
		c.inflight[key] = cl
		c.wg.Add(1)
		go c.runLoad(key, cl)
	}
	c.mu.Unlock()

	// Wait, honoring priority: (a) ErrClosed, (b) ctx.Err() while in flight,
	// (c) the resolved result.
	select {
	case <-c.ctx.Done():
		return nil, ErrClosed
	case <-cl.done:
		// Resolved. ErrClosed still takes priority if the cache has closed.
		if c.ctx.Err() != nil {
			return nil, ErrClosed
		}
		return result(cl)
	case <-ctx.Done():
		// The caller's own context fired. ErrClosed takes priority; and if the
		// load has in fact already resolved it is no longer "in flight", so the
		// result wins over ctx.Err().
		if c.ctx.Err() != nil {
			return nil, ErrClosed
		}
		select {
		case <-cl.done:
			return result(cl)
		default:
			return nil, ctx.Err()
		}
	}
}

// result returns the caller-facing value of a resolved call: the load's error,
// or a fresh copy of the successful bytes.
func result(cl *call) ([]byte, error) {
	if cl.err != nil {
		return nil, cl.err
	}
	return cloneBytes(cl.val), nil
}

// runLoad executes one Loader call and resolves cl. On success, unless the
// cache-scoped context was cancelled first, the bytes become resident (a
// cache-owned copy). A result produced after cancellation is discarded.
func (c *cache) runLoad(key string, cl *call) {
	defer c.wg.Done()

	data, err := c.load(c.ctx, key)

	c.mu.Lock()
	defer c.mu.Unlock()

	// The in-flight slot is consumed whatever the outcome; a later Get re-loads.
	delete(c.inflight, key)

	switch {
	case err != nil:
		// Error (including (bytes, err)): nothing is stored; the error wins.
		cl.err = err
	case c.ctx.Err() != nil || c.closed:
		// Post-cancel / close-window return: discard, do not store. Waiters will
		// observe ErrClosed via the cache-scoped context.
	default:
		stored := cloneBytes(data)
		c.store(key, stored)
		cl.val = stored
	}
	close(cl.done)
}

// store inserts key as a resident entry, evicting the least-recently-used entry
// first if the cache is at capacity. The caller must hold c.mu and must own
// data (it is retained, not copied again).
func (c *cache) store(key string, data []byte) {
	if el, ok := c.entries[key]; ok {
		el.Value.(*entry).data = data
		c.order.MoveToFront(el)
		return
	}
	for c.order.Len() >= c.capacity {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.order.Remove(back)
		delete(c.entries, back.Value.(*entry).key)
	}
	c.entries[key] = c.order.PushFront(&entry{key: key, data: data})
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		// Idempotent: a second Close waits for the same in-flight join, then
		// returns nil. Entry teardown is performed by the first Close.
		c.mu.Unlock()
		c.wg.Wait()
		return nil
	}
	c.closed = true
	c.cancel() // wake waiters (via c.ctx.Done) and signal Loaders to return
	c.mu.Unlock()

	// Wait for every in-flight Loader call to return before dropping state.
	c.wg.Wait()

	c.mu.Lock()
	c.entries = make(map[string]*list.Element)
	c.order.Init()
	c.mu.Unlock()
	return nil
}

// cloneBytes returns an independent copy of b (nil-safe).
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}
