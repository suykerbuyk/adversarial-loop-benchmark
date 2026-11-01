// Package blockcache provides a bounded, concurrent, single-flight block cache.
package blockcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// Loader fetches the block for key using a cache-scoped context. It must return
// promptly when that context is canceled. Calling Close from a Loader deadlocks,
// because Close waits for all Loaders to return. Calling Get for its own key
// waits on its own unresolved flight.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache stores blocks loaded from a backing store.
type Cache interface {
	// Get returns a copy of the block, loading on a miss. Concurrent misses for
	// one key share a single Loader call.
	Get(ctx context.Context, key string) ([]byte, error)
	// Len returns resident entries. It never exceeds capacity and may lag a load
	// completion by one critical section.
	Len() int
	// Close is idempotent. It cancels and joins Loaders, wakes waiters with
	// ErrClosed, and drops all resident entries.
	Close() error
}

// ErrClosed is returned by Gets admitted after Close begins and by waiters
// awakened by Close.
var ErrClosed = errors.New("blockcache: closed")
var errLoaderAborted = errors.New("blockcache: loader exited without returning")

type entry struct {
	key  string
	data []byte
	node *list.Element
}

type flight struct {
	done chan struct{}
	data []byte
	err  error
}

type cache struct {
	mu        sync.Mutex
	capacity  int
	load      Loader
	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	closedCh  chan struct{}
	closeDone chan struct{}
	entries   map[string]*entry
	lru       list.List // least recent at front, most recent at back
	flights   map[string]*flight
	loaders   sync.WaitGroup
}

// New creates a cache that holds at most capacity entries. It panics if
// capacity is less than one or load is nil. A successful nil or empty Loader
// result is returned as a non-nil empty slice.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity must be positive")
	}
	if load == nil {
		panic("blockcache: loader must not be nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity: capacity, load: load, ctx: ctx, cancel: cancel,
		closedCh: make(chan struct{}), closeDone: make(chan struct{}),
		entries: make(map[string]*entry), flights: make(map[string]*flight),
	}
}

func clone(data []byte) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	return out
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if e := c.entries[key]; e != nil {
		c.lru.MoveToBack(e.node)
		data := e.data
		c.mu.Unlock()
		return clone(data), nil
	}
	f := c.flights[key]
	start := f == nil
	if start {
		f = &flight{done: make(chan struct{})}
		c.flights[key] = f
		c.loaders.Add(1)
	}
	c.mu.Unlock()
	if start {
		go c.runLoader(key, f)
	}
	select {
	case <-f.done:
	case <-ctx.Done():
	case <-c.closedCh:
	}
	// Shutdown wins; then a completed flight wins over caller cancellation.
	select {
	case <-c.closedCh:
		return nil, ErrClosed
	default:
	}
	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		return clone(f.data), nil
	default:
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Defend against a malformed custom Context with Done closed and Err nil.
	return nil, context.Canceled
}

func (c *cache) runLoader(key string, f *flight) {
	var data []byte
	var err error
	returned := false
	defer func() {
		if !returned {
			// This also runs for Goexit and panic, publishing a terminal flight;
			// panic itself still propagates normally.
			err = errLoaderAborted
		}
		c.finish(key, f, data, err)
	}()
	data, err = c.load(c.ctx, key)
	returned = true
}

func (c *cache) finish(key string, f *flight, data []byte, err error) {
	if err != nil {
		data = nil
	} else {
		data = clone(data)
	}
	c.mu.Lock()
	// A flight is unique for a nonresident key; remove and admission are atomic.
	delete(c.flights, key)
	if !c.closed && err == nil {
		if len(c.entries) == c.capacity {
			oldest := c.lru.Front()
			victim := oldest.Value.(*entry)
			delete(c.entries, victim.key)
			c.lru.Remove(oldest)
		}
		e := &entry{key: key, data: data}
		e.node = c.lru.PushBack(e)
		c.entries[key] = e
	}
	f.data, f.err = data, err
	close(f.done)
	c.mu.Unlock()
	c.loaders.Done()
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		done := c.closeDone
		c.mu.Unlock()
		<-done
		return nil
	}
	c.closed = true
	c.cancel()
	// Wake Gets promptly; Close still joins Loaders before returning and late
	// completions cannot repopulate the cleared cache.
	close(c.closedCh)
	clear(c.entries)
	c.lru.Init()
	c.mu.Unlock()
	c.loaders.Wait()
	close(c.closeDone)
	return nil
}
