// Package blockcache implements a bounded, concurrent cache for blocks.
package blockcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// Loader fetches the block for a key. It must return promptly when its context
// is canceled.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded cache with per-key single-flight loading.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Len() int
	Close() error
}

var ErrClosed = errors.New("blockcache: closed")

type entry struct {
	key   string
	value []byte
}

type flight struct {
	done     chan struct{}
	resolved bool
	value    []byte
	err      error
}

type cache struct {
	mu       sync.Mutex
	capacity int
	load     Loader
	closed   bool

	ctx    context.Context
	cancel context.CancelFunc
	stop   chan struct{}

	items     *list.List
	residents map[string]*list.Element
	flights   map[string]*flight
	loaders   sync.WaitGroup
}

// New constructs a cache with the given maximum resident entry count.
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
		ctx:       ctx,
		cancel:    cancel,
		stop:      make(chan struct{}),
		items:     list.New(),
		residents: make(map[string]*list.Element),
		flights:   make(map[string]*flight),
	}
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el := c.residents[key]; el != nil {
		c.items.MoveToFront(el)
		value := clone(el.Value.(*entry).value)
		c.mu.Unlock()
		return value, nil
	}
	f := c.flights[key]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		c.flights[key] = f
		c.loaders.Add(1)
		go c.runLoad(key, f)
	}
	c.mu.Unlock()

	select {
	case <-f.done:
	case <-c.stop:
	case <-ctx.Done():
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if !f.resolved {
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		// The only remaining wakeup is shutdown, whose closed state is set
		// under this mutex before stop is closed.
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if f.err != nil {
		err := f.err
		c.mu.Unlock()
		return nil, err
	}
	value := clone(f.value)
	c.mu.Unlock()
	return value, nil
}

func (c *cache) runLoad(key string, f *flight) {
	var value []byte
	var err error
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		value, err = c.load(c.ctx, key)
	}()
	if panicValue != nil {
		err = errors.New("blockcache: loader panicked")
	}
	if err != nil {
		value = nil
	} else {
		value = clone(value)
	}

	c.mu.Lock()
	f.value, f.err, f.resolved = value, err, true
	if !c.closed && err == nil {
		if el := c.residents[key]; el != nil {
			el.Value.(*entry).value = value
			c.items.MoveToFront(el)
		} else {
			el := c.items.PushFront(&entry{key: key, value: value})
			c.residents[key] = el
			if c.items.Len() > c.capacity {
				last := c.items.Back()
				old := last.Value.(*entry)
				delete(c.residents, old.key)
				c.items.Remove(last)
			}
		}
	}
	if c.flights[key] == f {
		delete(c.flights, key)
	}
	close(f.done)
	c.mu.Unlock()
	c.loaders.Done()
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items.Len()
}

func (c *cache) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.cancel()
		close(c.stop)
		c.items.Init()
		clear(c.residents)
	}
	c.mu.Unlock()

	c.loaders.Wait()
	c.mu.Lock()
	c.items.Init()
	clear(c.residents)
	c.mu.Unlock()
	return nil
}

func clone(value []byte) []byte {
	if value == nil {
		return nil
	}
	out := make([]byte, len(value))
	copy(out, value)
	return out
}
