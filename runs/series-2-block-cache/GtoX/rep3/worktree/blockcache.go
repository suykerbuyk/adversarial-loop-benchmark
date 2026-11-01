// Package blockcache is a bounded, concurrent, single-flight block cache in
// front of a slow backing store.
package blockcache

import (
	"context"
	"errors"
	"sync"
)

// Loader fetches the block for key. The cache calls it at most once per in-flight
// miss for a key (single-flight). It is given a cache-scoped context, not any
// single caller's. A Loader MUST return promptly when its ctx is Done.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded single-flight block cache.
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

// ErrClosed is returned by Get after Close. Callers should use errors.Is.
var ErrClosed = errors.New("blockcache: closed")

// entry is one resident block. val is a cache-owned copy.
// prev points toward the LRU end; next points toward the MRU end.
type entry struct {
	key  string
	val  []byte
	prev *entry
	next *entry
}

// flight is one in-flight single-flight load. It is not resident and does not
// count toward capacity until it resolves successfully and is admitted.
//
// result and err are published under the cache mutex before done is closed.
// Waiters read them under the same mutex after observing done.
type flight struct {
	key    string
	done   chan struct{}
	result []byte
	err    error
	gen    uint64
}

// cache is the concrete Cache. The mutex guards closed, closeGen, resident,
// inflight, the LRU list, and n. Loader calls and caller waits run outside it.
type cache struct {
	capacity int
	load     Loader
	root     context.Context
	stop     context.CancelFunc

	mu        sync.Mutex
	closed    bool
	closeGen  uint64
	resident  map[string]*entry
	inflight  map[string]*flight
	head      *entry // sentinel; head.next is the least-recently-used entry
	tail      *entry // sentinel; tail.prev is the most-recently-used entry
	n         int
	loads     sync.WaitGroup
	closedCh  chan struct{} // closed once, when Close begins
	doneClose chan struct{} // closed once, after the first Close finishes joining
}

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity < 1")
	}
	if load == nil {
		panic("blockcache: nil Loader")
	}
	root, stop := context.WithCancel(context.Background())
	head := &entry{}
	tail := &entry{}
	head.next = tail
	tail.prev = head
	return &cache{
		capacity:  capacity,
		load:      load,
		root:      root,
		stop:      stop,
		resident:  make(map[string]*entry),
		inflight:  make(map[string]*flight),
		head:      head,
		tail:      tail,
		closedCh:  make(chan struct{}),
		doneClose: make(chan struct{}),
	}
}

// Get returns a copy of the block for key.
func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if e, ok := c.resident[key]; ok {
		c.unlink(e)
		c.linkMRU(e)
		val := e.val
		c.mu.Unlock()
		return clone(val), nil
	}
	f, ok := c.inflight[key]
	if !ok {
		// Insert before starting the loader so concurrent misses join this flight.
		f = &flight{
			key:  key,
			done: make(chan struct{}),
			gen:  c.closeGen,
		}
		c.inflight[key] = f
		c.loads.Add(1)
		c.mu.Unlock()
		go c.runLoad(f)
		return c.wait(ctx, f)
	}
	c.mu.Unlock()
	return c.wait(ctx, f)
}

// wait blocks until the flight resolves, the caller ctx is done, or the cache closes.
// Priority is ErrClosed, then ctx.Err() while the load is still in flight, then the
// load result. A resolved load wins over a done caller ctx. Leaving does not cancel
// the load; an orphaned success still populates the cache.
func (c *cache) wait(ctx context.Context, f *flight) ([]byte, error) {
	for {
		select {
		case <-c.closedCh:
		case <-ctx.Done():
		case <-f.done:
		}

		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		select {
		case <-f.done:
			result, err := f.result, f.err
			c.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return clone(result), nil
		default:
		}
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		// Spurious wake (ctx and close not fired, flight not done). Wait again
		// with the lock released so Close and the loader can proceed.
		c.mu.Unlock()
	}
}

// runLoad executes the Loader under the cache-scoped context. The caller's ctx
// is never passed in, so the load continues after every waiter has left.
func (c *cache) runLoad(f *flight) {
	defer c.loads.Done()

	b, err := c.load(c.root, f.key)
	var owned []byte
	if err == nil {
		owned = clone(b)
	}

	c.mu.Lock()
	if err != nil {
		// Error wins: discard any accompanying bytes and do not retain the slice.
		f.err = err
		f.result = nil
	} else {
		f.result = owned
	}
	// Admit before publishing. Otherwise a waiter can return and a new Get can
	// miss in the window where the flight is gone and the key is not resident yet.
	// A result that lands after Close, including a zero-waiter load, is discarded.
	if err == nil && !c.closed && f.gen == c.closeGen {
		c.admit(f.key, owned)
	}
	if c.inflight[f.key] == f {
		delete(c.inflight, f.key)
	}
	close(f.done)
	c.mu.Unlock()
}

// admit inserts a successfully loaded block and marks it most-recently-used.
// Caller holds c.mu. The key is not already resident. val is cache-owned.
func (c *cache) admit(key string, val []byte) {
	if c.n == c.capacity {
		c.evictLRU()
	}
	e := &entry{key: key, val: val}
	c.resident[key] = e
	c.linkMRU(e)
	c.n++
}

// evictLRU drops the least-recently-used resident entry. Caller holds c.mu.
func (c *cache) evictLRU() {
	victim := c.head.next
	c.unlink(victim)
	delete(c.resident, victim.key)
	c.n--
}

func (c *cache) unlink(e *entry) {
	e.prev.next = e.next
	e.next.prev = e.prev
	e.prev = nil
	e.next = nil
}

// linkMRU places e just before the MRU sentinel.
func (c *cache) linkMRU(e *entry) {
	e.prev = c.tail.prev
	e.next = c.tail
	c.tail.prev.next = e
	c.tail.prev = e
}

// Len is the number of resident entries and is never above capacity.
func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// Close cancels in-flight loads, waits for them, and drops every entry.
// A second Close waits for the same join and returns nil.
func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		done := c.doneClose
		c.mu.Unlock()
		<-done
		return nil
	}
	c.closed = true
	c.closeGen++
	close(c.closedCh)
	c.stop()
	c.mu.Unlock()

	c.loads.Wait()

	c.mu.Lock()
	c.resident = make(map[string]*entry)
	c.inflight = make(map[string]*flight)
	c.head.next = c.tail
	c.tail.prev = c.head
	c.n = 0
	close(c.doneClose)
	c.mu.Unlock()
	return nil
}

// clone copies b into a fresh slice. Nil stays nil; a non-nil empty slice stays
// non-nil so callers can tell the two apart.
func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
