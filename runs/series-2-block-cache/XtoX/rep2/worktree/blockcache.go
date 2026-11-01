package blockcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// Loader fetches the block for a key. Loads use a cache-scoped context.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded concurrent cache of blocks.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Len() int
	Close() error
}

var ErrClosed = errors.New("blockcache: closed")

type entry struct {
	key   string
	value []byte
	elem  *list.Element
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
	items    map[string]*entry
	lru      *list.List
	flights  map[string]*flight

	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	closing   chan struct{}
	closeDone chan struct{}
	loaders   sync.WaitGroup
}

// New creates a cache with the requested resident-entry capacity.
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
		load: load,
		items: make(map[string]*entry),
		lru: list.New(),
		flights: make(map[string]*flight),
		ctx: ctx,
		cancel: cancel,
		closing: make(chan struct{}),
		closeDone: make(chan struct{}),
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
	if e := c.items[key]; e != nil {
		c.lru.MoveToBack(e.elem)
		value := clone(e.value)
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
	case <-ctx.Done():
	case <-c.closing:
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	select {
	case <-ctx.Done():
		// Cancellation wins only while this flight is unresolved.
		select {
		case <-f.done:
		default:
			return nil, ctx.Err()
		}
	default:
	}
	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		return clone(f.value), nil
	default:
		return nil, ctx.Err()
	}
}

func (c *cache) runLoad(key string, f *flight) {
	defer c.loaders.Done()
	value, err := c.load(c.ctx, key)
	if err == nil {
		value = clone(value)
	} else {
		value = nil
	}

	c.mu.Lock()
	if c.closed {
		value = nil
		err = ErrClosed
	} else if err == nil {
		if len(c.items) == c.capacity {
			oldest := c.lru.Front()
			old := oldest.Value.(*entry)
			delete(c.items, old.key)
			c.lru.Remove(oldest)
		}
		e := &entry{key: key, value: value}
		e.elem = c.lru.PushBack(e)
		c.items[key] = e
	}
	f.value, f.err = value, err
	delete(c.flights, key)
	close(f.done)
	c.mu.Unlock()
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
	close(c.closing)
	c.cancel()
	c.items = make(map[string]*entry)
	c.lru.Init()
	c.mu.Unlock()

	c.loaders.Wait()
	close(c.closeDone)
	return nil
}
