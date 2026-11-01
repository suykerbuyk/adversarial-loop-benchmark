package blockcache

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
)

// Loader fetches the block for key. It receives a cache-scoped context rather
// than a caller's context, and must return promptly when that context is done.
// A Loader should not call Close or recursively Get the same key: either call
// waits for the Loader itself to finish. A panic from a Loader is recovered and
// returned as a load error.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent block cache. Every successful Get returns a
// fresh copy of the stored bytes. Gets for one absent key share a single load.
type Cache interface {
	// Get returns a copy of key's block, loading it if needed. It returns ErrClosed
	// after Close. Caller cancellation stops only that caller's wait; an
	// in-flight load continues, including when the caller is already canceled.
	Get(ctx context.Context, key string) ([]byte, error)
	// Len returns the number of resident entries. It may lag a load completion
	// by one critical section and never exceeds the cache capacity.
	Len() int
	// Close cancels and joins in-flight loads, wakes their waiters with ErrClosed,
	// discards late results, drops resident entries, and is idempotent. It waits
	// for Loader calls to return.
	Close() error
}

// ErrClosed is returned by Get when the cache has been closed.
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
	entries  map[string]*list.Element
	lru      list.List
	inflight map[string]*flight

	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	closing   chan struct{}
	closeDone chan struct{}
	loads     sync.WaitGroup
}

// New creates a cache with room for at most capacity resident blocks. It
// panics if capacity is less than one or load is nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity must be positive")
	}
	if load == nil {
		panic("blockcache: loader must not be nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity:  capacity,
		load:      load,
		entries:   make(map[string]*list.Element),
		inflight:  make(map[string]*flight),
		ctx:       ctx,
		cancel:    cancel,
		closing:   make(chan struct{}),
		closeDone: make(chan struct{}),
	}
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el := c.entries[key]; el != nil {
		c.lru.MoveToFront(el)
		value := copyBytes(el.Value.(*entry).value)
		c.mu.Unlock()
		return value, nil
	}

	f := c.inflight[key]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		c.inflight[key] = f
		c.loads.Add(1)
		go c.runLoad(key, f)
	}
	c.mu.Unlock()

	select {
	case <-f.done:
	case <-c.closing:
	case <-ctx.Done():
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	select {
	case <-f.done:
		return copyBytes(f.value), f.err
	default:
		return nil, ctx.Err()
	}
}

func (c *cache) runLoad(key string, f *flight) {
	var value []byte
	var err error
	returned := false
	defer func() {
		// Goexit skips recover and the statements after invokeLoader, so make
		// publication and WaitGroup release unconditional.
		if !returned {
			value = nil
			err = errors.New("blockcache: loader exited without returning")
		}
		if err == nil {
			value = copyBytes(value)
		} else {
			value = nil
		}

		c.mu.Lock()
		// Close cancels c.ctx in the same critical section that sets closed.
		if c.closed {
			f.value = nil
			f.err = ErrClosed
		} else {
			f.value = value
			f.err = err
			if err == nil {
				if len(c.entries) >= c.capacity {
					oldest := c.lru.Back()
					old := oldest.Value.(*entry)
					delete(c.entries, old.key)
					c.lru.Remove(oldest)
				}
				el := c.lru.PushFront(&entry{key: key, value: value})
				c.entries[key] = el
			}
		}
		if c.inflight[key] == f {
			delete(c.inflight, key)
		}
		close(f.done)
		c.mu.Unlock()
		c.loads.Done()
	}()

	value, err = invokeLoader(c.load, c.ctx, key)
	returned = true
}

// invokeLoader turns a callback panic into a load error so the flight is still
// published and its wait-group slot is released.
func invokeLoader(load Loader, ctx context.Context, key string) (value []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			value = nil
			err = fmt.Errorf("blockcache: loader panic: %v", recovered)
		}
	}()
	return load(ctx, key)
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
	c.entries = make(map[string]*list.Element)
	c.lru.Init()
	close(c.closing)
	c.mu.Unlock()

	c.loads.Wait()
	close(c.closeDone)
	return nil
}
