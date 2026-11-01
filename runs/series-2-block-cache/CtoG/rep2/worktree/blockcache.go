// Package blockcache provides a bounded, concurrent, single-flight block cache
// in front of a slow backing store.
package blockcache

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
)

// Loader fetches the block for key. The cache calls it at most once per
// in-flight miss for a key (single-flight). It is given a cache-scoped context,
// not any single caller's. A Loader MUST return promptly when its ctx is Done;
// otherwise Close blocks until it returns.
//
// The cache takes ownership of the returned slice only long enough to copy it;
// it never retains the Loader's slice. A panic in the Loader is recovered and
// reported to that load's waiters as an error; nothing is stored.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent, single-flight LRU block cache.
type Cache interface {
	// Get returns a COPY of the block for key, loading on a miss. Concurrent Gets
	// for the same absent key cause exactly one Loader call; all receive its result.
	//
	// A hit returns the bytes even if ctx is already Done; ctx only bounds
	// waiting on an in-flight load. A miss always starts, or joins, a load that
	// runs under the cache-scoped context; that load completes and populates the
	// cache even if this caller stops waiting.
	//
	// The wait on a miss ends only when the load resolves, the cache closes, or
	// ctx is Done. With a ctx that is never Done (such as context.Background) and
	// a Loader that never returns, Get blocks indefinitely; pass a ctx with a
	// deadline to bound it.
	Get(ctx context.Context, key string) ([]byte, error)
	// Len is the number of resident entries. Resident-only; it may lag a completion
	// by one critical section and is never above capacity.
	Len() int
	// Close cancels in-flight loads, waits for their Loader calls to return, wakes
	// every waiter with ErrClosed, drops all entries, and returns nil. Idempotent:
	// a concurrent second Close waits for the first one's join and returns nil.
	//
	// Close blocks until every in-flight Loader call has returned, so a Loader
	// that ignores cancellation blocks Close (and any concurrent Close). Callers
	// must call Close when done with the cache: it releases the cache-scoped
	// context and joins the load goroutines.
	Close() error
}

// ErrClosed is returned by Get once the cache has been closed, and to every
// waiter whose load was still in flight when Close started.
var ErrClosed = errors.New("blockcache: closed")

// New returns a Cache holding at most capacity entries. It panics if
// capacity < 1 or load == nil.
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
		lru:       list.New(),
		items:     make(map[string]*list.Element),
		inflight:  make(map[string]*call),
		closing:   make(chan struct{}),
		closeDone: make(chan struct{}),
	}
}

type cache struct {
	capacity int
	load     Loader

	ctx    context.Context    // cache-scoped; every Loader call receives it
	cancel context.CancelFunc // called only by the first Close, under mu, with closed = true

	mu     sync.Mutex               // guards everything below
	closed bool                     // set once, by the first Close, under mu
	lru    *list.List               // resident entries; Front = MRU, Back = LRU; Value is *entry
	items  map[string]*list.Element // resident key -> element in lru
	// inflight holds keys with a load in progress. They are not resident, do not
	// count toward capacity and cannot be evicted (spec rule 5), and orphaned
	// loads keep running (rule 7). Its size is therefore bounded by the number of
	// distinct missing keys, not by capacity. This is by design: do not cap it.
	inflight map[string]*call

	loads     sync.WaitGroup // one count per running load goroutine
	closing   chan struct{}  // closed together with closed = true; wakes blocked waiters
	closeDone chan struct{}  // closed by the first Close after join + drop

	// afterWake, if non-nil, runs in wait after the waiter wakes on closing or
	// ctx.Done and before it takes mu. Tests use it to pin that window; it is
	// always nil in production.
	afterWake func()
}

type entry struct {
	key string
	// val is cache-owned and never written after publication. It may share its
	// backing array with call.val; readers only copy it.
	val []byte
}

type call struct {
	done chan struct{} // closed exactly once, under mu, after val/err are set
	// val is the cache-owned copy on success; read-only after done is closed.
	// It shares its backing array with the resident entry.val.
	val []byte
	err error // Loader error, or ErrClosed if the result was discarded by Close
}

// result returns the outcome of a resolved call, giving each caller its own copy.
func (cl *call) result() ([]byte, error) {
	if cl.err != nil {
		return nil, cl.err
	}
	return bytes.Clone(cl.val), nil
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el, ok := c.items[key]; ok {
		// Hit: the caller's ctx is deliberately not consulted.
		c.lru.MoveToFront(el)
		v := el.Value.(*entry).val
		c.mu.Unlock()
		return bytes.Clone(v), nil // safe outside mu: stored slices are immutable
	}
	cl, ok := c.inflight[key]
	if !ok {
		cl = &call{done: make(chan struct{})}
		c.inflight[key] = cl
		c.loads.Add(1) // under mu after seeing !closed, so it cannot race Close's Wait
		go c.runLoad(key, cl)
	}
	c.mu.Unlock()
	return c.wait(ctx, cl)
}

// wait blocks until cl resolves, the cache closes, or ctx is Done, and returns
// per spec rule 7: resolved result (c) > ErrClosed (a) > ctx.Err() (b).
//
// Do NOT collapse this into a single select that returns on whichever case
// fires: Go picks randomly among ready cases, so (b) could beat (a) and either
// could beat an already-resolved (c). Waking on ctx.Done or closing never
// returns directly; the final decision is taken under mu, where runLoad closes
// cl.done and Close sets closed, so it is linearizable. In particular, if Close
// runs between the wake and the lock, the waiter observes closed (or a call
// that Close resolved with ErrClosed) and returns ErrClosed, not ctx.Err().
// ctx.Err() is returned only when, at the moment of the decision, the cache is
// open and the load is still in flight; such a waiter has left before Close and
// is not one of the "current waiters" Close must wake.
func (c *cache) wait(ctx context.Context, cl *call) ([]byte, error) {
	select {
	case <-cl.done:
		return cl.result()
	case <-c.closing:
	case <-ctx.Done():
	}
	if c.afterWake != nil {
		c.afterWake()
	}

	c.mu.Lock()
	resolved := false
	select {
	case <-cl.done:
		resolved = true
	default:
	}
	closed := c.closed
	c.mu.Unlock()

	switch {
	case resolved:
		return cl.result()
	case closed:
		return nil, ErrClosed
	default:
		return nil, ctx.Err()
	}
}

// runLoad performs the single Loader call for key and publishes its outcome.
func (c *cache) runLoad(key string, cl *call) {
	defer c.loads.Done()
	b, err := c.callLoader(key)
	var own []byte
	if err == nil {
		own = bytes.Clone(b) // never retain the Loader's slice; bytes with an error are dropped
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	defer close(cl.done) // runs before Unlock on every path
	delete(c.inflight, key)
	switch {
	case c.closed:
		// Close has started (and cancelled c.ctx in the same critical section):
		// discard the result so nothing survives Close.
		cl.err = ErrClosed
	case err != nil:
		cl.err = err
	default:
		// entry.val and cl.val alias own; neither is ever written afterwards.
		// Anyone wanting to mutate either must first give each its own copy.
		c.admit(key, own)
		cl.val = own
	}
}

// callLoader invokes the Loader under the cache context, converting a panic
// into an error. The key is intentionally not included in the message.
func (c *cache) callLoader(key string) (b []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			b = nil
			if e, ok := r.(error); ok {
				err = fmt.Errorf("blockcache: loader panicked: %w", e)
			} else {
				err = fmt.Errorf("blockcache: loader panicked: %v", r)
			}
		}
	}()
	return c.load(c.ctx, key)
}

// admit makes key resident as MRU, evicting LRU entries to stay within
// capacity. Caller must hold mu. key is never already resident (single-flight).
func (c *cache) admit(key string, val []byte) {
	for c.lru.Len() >= c.capacity {
		back := c.lru.Back()
		delete(c.items, back.Value.(*entry).key)
		c.lru.Remove(back)
	}
	c.items[key] = c.lru.PushFront(&entry{key: key, val: val})
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone
		return nil
	}
	// closed, cancel and closing must change together, here only.
	c.closed = true
	c.cancel()
	close(c.closing)
	c.mu.Unlock()
	defer close(c.closeDone)

	c.loads.Wait() // no lock held: runLoad needs mu to finish

	c.mu.Lock()
	clear(c.items)
	c.lru.Init()
	c.mu.Unlock()
	return nil
}
