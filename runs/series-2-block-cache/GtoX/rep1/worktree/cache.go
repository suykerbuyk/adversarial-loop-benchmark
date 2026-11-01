package blockcache

import (
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

// ErrClosed is returned by Get after Close, and by waiters woken by Close.
var ErrClosed = errors.New("blockcache: closed")

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity must be >= 1")
	}
	if load == nil {
		panic("blockcache: nil Loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity:    capacity,
		load:        load,
		baseCtx:     ctx,
		cancel:      cancel,
		resident:    make(map[string]*entry),
		inflight:    make(map[string]*flight),
		doneClosing: make(chan struct{}),
		joined:      make(chan struct{}),
	}
}

// cache is the concrete Cache. mu protects the maps, the LRU list, closed, and
// publication of a flight result. The Loader runs outside mu.
type cache struct {
	mu       sync.Mutex
	capacity int
	load     Loader

	baseCtx context.Context
	cancel  context.CancelFunc

	closed      bool
	doneClosing chan struct{} // closed once, when closed is set
	joined      chan struct{} // closed once, after the first Close finishes joining

	resident map[string]*entry
	head     *entry // MRU
	tail     *entry // LRU

	inflight map[string]*flight
	loading  sync.WaitGroup

	// testWaitHook, if set, runs inside wait after mu is dropped and before the
	// select. Tests use it to pin a waiter in the not-ready, not-closed window.
	// Production code leaves it nil. The hook is called without mu held.
	testWaitHook func()
}

// entry is a resident block. value is a cache-owned copy.
type entry struct {
	key   string
	value []byte
	prev  *entry
	next  *entry
}

// flight is one in-flight Loader call for a key. done is closed only after
// result, err, and ready are published under mu.
type flight struct {
	key    string
	done   chan struct{}
	result []byte
	err    error
	ready  bool
}

// Get returns a copy of the block for key, loading on a miss.
func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if e := c.resident[key]; e != nil {
		c.touch(e)
		out := clone(e.value)
		c.mu.Unlock()
		return out, nil
	}
	if f := c.inflight[key]; f != nil {
		c.mu.Unlock()
		return c.wait(ctx, f)
	}
	// Cold miss: this caller owns the single flight. The Loader runs on its own
	// goroutine so a cancelled waiter cannot stall Close, and so an orphaned
	// success can still populate the cache.
	f := &flight{key: key, done: make(chan struct{})}
	c.inflight[key] = f
	c.loading.Add(1)
	c.mu.Unlock()
	go c.runLoad(f)
	return c.wait(ctx, f)
}

// closedForTest reports whether Close has set the closed flag. It exists so
// tests can wait until a blocked Get is past the point where ErrClosed wins,
// without racing the caller-cancel path.
func (c *cache) closedForTest() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Len is the number of resident entries. It never includes in-flight keys and
// never exceeds capacity.
func (c *cache) Len() int {
	c.mu.Lock()
	n := len(c.resident)
	c.mu.Unlock()
	return n
}

// Close cancels in-flight loads, waits for their Loader calls to return, wakes
// every waiter with ErrClosed, drops all entries, and returns nil. Idempotent.
func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.joined
		return nil
	}
	c.closed = true
	close(c.doneClosing)
	c.resident = make(map[string]*entry)
	c.head = nil
	c.tail = nil
	c.mu.Unlock()

	c.cancel()
	c.loading.Wait()
	close(c.joined)
	return nil
}

// wait blocks until the flight resolves, the cache closes, or ctx is done.
// Priority is ErrClosed, then ctx.Err() while the load is still in flight, then
// the load result. Leaving does not cancel the flight.
func (c *cache) wait(ctx context.Context, f *flight) ([]byte, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if f.ready {
			out := clone(f.result)
			err := f.err
			c.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return out, nil
		}
		callerDone := ctx.Err()
		hook := c.testWaitHook
		c.mu.Unlock()
		if callerDone != nil {
			return nil, callerDone
		}
		if hook != nil {
			// Pin the waiter immediately before the select so a test can publish
			// closed, and a caller cancel, before either case is chosen.
			hook()
		}
		select {
		case <-f.done:
		case <-c.doneClosing:
		case <-ctx.Done():
		}
	}
}

// runLoad invokes the Loader under the cache-scoped context and publishes the
// result. A non-nil error, or a completion after Close, is not stored.
func (c *cache) runLoad(f *flight) {
	defer c.loading.Done()

	val, err := c.load(c.baseCtx, f.key)
	// Copy before any other cache work so the cache never retains the Loader's
	// slice. A non-nil error wins and the bytes are discarded.
	var owned []byte
	if err != nil {
		val = nil
	} else {
		owned = clone(val)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.inflight, f.key)

	// Close sets closed before it cancels baseCtx. A Loader that loses the race
	// still observes one of the two, so a late success is discarded.
	if err != nil || c.closed || c.baseCtx.Err() != nil {
		f.result = nil
		f.err = err
		f.ready = true
		close(f.done)
		return
	}

	f.result = owned
	f.err = nil
	f.ready = true
	if _, ok := c.resident[f.key]; !ok {
		if len(c.resident) == c.capacity {
			c.evictOne()
		}
		e := &entry{key: f.key, value: owned}
		c.resident[f.key] = e
		c.insertMRU(e)
	}
	close(f.done)
}

// touch moves e to the MRU head. e must already be linked.
func (c *cache) touch(e *entry) {
	if c.head == e {
		return
	}
	c.unlink(e)
	c.insertMRU(e)
}

// insertMRU places e at the head. e must not currently be linked.
func (c *cache) insertMRU(e *entry) {
	e.prev = nil
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	} else {
		c.tail = e
	}
	c.head = e
}

// unlink removes e from the LRU list.
func (c *cache) unlink(e *entry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		c.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		c.tail = e.prev
	}
	e.prev = nil
	e.next = nil
}

// evictOne drops the LRU resident entry. Called only when the cache is full
// and a new key is about to be inserted.
func (c *cache) evictOne() {
	e := c.tail
	if e == nil {
		return
	}
	c.unlink(e)
	delete(c.resident, e.key)
}

// clone returns a cache-owned copy of b. A nil slice stays nil.
func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
