// Package blockcache provides a bounded, concurrent, single-flight block cache
// in front of a slow backing store.
//
// Caller preconditions:
//   - The ctx passed to Get must be non-nil.
//   - A Loader must not call Close on its own cache (Close waits for that
//     Loader to return, so it would deadlock).
//   - A Loader must not Get its own key (it would wait on its own result).
//   - A Cache must not be shared across testing/synctest bubble boundaries.
package blockcache

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
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

// errLoaderExited is reported to waiters when the Loader goroutine exits
// without returning (runtime.Goexit).
var errLoaderExited = errors.New("blockcache: loader exited without returning")

type cache struct {
	capacity int
	load     Loader

	// Cache-scoped context handed to every Loader call. Only Close cancels it,
	// in the same critical section that sets closed, so "closed" and "ctx
	// cancelled" are indistinguishable to any code holding mu.
	ctx    context.Context
	cancel context.CancelFunc

	closing chan struct{} // closed when closed is set; wakes waiters
	drained chan struct{} // closed exactly once, when closed && active == 0

	mu       sync.Mutex // guards everything below
	closed   bool
	active   int                      // load goroutines started and not yet completed; == len(inflight)
	lru      *list.List               // resident entries; Front = MRU; Value = *entry
	items    map[string]*list.Element // resident key -> element of lru
	inflight map[string]*call         // in-flight loads; not resident, not evictable
}

type entry struct {
	key string
	val []byte // cache-owned; never mutated after insertion
}

// call is one in-flight load. val and err are written only before done is
// closed and read only after.
type call struct {
	done chan struct{}
	val  []byte
	err  error

	// waiters are the Gets currently waiting on this load. Guarded by
	// cache.mu; set to nil once the load completes.
	waiters map[*waiter]struct{}
}

// waiter is one Get blocked on a call.
type waiter struct {
	ctx context.Context
	// cancelled records whether ctx was already Done when the load resolved,
	// i.e. ctx became Done while the load was still in flight (rule 7(b)).
	// Written under cache.mu before call.done is closed; read after.
	cancelled bool
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
		capacity: capacity,
		load:     load,
		ctx:      ctx,
		cancel:   cancel,
		closing:  make(chan struct{}),
		drained:  make(chan struct{}),
		lru:      list.New(),
		items:    make(map[string]*list.Element),
		inflight: make(map[string]*call),
	}
}

// Get implements Cache.
func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	// The closed check, lookup, and load start form one critical section so
	// that Close either precedes this Get entirely or joins the load it starts.
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el, ok := c.items[key]; ok {
		c.lru.MoveToFront(el)
		out := bytes.Clone(el.Value.(*entry).val)
		c.mu.Unlock()
		return out, nil
	}
	cl, ok := c.inflight[key]
	if !ok {
		cl = &call{done: make(chan struct{}), waiters: make(map[*waiter]struct{})}
		c.inflight[key] = cl
		c.active++
		go c.runLoad(key, cl)
	}
	w := &waiter{ctx: ctx}
	cl.waiters[w] = struct{}{}
	c.mu.Unlock()
	return c.wait(w, cl)
}

// wait blocks until the cache closes, the load resolves, or w.ctx is Done, and
// then answers with rule 7's priority: (a) ErrClosed, (b) ctx.Err() if ctx
// became Done while the load was in flight, (c) the load's result.
//
// Whether ctx became Done before the load resolved is decided by complete,
// which samples ctx.Err() at the moment of resolution, so a waiter that wakes
// to find both channels ready still answers by the order in which they became
// ready rather than by scheduling luck.
func (c *cache) wait(w *waiter, cl *call) ([]byte, error) {
	select {
	case <-c.closing:
	case <-cl.done:
	case <-w.ctx.Done():
	}
	select {
	case <-c.closing:
		return nil, ErrClosed
	default:
	}
	select {
	case <-cl.done:
		// cancelled is never set on a result discarded by Close, so (a) the
		// ErrClosed stamped by complete still beats (b).
		switch {
		case w.cancelled:
			return nil, w.ctx.Err()
		case cl.err != nil:
			return nil, cl.err
		}
		return bytes.Clone(cl.val), nil
	default:
	}
	// ctx is Done and the load is still in flight. Deregister so the call does
	// not retain this waiter. If the load resolves in the meantime, complete
	// sees ctx Done and records cancelled, which agrees with this answer.
	c.mu.Lock()
	delete(cl.waiters, w)
	c.mu.Unlock()
	return nil, w.ctx.Err()
}

// runLoad performs the single Loader call for cl in its own goroutine. The
// deferred completion runs on return, recovered panic, or runtime.Goexit.
func (c *cache) runLoad(key string, cl *call) {
	val, err := []byte(nil), errLoaderExited
	defer func() { c.complete(key, cl, val, err) }()
	val, err = c.callLoader(key)
}

// callLoader invokes the Loader under the cache-scoped context, converting a
// panic into an error. The recover covers only the Loader frame: this function
// contains no cache logic, so any panic recovered here originated in the
// Loader. Cache code (complete, admit) runs outside it and still crashes.
func (c *cache) callLoader(key string) (val []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			val, err = nil, fmt.Errorf("blockcache: loader panicked for key %q: %v\n%s", key, r, debug.Stack())
		}
	}()
	return c.load(c.ctx, key)
}

// complete publishes a load's result and updates the cache.
func (c *cache) complete(key string, cl *call, val []byte, err error) {
	var own []byte
	if err == nil {
		own = bytes.Clone(val) // do not retain the Loader's slice
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, key)
	c.active--
	switch {
	case c.closed:
		cl.err = ErrClosed // result after Close is discarded, not stored
	case err != nil:
		cl.err = err // error wins; not stored, so a later Get reloads
	default:
		cl.val = own
		c.admit(key, own)
	}
	if !c.closed {
		// Rule 7(b): a waiter whose ctx is already Done at resolution saw its
		// ctx end while the load was still in flight.
		for w := range cl.waiters {
			w.cancelled = w.ctx.Err() != nil
		}
	}
	cl.waiters = nil
	close(cl.done)
	if c.closed && c.active == 0 {
		close(c.drained)
	}
}

// admit inserts key as MRU, evicting LRU entries to stay within capacity.
// The caller must hold c.mu.
func (c *cache) admit(key string, val []byte) {
	if _, ok := c.items[key]; ok {
		panic("blockcache: internal error: admit of resident key")
	}
	for c.lru.Len() >= c.capacity {
		back := c.lru.Back()
		c.lru.Remove(back)
		delete(c.items, back.Value.(*entry).key)
	}
	c.items[key] = c.lru.PushFront(&entry{key: key, val: val})
}

// Len implements Cache.
func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// Close implements Cache.
func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.drained // a second Close waits for the same join
		return nil
	}
	c.closed = true
	// Cancel under mu so that every completion observing closed also observes
	// a cancelled cache context. cancel only closes the context's Done channel.
	c.cancel()
	close(c.closing)
	c.items = make(map[string]*list.Element)
	c.lru.Init()
	if c.active == 0 {
		close(c.drained)
	}
	c.mu.Unlock()

	<-c.drained
	return nil
}
