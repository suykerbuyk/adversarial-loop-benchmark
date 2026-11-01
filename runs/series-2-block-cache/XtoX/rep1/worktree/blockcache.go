package blockcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// Loader fetches a block from the backing store.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent block cache.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Len() int
	Close() error
}

var ErrClosed = errors.New("blockcache: closed")

type entry struct {
	key  string
	data []byte
}

type flight struct {
	done chan struct{}
	data []byte
	err  error
}

type cache struct {
	mu       sync.Mutex
	capacity int
	load     Loader
	entries  map[string]*list.Element
	lru      list.List // front is most recently used
	flights  map[string]*flight
	ctx      context.Context
	cancel   context.CancelFunc
	closed   bool
	closedCh chan struct{}
	loaders  sync.WaitGroup
}

func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity must be positive")
	}
	if load == nil {
		panic("blockcache: nil loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{capacity: capacity, load: load, entries: make(map[string]*list.Element), flights: make(map[string]*flight), ctx: ctx, cancel: cancel, closedCh: make(chan struct{})}
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if e := c.entries[key]; e != nil {
		c.lru.MoveToFront(e)
		b := clone(e.Value.(*entry).data)
		c.mu.Unlock()
		return b, nil
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
	case <-ctx.Done():
	case <-c.closedCh:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
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
	// A completion can race with the final nonblocking check only by taking
	// the cache lock, so recheck after the cancellation test.
	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		return clone(f.data), nil
	default:
		return nil, ctx.Err()
	}
}

func (c *cache) runLoad(key string, f *flight) {
	defer c.loaders.Done()
	b, err := c.load(c.ctx, key)
	if err == nil {
		b = clone(b)
	} else {
		b = nil
	}
	c.mu.Lock()
	if err == nil && !c.closed && c.ctx.Err() == nil {
		if len(c.entries) == c.capacity {
			old := c.lru.Back()
			delete(c.entries, old.Value.(*entry).key)
			c.lru.Remove(old)
		}
		e := &entry{key: key, data: b}
		c.entries[key] = c.lru.PushFront(e)
	}
	if c.closed || c.ctx.Err() != nil {
		f.data, f.err = nil, ErrClosed
	} else {
		f.data, f.err = b, err
	}
	delete(c.flights, key)
	close(f.done)
	c.mu.Unlock()
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
		close(c.closedCh)
		c.entries = make(map[string]*list.Element)
		c.lru.Init()
	}
	c.mu.Unlock()
	c.loaders.Wait()
	return nil
}
