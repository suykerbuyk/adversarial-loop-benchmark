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

// ErrClosed is returned by Get after Close.
var ErrClosed = errors.New("blockcache: closed")

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity < 1")
	}
	if load == nil {
		panic("blockcache: nil loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity: capacity,
		load:     load,
		ctx:      ctx,
		cancel:   cancel,
		closing:  make(chan struct{}),
		entries:  make(map[string]*entry),
		inflight: make(map[string]*flight),
	}
}

type cache struct {
	capacity int
	load     Loader

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	closing  chan struct{}
	entries  map[string]*entry
	mru      *entry // front: most recently used
	lru      *entry // back: least recently used
	inflight map[string]*flight

	loads sync.WaitGroup
}

type entry struct {
	key  string
	val  []byte
	prev *entry
	next *entry
}

type flight struct {
	done  chan struct{}
	val   []byte
	err   error
	ready bool
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if e, ok := c.entries[key]; ok {
		c.moveToMRU(e)
		val := clone(e.val)
		c.mu.Unlock()
		return val, nil
	}
	if f, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		return c.wait(ctx, f)
	}

	f := &flight{done: make(chan struct{})}
	c.inflight[key] = f
	c.loads.Add(1)
	c.mu.Unlock()

	go c.runLoad(key, f)
	return c.wait(ctx, f)
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *cache) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.closing)
		c.cancel()
		c.entries = make(map[string]*entry)
		c.mru = nil
		c.lru = nil
	}
	c.mu.Unlock()

	c.loads.Wait()

	c.mu.Lock()
	c.inflight = make(map[string]*flight)
	c.mu.Unlock()
	return nil
}

func (c *cache) runLoad(key string, f *flight) {
	defer c.loads.Done()

	val, err := c.load(c.ctx, key)
	if err != nil {
		val = nil
	} else {
		val = clone(val)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.inflight, key)

	store := err == nil && !c.closed && c.ctx.Err() == nil
	if store {
		for len(c.entries) >= c.capacity {
			c.evictLRU()
		}
		e := &entry{key: key, val: val}
		c.entries[key] = e
		c.insertMRU(e)
	} else if err == nil {
		// Discarded success (close/cancel). Waiters must not observe a hit.
		val = nil
	}

	f.val = val
	f.err = err
	f.ready = true
	close(f.done)
}

func (c *cache) wait(ctx context.Context, f *flight) ([]byte, error) {
	select {
	case <-c.closing:
		return nil, ErrClosed
	case <-ctx.Done():
		select {
		case <-c.closing:
			return nil, ErrClosed
		default:
			return nil, ctx.Err()
		}
	case <-f.done:
		select {
		case <-c.closing:
			return nil, ErrClosed
		default:
		}
		if f.err != nil {
			return nil, f.err
		}
		return clone(f.val), nil
	}
}

func (c *cache) insertMRU(e *entry) {
	e.prev = nil
	e.next = c.mru
	if c.mru != nil {
		c.mru.prev = e
	}
	c.mru = e
	if c.lru == nil {
		c.lru = e
	}
}

func (c *cache) moveToMRU(e *entry) {
	if c.mru == e {
		return
	}
	c.unlink(e)
	c.insertMRU(e)
}

func (c *cache) evictLRU() {
	victim := c.lru
	if victim == nil {
		return
	}
	c.unlink(victim)
	delete(c.entries, victim.key)
}

func (c *cache) unlink(e *entry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		c.mru = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		c.lru = e.prev
	}
	e.prev = nil
	e.next = nil
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
