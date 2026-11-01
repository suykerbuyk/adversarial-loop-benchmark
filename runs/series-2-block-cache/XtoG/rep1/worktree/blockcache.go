package blockcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// Loader fetches a block from the backing store.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent cache of blocks.
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
	done    chan struct{}
	value   []byte
	err     error
	doneSet bool
}

type cache struct {
	mu           sync.Mutex
	capacity     int
	load         Loader
	ctx          context.Context
	cancel       context.CancelFunc
	closed       bool
	shutdownDone chan struct{}
	loaders      sync.WaitGroup
	items        map[string]*list.Element
	lru          list.List
	flights      map[string]*flight
}

// New creates a cache with the given resident-entry capacity.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity must be positive")
	}
	if load == nil {
		panic("blockcache: loader must not be nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{capacity: capacity, load: load, ctx: ctx, cancel: cancel,
		shutdownDone: make(chan struct{}), items: make(map[string]*list.Element),
		flights: make(map[string]*flight)}
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el := c.items[key]; el != nil {
		c.lru.MoveToBack(el)
		v := el.Value.(*entry).value
		c.mu.Unlock()
		return clone(v), nil
	}
	f := c.flights[key]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		c.flights[key] = f
		c.loaders.Add(1)
		go c.loadKey(key, f)
	}
	c.mu.Unlock()

	select {
	case <-f.done:
	case <-ctx.Done():
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
		return clone(f.value), nil
	default:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A context may be canceled just after the select; the load is still pending.
		return nil, ctx.Err()
	}
}

func (c *cache) loadKey(key string, f *flight) {
	value, err := c.load(c.ctx, key)
	if err != nil {
		value = nil
	} else {
		value = clone(value)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed && err == nil {
		if len(c.items) == c.capacity {
			oldest := c.lru.Front()
			delete(c.items, oldest.Value.(*entry).key)
			c.lru.Remove(oldest)
		}
		el := c.lru.PushBack(&entry{key: key, value: value})
		c.items[key] = el
	}
	if c.closed {
		value = nil
		err = ErrClosed
	}
	f.value, f.err = value, err
	if c.flights[key] == f {
		delete(c.flights, key)
	}
	if !f.doneSet {
		f.doneSet = true
		close(f.done)
	}
	c.loaders.Done()
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		done := c.shutdownDone
		c.mu.Unlock()
		<-done
		return nil
	}
	c.closed = true
	c.cancel()
	for _, f := range c.flights {
		if !f.doneSet {
			f.doneSet = true
			close(f.done)
		}
	}
	c.mu.Unlock()

	c.loaders.Wait()
	c.mu.Lock()
	c.items = make(map[string]*list.Element)
	c.lru.Init()
	c.mu.Unlock()
	close(c.shutdownDone)
	return nil
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
