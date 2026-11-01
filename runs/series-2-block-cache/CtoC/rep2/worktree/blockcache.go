// Package blockcache provides a bounded, concurrent, single-flight block cache
// in front of a slow backing store.
//
// Resident entries are kept in LRU order and bounded by a fixed capacity.
// Concurrent misses for the same key share one Loader call. Every Get returns
// a fresh copy of the block, so callers may mutate what they receive.
package blockcache

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
)

// Loader fetches the block for key. The cache calls it at most once per in-flight
// miss for a key (single-flight). It is given a cache-scoped context, not any
// single caller's. A Loader MUST return promptly when its ctx is Done.
//
// A Loader may call Len, and Get for other keys. If it calls Get for the key it
// is loading, with its own ctx (or one derived from it), that Get returns
// ErrSelfGet. A Loader must not call Close on its own cache: Close waits for
// every in-flight Loader call to return, so doing so deadlocks. A cycle of
// Loaders across keys (A's Loader gets B, B's Loader gets A) cannot be detected;
// it ends only when the callers' contexts are Done or the cache is closed.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent, single-flight block cache.
type Cache interface {
	// Get returns a COPY of the block for key, loading on a miss. Concurrent Gets
	// for the same absent key cause exactly one Loader call; all receive its result.
	//
	// ctx must be non-nil. A hit is served even if ctx is already Done; a miss
	// with a Done ctx returns ctx.Err() without starting or joining a load.
	Get(ctx context.Context, key string) ([]byte, error)
	// Len is the number of resident entries. Resident-only; it may lag a completion
	// by one critical section and is never above capacity.
	Len() int
	// Close cancels in-flight loads, waits for their Loader calls to return, wakes
	// every waiter with ErrClosed, drops all entries, and returns nil. Idempotent.
	Close() error
}

// ErrClosed is returned by Get once the cache has been closed.
var ErrClosed = errors.New("blockcache: closed")

// ErrSelfGet is returned by Get when it is called, with the Loader's ctx (or a ctx
// derived from it), from inside the Loader call that is loading that same key.
// Waiting would deadlock, because the load would wait on itself.
var ErrSelfGet = errors.New("blockcache: Loader called Get for the key it is loading")

// PanicError is the error waiters receive when the Loader panics. Like any Loader
// error it is not cached. It keeps the panic value and the stack, so a
// programming bug can be told apart from a transient load failure.
type PanicError struct {
	Key   string
	Value any    // the value passed to panic
	Stack []byte // debug.Stack() captured in the recovering goroutine
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("blockcache: loader panicked for key %q: %v\n\n%s", e.Key, e.Value, e.Stack)
}

// Unwrap returns the panic value if it is an error, else nil.
func (e *PanicError) Unwrap() error {
	err, _ := e.Value.(error)
	return err
}

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic(fmt.Sprintf("blockcache: New: capacity must be >= 1, got %d", capacity))
	}
	if load == nil {
		panic("blockcache: New: nil Loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity:  capacity,
		load:      load,
		ctx:       ctx,
		cancel:    cancel,
		entries:   make(map[string]*list.Element),
		lru:       list.New(),
		inflight:  make(map[string]*call),
		closedCh:  make(chan struct{}),
		closeDone: make(chan struct{}),
	}
}

type cache struct {
	capacity int
	load     Loader

	ctx    context.Context // cache-scoped; parent of every Loader ctx
	cancel context.CancelFunc

	mu       sync.Mutex               // guards closed, entries, lru, inflight
	closed   bool                     // set once, by the first Close
	entries  map[string]*list.Element // key -> element of lru; Value is *entry
	lru      *list.List               // front = MRU, back = LRU; resident entries only
	inflight map[string]*call         // unresolved loads; not in lru, not counted toward capacity

	closedCh  chan struct{}  // closed under mu in the critical section that sets closed
	loaders   sync.WaitGroup // one Add per Loader goroutine, only under mu while !closed
	closeDone chan struct{}  // closed when the first Close has finished its join
}

// entry is a resident block. val is cache-owned and never mutated.
type entry struct {
	key string
	val []byte
}

// call is one in-flight load shared by all of its waiters. val and err are
// written before done is closed; closing done publishes them.
type call struct {
	done chan struct{}
	val  []byte // cache-owned copy; nil on error
	err  error
}

type loadTagKey struct{}

// loadTag is attached to each Loader's ctx so a re-entrant own-key Get can be detected.
type loadTag struct {
	c   *cache
	key string
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	if t, ok := ctx.Value(loadTagKey{}).(*loadTag); ok && t.c == c && t.key == key {
		return nil, ErrSelfGet
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el, ok := c.entries[key]; ok {
		c.lru.MoveToFront(el)
		v := el.Value.(*entry).val
		c.mu.Unlock()
		return clone(v), nil
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	cl, ok := c.inflight[key]
	if !ok {
		cl = &call{done: make(chan struct{})}
		c.inflight[key] = cl
		c.loaders.Add(1) // under mu while !closed, so Close's Wait cannot miss it
		go c.runLoad(key, cl)
	}
	c.mu.Unlock()
	return c.wait(ctx, cl)
}

// wait blocks until the load resolves, ctx is Done, or the cache closes, then
// resolves in the priority order of the spec: closed, then the load's result,
// then ctx.Err() (ctx Done while the load is still in flight).
func (c *cache) wait(ctx context.Context, cl *call) ([]byte, error) {
	select {
	case <-cl.done:
	case <-ctx.Done():
	case <-c.closedCh:
	}
	select {
	case <-c.closedCh:
		return nil, ErrClosed
	default:
	}
	select {
	case <-cl.done:
		if cl.err != nil {
			return nil, cl.err
		}
		return clone(cl.val), nil
	default:
	}
	return nil, ctx.Err()
}

// runLoad runs the Loader for key and resolves cl. It continues even if every
// waiter has left, so an orphaned successful load still populates the cache.
func (c *cache) runLoad(key string, cl *call) {
	defer c.loaders.Done()

	val, err := c.callLoader(key)
	var owned []byte
	if err == nil {
		owned = clone(val) // never retain the Loader's slice
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, key) // a key has at most one call, and it is cl
	switch {
	case c.closed:
		// cancel is only called after closed is set, so this also covers a
		// Loader that returned after the cache-scoped ctx was cancelled.
		cl.err = ErrClosed
	case err != nil:
		cl.err = err
	default:
		if c.lru.Len() >= c.capacity {
			back := c.lru.Back()
			c.lru.Remove(back)
			delete(c.entries, back.Value.(*entry).key)
		}
		c.entries[key] = c.lru.PushFront(&entry{key: key, val: owned})
		cl.val = owned
	}
	close(cl.done)
}

// callLoader invokes the Loader under the cache-scoped ctx, converting a panic
// into a *PanicError.
func (c *cache) callLoader(key string) (val []byte, err error) {
	lctx := context.WithValue(c.ctx, loadTagKey{}, &loadTag{c: c, key: key})
	defer func() {
		if r := recover(); r != nil {
			val, err = nil, &PanicError{Key: key, Value: r, Stack: debug.Stack()}
		}
	}()
	return c.load(lctx, key)
}

func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone // a concurrent second Close waits for the same join
		return nil
	}
	c.closed = true
	close(c.closedCh) // wake every current waiter with ErrClosed
	c.entries = make(map[string]*list.Element)
	c.lru.Init()
	c.mu.Unlock()

	c.cancel()
	c.loaders.Wait()
	// Every runLoad saw closed, resolved its call with ErrClosed, and removed
	// its key from inflight; nothing new can be added, so inflight is empty.
	close(c.closeDone)
	return nil
}

// clone returns a fresh, non-nil copy of b.
func clone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
