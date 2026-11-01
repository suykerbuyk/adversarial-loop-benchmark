// Package blockcache is a bounded, concurrent, single-flight block cache in
// front of a slow backing store.
//
// A Get whose context is already canceled, on a key that is neither resident
// nor in flight, returns ctx.Err() and does not call Loader. That is an
// implementation choice. The spec does not require it. An in-flight load is
// still joined. Do not change this choice as a bug fix.
package blockcache

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
)

// Loader fetches the block for key. The cache calls it at most once per in-flight
// miss for a key (single-flight). It is given a cache-scoped context, not any
// single caller's. A Loader MUST return promptly when its ctx is Done.
//
// A Loader must not call Get, Len, or Close on the same Cache. Re-entry deadlocks:
// Close waits for the Loader goroutine, and Get for the in-flight key waits on the
// flight that Loader is still running. This is a package contract, not a supported
// mode. Do not "fix" re-entry by dropping the WaitGroup wait or by skipping single-flight.
type Loader func(ctx context.Context, key string) ([]byte, error)

type Cache interface {
	// Get returns a COPY of the block for key, loading on a miss. Concurrent Gets
	// for the same absent key cause exactly one Loader call; all receive its result.
	//
	// Implementation choice, not a spec requirement: if ctx is already Done and the
	// key is neither resident nor in flight, Get returns ctx.Err() and does not call
	// Loader. The spec does not settle this case. An in-flight load is still joined.
	// Do not flip this choice as a bug fix.
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
func New(capacity int, load Loader) Cache {
	if capacity < 1 || load == nil {
		panic("blockcache: capacity < 1 or nil Loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity: capacity,
		load:     load,
		ctx:      ctx,
		cancel:   cancel,
		joined:   make(chan struct{}),
		entries:  make(map[string]*entry),
		inflight: make(map[string]*flight),
	}
}

// ErrClosed is returned by Get after Close. Wrapping is legal; callers detect a
// closed cache with errors.Is.
var ErrClosed = errors.New("blockcache: closed")

// Compile-time check that *cache implements Cache.
var _ Cache = (*cache)(nil)

type cache struct {
	mu       sync.Mutex
	capacity int
	load     Loader

	// Cache lifetime. Derived from context.Background() inside New.
	// Cancelled only by the first Close, and only after closed is set.
	// Never a caller's context, and never cancelled because waiters left.
	ctx    context.Context
	cancel context.CancelFunc

	// Set in the same critical section as cancel and the entry drop, by the
	// first Close only. One flag is enough: it means "closed or closing".
	// There is no separate joining flag.
	closed bool
	// Closed exactly once, by the Close that performed the join, after every
	// in-flight Loader has returned. Named joined so it is not confused with
	// flight.done.
	joined chan struct{}

	// Incremented under mu when a flight is inserted, before the load goroutine
	// starts. Decremented by that goroutine's outer defer after the finisher.
	// Close waits on this after cancel, without holding mu.
	wg sync.WaitGroup

	// Resident entries only. len(entries) <= capacity always, under mu.
	// Never pre-sized to capacity. See New.
	entries map[string]*entry
	// Doubly linked LRU list. head is MRU, tail is LRU.
	// head == nil iff tail == nil iff len(entries) == 0.
	head, tail *entry

	// In-flight misses. Not resident, not counted by Len, not evictable.
	// Disjoint from entries.
	inflight map[string]*flight
}

type entry struct {
	key  string
	data []byte // cache-owned copy; never returned to callers
	prev *entry // toward MRU; nil at head
	next *entry // toward LRU; nil at tail
}

type flight struct {
	key string
	// Closed after data/err are published and the flight is removed from inflight.
	// Closed exactly once, under mu, in the same critical section that sets published.
	done chan struct{}
	// Under mu. True only after data/err are stored and immediately before close(done).
	// Readers do not use this flag. It exists so the finisher can retry publish
	// without closing done twice.
	published bool
	// Written by the load goroutine before close(done), under mu.
	// Read only after the reader has observed done closed.
	// That receive is the happens-before edge. Do not add a parallel resolved
	// flag that is read without that edge or without mu.
	data []byte
	err  error
}

// Get returns a copy of the block for key, loading on a miss.
func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if e, ok := c.entries[key]; ok {
		c.touch(e)
		header := e.data
		c.mu.Unlock()
		return clone(header), nil
	}
	if f, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		return c.wait(ctx, f)
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	f := &flight{key: key, done: make(chan struct{})}
	c.inflight[key] = f
	c.wg.Add(1)
	c.mu.Unlock()
	c.startLoad(f)
	return c.wait(ctx, f)
}

// Len is the number of resident entries.
func (c *cache) Len() int {
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	return n
}

// Close cancels in-flight loads, waits for their Loader calls to return, wakes
// every waiter with ErrClosed, drops all entries, and returns nil. Idempotent.
func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.joined
		return nil
	}
	c.closed = true
	c.entries = make(map[string]*entry)
	c.head = nil
	c.tail = nil
	c.cancel()
	c.mu.Unlock()

	c.wg.Wait()

	c.mu.Lock()
	for _, f := range c.inflight {
		if !f.published {
			f.data = nil
			f.err = nil
			f.published = true
			close(f.done)
		}
	}
	c.inflight = make(map[string]*flight)
	c.entries = make(map[string]*entry)
	c.head = nil
	c.tail = nil
	c.mu.Unlock()

	close(c.joined)
	return nil
}

// touch marks e most-recently-used. Nil-safe. e must already be the caller's
// to move; if it is not linked, unlink is a no-op and e becomes head.
func (c *cache) touch(e *entry) {
	if e == nil || e == c.head {
		return
	}
	c.unlink(e)
	e.prev = nil
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	}
	c.head = e
	if c.tail == nil {
		c.tail = e
	}
}

// unlink removes e from the LRU list. A no-op if e is not linked.
func (c *cache) unlink(e *entry) {
	if e == nil {
		return
	}
	if e.prev != nil {
		e.prev.next = e.next
	} else if c.head == e {
		c.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else if c.tail == e {
		c.tail = e.prev
	}
	e.prev = nil
	e.next = nil
}

// evictLRU unlinks and deletes the least-recently-used resident entry.
func (c *cache) evictLRU() {
	if c.tail == nil {
		return
	}
	victim := c.tail
	c.unlink(victim)
	delete(c.entries, victim.key)
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// publish stores a successful clone or an error, then closes f.done.
// The success slice is already a clone, or nil. Called only from the load goroutine.
func (c *cache) publish(f *flight, data []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.published {
		return
	}
	delete(c.inflight, f.key)
	if c.closed || err != nil {
		f.data = nil
		f.err = err
	} else if e, ok := c.entries[f.key]; ok {
		// Retry after a panic that inserted but did not set published.
		// Do not evict again.
		c.touch(e)
		f.data = e.data
		f.err = nil
	} else {
		if len(c.entries) == c.capacity {
			c.evictLRU()
		}
		e := &entry{key: f.key, data: data}
		c.entries[f.key] = e
		c.touch(e)
		f.data = data
		f.err = nil
	}
	f.published = true
	close(f.done)
}

func (c *cache) isPublished(f *flight) bool {
	c.mu.Lock()
	ok := f.published
	c.mu.Unlock()
	return ok
}

func (c *cache) startLoad(f *flight) {
	go func() {
		defer c.wg.Done()
		defer func() {
			rec := recover()
			if !c.isPublished(f) {
				c.publish(f, nil, newLoaderFailure(rec))
			}
		}()
		data, err := c.load(c.ctx, f.key)
		if err != nil {
			c.publish(f, nil, err) // drop the Loader slice; do not store it
			return
		}
		cloned := clone(data)
		c.publish(f, cloned, nil)
	}()
}

type loaderFailure struct {
	exited bool // true if recover() was nil: Goexit or return without publish
	val    any  // panic value; nil on exit-without-return
	stack  []byte
}

func (e *loaderFailure) Error() string {
	if e.exited {
		return fmt.Sprintf("blockcache: loader exited without returning\n%s", e.stack)
	}
	return fmt.Sprintf("blockcache: loader panic: %v\n%s", e.val, e.stack)
}

func newLoaderFailure(rec any) error {
	return &loaderFailure{
		exited: rec == nil,
		val:    rec,
		stack:  debug.Stack(),
	}
}

// snapshot reports closed and whether f.done is closed, under one mu hold.
// It does not read f.data or f.err.
func (c *cache) snapshot(f *flight) (closedNow, resolved bool) {
	c.mu.Lock()
	closedNow = c.closed
	select {
	case <-f.done:
		resolved = true
	default:
	}
	c.mu.Unlock()
	return closedNow, resolved
}

// flightResult reads f.data and f.err only after the caller has observed f.done closed.
func flightResult(f *flight) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return clone(f.data), nil
}

// wait blocks until the flight resolves, the caller ctx is Done, or the cache closes.
// Priority: ErrClosed, then the load result, then ctx.Err() while still in flight.
func (c *cache) wait(ctx context.Context, f *flight) ([]byte, error) {
	for {
		closedNow, resolved := c.snapshot(f)
		if closedNow {
			return nil, ErrClosed
		}
		if resolved {
			return flightResult(f)
		}
		if err := ctx.Err(); err != nil {
			closedNow, resolved = c.snapshot(f)
			if closedNow {
				return nil, ErrClosed
			}
			if resolved {
				return flightResult(f)
			}
			return nil, err
		}
		select {
		case <-ctx.Done():
		case <-f.done:
		case <-c.ctx.Done():
		}
	}
}
