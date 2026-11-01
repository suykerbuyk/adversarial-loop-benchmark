package blockcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// Loader fetches the block associated with a key.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent block cache.
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
	value    []byte
	err      error
	resolved bool
}

type cache struct {
	mu           sync.Mutex
	capacity     int
	load         Loader
	items        map[string]*list.Element
	lru          *list.List
	flights      map[string]*flight
	closed       bool
	closeStarted chan struct{}
	closeDone    chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	loads        sync.WaitGroup
}

// New constructs a cache with the given resident-entry limit.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity must be positive")
	}
	if load == nil {
		panic("blockcache: loader must not be nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity:     capacity,
		load:         load,
		items:        make(map[string]*list.Element),
		lru:          list.New(),
		flights:      make(map[string]*flight),
		closeStarted: make(chan struct{}),
		closeDone:    make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}
}

func clone(b []byte) []byte {
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
	if el := c.items[key]; el != nil {
		c.lru.MoveToFront(el)
		value := clone(el.Value.(*entry).value)
		c.mu.Unlock()
		return value, nil
	}
	f := c.flights[key]
	start := false
	if f == nil {
		f = &flight{done: make(chan struct{})}
		c.flights[key] = f
		c.loads.Add(1)
		start = true
	}
	c.mu.Unlock()
	if start {
		go c.runLoad(key, f)
	}

	for {
		select {
		case <-f.done:
		case <-c.closeStarted:
		case <-ctx.Done():
		}

		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if f.resolved {
			value, err := clone(f.value), f.err
			c.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return value, nil
		}
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		c.mu.Unlock()
	}
}

func (c *cache) runLoad(key string, f *flight) {
	defer c.loads.Done()
	value, err := c.load(c.ctx, key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		value, err = nil, ErrClosed
	} else if err != nil {
		value = nil
	} else {
		value = clone(value)
		if len(c.items) == c.capacity {
			old := c.lru.Back()
			delete(c.items, old.Value.(*entry).key)
			c.lru.Remove(old)
		}
		el := c.lru.PushFront(&entry{key: key, value: value})
		c.items[key] = el
	}
	f.value, f.err, f.resolved = value, err, true
	delete(c.flights, key)
	close(f.done)
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
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
	close(c.closeStarted)
	c.cancel()
	c.mu.Unlock()

	c.loads.Wait()
	c.mu.Lock()
	c.items = make(map[string]*list.Element)
	c.lru.Init()
	c.mu.Unlock()
	close(c.closeDone)
	return nil
}
