package blockcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// Loader fetches the block for key. It runs at most once per in-flight miss,
// using a cache-scoped context rather than a caller's context. It must return
// promptly when ctx is canceled. Loads continue when all callers stop waiting,
// so applications with many abandoned distinct keys may have many active loads.
// A Loader must not call Close or synchronously wait on Get for its own key.
// A Loader panic is not recovered and follows Go's normal goroutine panic behavior.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent, single-flight cache of blocks.
type Cache interface {
	// Get returns a copy of the block, loading it on a miss.
	Get(ctx context.Context, key string) ([]byte, error)
	// Len returns the number of resident entries.
	Len() int
	// Close cancels and joins active loads, wakes waiters with ErrClosed, and
	// drops all resident entries. It is idempotent.
	Close() error
}

// ErrClosed is returned when an operation observes a closed cache.
var ErrClosed = errors.New("blockcache: closed")

type entry struct {
	key   string
	value []byte
}

type flight struct {
	done  chan struct{}
	value []byte
	err   error
}

type cache struct {
	mu       sync.Mutex
	capacity int
	load     Loader
	ctx      context.Context
	cancel   context.CancelFunc
	closed   bool
	closeCh  chan struct{}
	entries  map[string]*list.Element
	lru      list.List
	flights  map[string]*flight
	loaders  sync.WaitGroup
}

// New returns a cache holding at most capacity entries. It panics if capacity
// is less than one or load is nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity must be positive")
	}
	if load == nil {
		panic("blockcache: loader must not be nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity: capacity,
		load:     load,
		ctx:      ctx,
		cancel:   cancel,
		closeCh:  make(chan struct{}),
		entries:  make(map[string]*list.Element),
		flights:  make(map[string]*flight),
	}
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	if ctx == nil {
		panic("blockcache: nil context")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el := c.entries[key]; el != nil {
		c.lru.MoveToBack(el)
		stored := el.Value.(*entry).value
		c.mu.Unlock()
		return cloneBytes(stored), nil
	}
	f := c.flights[key]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		c.flights[key] = f
		// Add is serialized with the transition to closed, so Close cannot
		// begin waiting before this loader has been accounted for.
		c.loaders.Add(1)
		go c.runLoad(key, f)
	}
	c.mu.Unlock()

	select {
	case <-f.done:
	case <-ctx.Done():
	case <-c.closeCh:
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if !isClosed(f.done) {
		// Reaching this path means the caller context won the select. The
		// closed check above gives cache shutdown priority over cancellation.
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	value := f.value
	return cloneBytes(value), nil
}

func (c *cache) runLoad(key string, f *flight) {
	defer c.loaders.Done()
	loaded, err := c.load(c.ctx, key)
	var value []byte
	if err == nil {
		value = cloneBytes(loaded)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// closed and cancellation are set together under mu by Close; checking
	// both documents that canceled-context results must never be admitted.
	if c.closed || c.ctx.Err() != nil {
		f.value = nil
		f.err = ErrClosed
	} else {
		f.value = value
		f.err = err
		if err == nil {
			// A key cannot be resident while its flight is active. Use >= as a
			// defensive bound in case that invariant changes in a future edit.
			if len(c.entries) >= c.capacity {
				oldest := c.lru.Front()
				delete(c.entries, oldest.Value.(*entry).key)
				c.lru.Remove(oldest)
			}
			el := c.lru.PushBack(&entry{key: key, value: value})
			c.entries[key] = el
		}
	}
	delete(c.flights, key)
	close(f.done)
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
		c.cancel()
		c.entries = make(map[string]*list.Element)
		c.lru.Init()
	}
	c.mu.Unlock()
	c.loaders.Wait()
	// Keep closeCh open until all Loader calls have returned. Flights also
	// wake their waiters as they resolve during the join.
	c.mu.Lock()
	if !isClosed(c.closeCh) {
		close(c.closeCh)
	}
	c.mu.Unlock()
	return nil
}

// cloneBytes preserves the distinction between a nil result and a successful
// non-nil empty block.
func cloneBytes(src []byte) []byte {
	if src == nil {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
