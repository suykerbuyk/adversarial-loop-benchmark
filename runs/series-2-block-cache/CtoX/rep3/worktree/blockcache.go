// Package blockcache provides a bounded, concurrent, single-flight block cache
// in front of a slow backing store.
//
// Caller contract: a Loader must return promptly once its ctx is Done, must not
// call Close on its own cache (Close would wait for it), and must not Get its
// own key (it would wait on its own in-flight load). It may Get other keys.
package blockcache

import (
	"container/list"
	"context"
	"errors"
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

// entry is a resident block. val is cache-owned and never mutated after creation.
type entry struct {
	key string
	val []byte
}

// call is one in-flight load shared by every waiter for its key.
type call struct {
	done    chan struct{} // closed exactly once (under mu) when the load resolves
	val     []byte        // written before close(done); read only after <-done
	err     error
	waiters map[*waiter]struct{} // waiters still blocked on this load; guarded by mu
}

// waiter is one Get blocked on a call. At resolution (under mu) run records
// whether the waiter's ctx was already Done, which by monotonicity of ctx
// cancellation means it was Done while the load was still in flight (rule 7b).
type waiter struct {
	ctx      context.Context
	canceled bool // written by run under mu before close(done)
}

type cache struct {
	capacity int
	load     Loader

	ctx    context.Context // cache-scoped; passed to every Loader call
	cancel context.CancelFunc

	mu       sync.Mutex // guards the fields below
	ll       *list.List // resident entries; front = MRU, back = LRU
	entries  map[string]*list.Element
	inflight map[string]*call
	closed   bool

	closedCh  chan struct{}  // closed when Close starts; wakes all waiters
	loaders   sync.WaitGroup // one count per running Loader goroutine
	closeDone chan struct{}  // closed when the first Close has finished joining
}

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity must be >= 1")
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
		entries:   make(map[string]*list.Element),
		inflight:  make(map[string]*call),
		closedCh:  make(chan struct{}),
		closeDone: make(chan struct{}),
	}
}

// Get implements Cache.
func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el, ok := c.entries[key]; ok {
		c.ll.MoveToFront(el)
		v := el.Value.(*entry).val
		c.mu.Unlock()
		return clone(v), nil // v is immutable, so copying outside mu is safe
	}
	cl, ok := c.inflight[key]
	if !ok {
		cl = &call{done: make(chan struct{}), waiters: make(map[*waiter]struct{})}
		c.inflight[key] = cl
		c.loaders.Add(1) // under mu with closed == false: never races Close's Wait
		go c.run(key, cl)
	}
	w := &waiter{ctx: ctx}
	cl.waiters[w] = struct{}{}
	c.mu.Unlock()
	return c.wait(cl, w)
}

// wait blocks until the cache closes, w's ctx is Done, or cl resolves, then
// answers by rule 7 priority: (a) ErrClosed; (b) ctx.Err() if the ctx was Done
// while the load was still in flight; (c) the load's result.
func (c *cache) wait(cl *call, w *waiter) ([]byte, error) {
	select {
	case <-c.closedCh:
	case <-w.ctx.Done():
	case <-cl.done:
	}
	if isDone(c.closedCh) {
		return nil, ErrClosed
	}

	c.mu.Lock()
	resolved := isDone(cl.done)
	if !resolved {
		// Only reachable via ctx.Done: the ctx is Done and the load is still in
		// flight. Deregister so a long load does not accumulate departed waiters.
		delete(cl.waiters, w)
	}
	canceled := w.canceled
	c.mu.Unlock()

	switch {
	case !resolved || canceled:
		return nil, w.ctx.Err()
	case cl.err != nil:
		return nil, cl.err
	default:
		return clone(cl.val), nil
	}
}

// run performs the single Loader call for key and publishes its result to cl.
func (c *cache) run(key string, cl *call) {
	defer c.loaders.Done()
	v, err := c.load(c.ctx, key)
	var own []byte
	if err == nil && c.ctx.Err() == nil {
		// Never retain the Loader's slice; on error the bytes are dropped. Skip
		// the copy once the cache is cancelled: the result will be discarded.
		own = clone(v)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[key] == cl {
		delete(c.inflight, key)
	}
	switch {
	case c.closed || c.ctx.Err() != nil:
		cl.err = ErrClosed // result discarded after cancellation
	case err != nil:
		cl.err = err
	default:
		c.storeLocked(key, own)
		cl.val = own
	}
	// Linearise each waiter's cancellation against resolution: a ctx already
	// Done here was Done while the load was in flight (rule 7b).
	for w := range cl.waiters {
		w.canceled = w.ctx.Err() != nil
	}
	cl.waiters = nil
	close(cl.done)
}

// storeLocked makes key resident as the MRU entry, evicting the LRU entry first
// if the cache is full. c.mu must be held.
func (c *cache) storeLocked(key string, val []byte) {
	if el, ok := c.entries[key]; ok { // defensive: a key is never both resident and in flight
		el.Value.(*entry).val = val
		c.ll.MoveToFront(el)
		return
	}
	if c.ll.Len() >= c.capacity {
		back := c.ll.Back()
		c.ll.Remove(back)
		delete(c.entries, back.Value.(*entry).key)
	}
	c.entries[key] = c.ll.PushFront(&entry{key: key, val: val})
}

// Len implements Cache.
func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Close implements Cache.
func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone // a concurrent second Close joins the first
		return nil
	}
	c.closed = true
	close(c.closedCh)
	c.cancel()
	c.entries = make(map[string]*list.Element)
	c.ll.Init()
	c.inflight = make(map[string]*call)
	c.mu.Unlock()

	c.loaders.Wait()
	close(c.closeDone)
	return nil
}

// clone returns a fresh, non-nil copy of b.
func clone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// isDone reports whether ch has been closed, without blocking.
func isDone(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
