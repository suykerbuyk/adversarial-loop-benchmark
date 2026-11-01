// Package blockcache provides a bounded concurrent cache for loaded blocks.
package blockcache

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
)

// Loader fetches the block for key. It must return promptly after ctx is cancelled.
// A panic from a Loader is recovered and returned to all callers waiting on
// that load as an error; the flight is then removed so a later Get can retry.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent, single-flight block cache.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Len() int
	Close() error
}

// ErrClosed is returned by Gets that observe a closed cache.
var ErrClosed = errors.New("blockcache: closed")

type entry struct {
	data []byte
	item *list.Element
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
	closeCh   chan struct{}
	closeDone chan struct{}
	flights   map[string]*flight
	entries   map[string]*entry
	lru       list.List
	loaders   sync.WaitGroup
}

// New creates a cache that holds at most capacity entries.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity must be positive")
	}
	if load == nil {
		panic("blockcache: nil loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity:  capacity,
		load:      load,
		ctx:       ctx,
		cancel:    cancel,
		closeCh:   make(chan struct{}),
		closeDone: make(chan struct{}),
		flights:   make(map[string]*flight),
		entries:   make(map[string]*entry),
	}
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if e := c.entries[key]; e != nil {
		c.lru.MoveToBack(e.item)
		data := bytes.Clone(e.data)
		c.mu.Unlock()
		return data, nil
	}
	f := c.flights[key]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		c.flights[key] = f
		c.loaders.Add(1)
		go c.runLoad(key, f)
	}
	c.mu.Unlock()

	for {
		select {
		case <-f.done:
		case <-c.closeCh:
		case <-ctx.Done():
		}

		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		select {
		case <-f.done:
			data, err := bytes.Clone(f.data), f.err
			c.mu.Unlock()
			return data, err
		default:
		}
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		c.mu.Unlock()
	}
}

func (c *cache) runLoad(key string, f *flight) {
	defer c.loaders.Done()
	var data []byte
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				// Treat loader panics as terminal flight errors so waiters are
				// released and Close can join the loader goroutine.
				err = fmt.Errorf("blockcache: loader panic: %v", p)
				data = nil
			}
		}()
		data, err = c.load(c.ctx, key)
	}()
	if err != nil {
		data = nil
	} else {
		data = bytes.Clone(data)
	}

	c.mu.Lock()
	if !c.closed && c.ctx.Err() == nil && err == nil {
		if len(c.entries) == c.capacity {
			oldest := c.lru.Front()
			if oldest != nil {
				oldKey := oldest.Value.(string)
				delete(c.entries, oldKey)
				c.lru.Remove(oldest)
			}
		}
		item := c.lru.PushBack(key)
		c.entries[key] = &entry{data: data, item: item}
	}
	f.data, f.err = data, err
	if c.flights[key] == f {
		delete(c.flights, key)
	}
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
	if c.closed {
		done := c.closeDone
		c.mu.Unlock()
		<-done
		return nil
	}
	c.closed = true
	close(c.closeCh)
	c.cancel()
	c.mu.Unlock()

	c.loaders.Wait()
	c.mu.Lock()
	c.entries = make(map[string]*entry)
	c.lru.Init()
	c.flights = make(map[string]*flight)
	c.mu.Unlock()
	close(c.closeDone)
	return nil
}
