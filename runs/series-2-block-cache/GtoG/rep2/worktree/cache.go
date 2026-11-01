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

// ErrClosed is returned by Get after the cache has been closed.
var ErrClosed = errors.New("blockcache: closed")

// errLoaderFailed is published only when go panics before the loader goroutine
// exists, or when the Loader itself panics. It is not cached. It is not ErrClosed.
var errLoaderFailed = errors.New("blockcache: loader failed to start")

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity < 1")
	}
	if load == nil {
		panic("blockcache: nil Loader")
	}
	root, cancel := context.WithCancel(context.Background())
	c := &cache{
		capacity: capacity,
		load:     load,
		root:     root,
		cancel:   cancel,
		resident: make(map[string]*entry),
		inflight: make(map[string]*flight),
		order:    list.New(),
	}
	c.closeWG.Add(1)
	return c
}

type cache struct {
	capacity int
	load     Loader

	root   context.Context    // cache-scoped parent of every Loader call
	cancel context.CancelFunc // first Close only; CancelFunc is idempotent

	mu       sync.Mutex
	closed   bool
	resident map[string]*entry // successful admits only; len <= capacity whenever mu is released
	inflight map[string]*flight
	order    *list.List     // front = MRU, back = LRU; elements are *entry
	joining  sync.WaitGroup // one Add per flight whose loader goroutine was launched, or whose launch failed
	waiters  sync.WaitGroup // one Add per Get that observed !closed
	closeWG  sync.WaitGroup // 1 until the first Close finishes join + waiter drain + drop
}

type entry struct {
	key  string
	data []byte        // cache-owned clone; never the Loader's slice, never a caller's slice
	elem *list.Element // position in order
}

type flight struct {
	done chan struct{} // closed exactly once, under mu, after data/err are published
	data []byte        // cache-owned clone on published success; nil on error or discard
	err  error         // Loader error, or the spawn-failure error; never ErrClosed
}

var _ Cache = (*cache)(nil)

// cloneBytes copies src.
// A nil src stays nil. A non-nil src, including length 0, is a new slice of the same length.
// make returns a non-nil slice when the length argument is 0, so a non-nil empty payload
// stays non-nil and does not alias src.
func cloneBytes(src []byte) []byte {
	if src == nil {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

// snapshot reports the waiter result.
// waiting == true means the load is still in flight and the caller is still waiting;
// the caller must then block in select and call snapshot again.
// waiting == false means data/err is the return value; the caller must not select again.
func (c *cache) snapshot(ctx context.Context, f *flight) (data []byte, err error, waiting bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed, false
	}
	select {
	case <-f.done:
		// Resolved in this same hold as the closed check.
		if f.err != nil {
			return nil, f.err, false
		}
		return cloneBytes(f.data), nil, false
	default:
	}
	// f.done is still open. Priority (b) is decided here, before the caller waits again.
	if err = ctx.Err(); err != nil {
		return nil, err, false
	}
	return nil, nil, true
}

func (c *cache) wait(ctx context.Context, f *flight) ([]byte, error) {
	for {
		data, err, waiting := c.snapshot(ctx, f)
		if !waiting {
			return data, err
		}
		// Durable block. This select has no return.
		if done := ctx.Done(); done == nil {
			select {
			case <-f.done:
			case <-c.root.Done():
			}
		} else {
			select {
			case <-f.done:
			case <-done:
			case <-c.root.Done():
			}
		}
	}
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.waiters.Add(1)
	defer c.waiters.Done()

	if ent, ok := c.resident[key]; ok {
		c.order.MoveToFront(ent.elem)
		data := cloneBytes(ent.data)
		c.mu.Unlock()
		return data, nil
	}
	if f, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		return c.wait(ctx, f) // join even when ctx.Err() != nil
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err // cold miss, already done: do not insert, do not call Loader
	}
	f := &flight{done: make(chan struct{})}
	c.inflight[key] = f
	c.joining.Add(1)
	c.mu.Unlock()

	c.spawn(key, f)
	return c.wait(ctx, f)
}

func (c *cache) spawn(key string, f *flight) {
	started := false
	defer func() {
		if started {
			return
		}
		// go did not return. runLoad will not run and will not Done.
		c.finish(key, f, nil, errLoaderFailed)
		c.joining.Done()
	}()
	go c.runLoad(key, f)
	started = true
}

func (c *cache) runLoad(key string, f *flight) {
	defer c.joining.Done()
	defer func() {
		if rec := recover(); rec != nil {
			c.finish(key, f, nil, errLoaderFailed)
			panic(rec)
		}
	}()
	raw, err := c.load(c.root, key) // cache-scoped ctx, never the caller's
	if err != nil {
		c.finish(key, f, nil, err)
		return
	}
	c.finish(key, f, cloneBytes(raw), nil)
}

func (c *cache) finish(key string, f *flight, owned []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-f.done:
		return // already published
	default:
	}
	if c.inflight[key] == f {
		delete(c.inflight, key)
	}
	if err != nil || c.closed {
		// Error wins over accompanying bytes. Closed discards success too.
		// Do not store. Do not publish success bytes.
		// When closed, leave f.err nil so a waiter cannot observe a Loader
		// context.Canceled; the snapshot returns ErrClosed first anyway.
		if err != nil && !c.closed {
			f.err = err
		}
		f.data = nil
		close(f.done)
		return
	}
	// Success and cache still open.
	// closed, set before cancel, is the only discard bit.
	// Caller-ctx cancellation must not discard: this load is orphaned-but-valid under rule 7.
	f.data = owned
	f.err = nil
	for len(c.resident) >= c.capacity {
		elem := c.order.Back()
		if elem == nil {
			break
		}
		old := elem.Value.(*entry)
		delete(c.resident, old.key)
		c.order.Remove(elem)
	}
	ent := &entry{key: key, data: owned}
	ent.elem = c.order.PushFront(ent)
	c.resident[key] = ent
	close(f.done)
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.resident)
}

func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.closeWG.Wait()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	c.cancel()       // after the flag; wakes Loaders via root and blocked waiters via root.Done
	c.joining.Wait() // every loader goroutine has returned, or a failed spawn has Done
	c.waiters.Wait() // every Get that observed !closed has returned

	c.mu.Lock()
	c.resident = make(map[string]*entry)
	c.order.Init()
	c.inflight = make(map[string]*flight) // finish already deleted its flight; clear anyway
	c.mu.Unlock()

	c.closeWG.Done()
	return nil
}
