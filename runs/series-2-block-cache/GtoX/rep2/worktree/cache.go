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

// ErrClosed is returned by Get after Close, and to waiters woken by Close.
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
		resident: make(map[string]*entry),
		order:    list.New(),
		inflight: make(map[string]*flight),
		base:     ctx,
		cancel:   cancel,
	}
}

type cache struct {
	mu       sync.Mutex
	capacity int
	load     Loader

	resident map[string]*entry // key -> resident entry; len <= capacity at every unlock
	order    *list.List        // front = MRU, back = LRU; Value is *entry
	inflight map[string]*flight

	base    context.Context    // passed to every Loader; never a caller's ctx
	cancel  context.CancelFunc // first Close calls this once
	closed  bool
	wg      sync.WaitGroup // one Add per started Loader goroutine; Done after publish
	closeMu sync.Mutex     // serializes Close; overlapping Close waits for the same join
}

type entry struct {
	key  string
	val  []byte        // immutable cache-owned copy; never the Loader's slice
	elem *list.Element // position in order
}

type flight struct {
	done chan struct{} // closed under mu after val/err are final; never sent on
	val  []byte        // immutable cache-owned copy on success; nil on error
	err  error         // Loader error, or nil
}

var _ Cache = (*cache)(nil)

// clone preserves nilness. nil -> nil; non-nil empty -> new non-nil empty slice;
// non-nil non-empty -> new slice with the same bytes.
func clone(src []byte) []byte {
	if src == nil {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

// Get returns a copy of the block for key, loading on a miss.
func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if e, ok := c.resident[key]; ok {
		c.order.MoveToFront(e.elem)
		snap := e.val
		c.mu.Unlock()
		return clone(snap), nil
	}
	if f, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		return c.wait(ctx, f)
	}
	f := &flight{done: make(chan struct{})}
	c.inflight[key] = f
	c.wg.Add(1)
	c.mu.Unlock()

	go c.publish(key, f)
	return c.wait(ctx, f)
}

// wait blocks until arbitration can decide. No select case returns a result;
// every wake re-enters arbitrate so closed, resolution, and caller cancel keep
// a single priority.
func (c *cache) wait(ctx context.Context, f *flight) ([]byte, error) {
	for {
		if bytes, err, decided := c.arbitrate(ctx, f); decided {
			return bytes, err
		}
		select {
		case <-ctx.Done():
		case <-f.done:
		case <-c.base.Done():
		}
	}
}

// arbitrate is the linearization point of a waiting Get. closed beats a
// resolved flight; a resolved flight beats caller cancellation.
func (c *cache) arbitrate(ctx context.Context, f *flight) ([]byte, error, bool) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed, true
	}
	select {
	case <-f.done:
		if f.err != nil {
			err := f.err
			c.mu.Unlock()
			return nil, err, true
		}
		// Snapshot under mu. Bytes are immutable, so cloning after unlock is
		// race-free and the header keeps the array alive across eviction.
		snap := f.val
		c.mu.Unlock()
		return clone(snap), nil, true
	default:
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err, true
	}
	c.mu.Unlock()
	return nil, nil, false
}

// publish runs the Loader and installs the result. Admission happens only when
// the cache is still open and the Loader returned no error.
func (c *cache) publish(key string, f *flight) {
	defer c.wg.Done()
	result, err := c.load(c.base, key)
	if err != nil {
		result = nil // error wins; drop the Loader slice
	} else {
		result = clone(result)
	}
	c.mu.Lock()
	if !c.closed && err == nil {
		c.admit(key, result)
	}
	f.val, f.err = result, err
	close(f.done)
	if c.inflight[key] == f {
		delete(c.inflight, key)
	}
	c.mu.Unlock()
}

// admit inserts key as MRU, evicting the LRU resident until there is room.
// Caller holds mu. The new key is not yet resident, so it cannot evict itself.
func (c *cache) admit(key string, val []byte) {
	for len(c.resident) >= c.capacity {
		back := c.order.Back()
		victim := back.Value.(*entry)
		c.order.Remove(back)
		delete(c.resident, victim.key)
	}
	e := &entry{key: key, val: val}
	e.elem = c.order.PushFront(e)
	c.resident[key] = e
}

// Len is the number of resident entries.
func (c *cache) Len() int {
	c.mu.Lock()
	n := len(c.resident)
	c.mu.Unlock()
	return n
}

// Close cancels in-flight loads, waits for their Loader calls to return, wakes
// every waiter with ErrClosed, drops all entries, and returns nil. Idempotent.
func (c *cache) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()

	c.mu.Lock()
	already := c.closed
	if !c.closed {
		c.closed = true
		c.resident = make(map[string]*entry)
		c.order = list.New()
	}
	c.mu.Unlock()
	if already {
		return nil
	}
	c.cancel()
	c.wg.Wait()
	return nil
}
