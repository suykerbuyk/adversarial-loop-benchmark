// Package blockcache provides a bounded, concurrent, single-flight block cache
// in front of a slow backing store.
//
// Design notes (judgment calls):
//   - Rule 7's priority (closed > caller ctx Done while in flight > load result)
//     is decided at the first linearizing event under the cache mutex: the load's
//     resolution, Close, or the waiter observing its own ctx while still unsettled.
//     It is not decided by whichever channel a goroutine happens to observe first
//     at wake-up, so a cancellation that arrives after resolution cannot override
//     the result.
//   - A waiter settled with its load result before Close returns that result even
//     if Close runs before the waiter goroutine is scheduled; it is no longer a
//     "current waiter".
//   - A miss with an already-Done ctx still starts the load (it is orphaned and
//     populates the cache); a hit with a Done ctx returns the data.
//   - A Loader returning (nil, nil) caches an empty, non-nil block.
//   - A Loader panic is converted to an error so waiters never hang.
//   - Close fixes every current waiter's outcome as ErrClosed and wakes it before
//     joining in-flight Loaders, so waiters are not held hostage by a slow Loader.
package blockcache

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
)

// Loader fetches the block for key. The cache calls it at most once per in-flight
// miss for a key (single-flight). It is given a cache-scoped context, not any
// single caller's. A Loader MUST return promptly when its ctx is Done.
type Loader func(ctx context.Context, key string) ([]byte, error)

// Cache is a bounded, concurrent, single-flight block cache. All methods are
// safe for concurrent use.
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

// ErrClosed is returned by Get once the cache has been closed.
var ErrClosed = errors.New("blockcache: closed")

// entry is one resident block.
type entry struct {
	key string
	val []byte // cache-owned; never mutated after store; never handed out directly
}

// waiter is one Get blocked on a flight. Its outcome is settled exactly once,
// under cache.mu, by whichever of resolution, Close, or the waiter itself (on
// observing its ctx Done) gets there first.
type waiter struct {
	ctx     context.Context
	settled bool   // guarded by cache.mu
	val     []byte // guarded by cache.mu; shared immutable flight value
	err     error  // guarded by cache.mu
}

// flight is one in-flight Loader call.
type flight struct {
	done    chan struct{}        // closed once, after the Loader returned and waiters were settled
	waiters map[*waiter]struct{} // guarded by cache.mu; unsettled waiters only
}

type cache struct {
	capacity int
	load     Loader
	ctx      context.Context // cache-scoped; passed to every Loader call
	cancel   context.CancelFunc

	closing    chan struct{} // closed by the first Close (under mu): wakes every waiter
	closedDone chan struct{} // closed at the end of the first Close: joins later Closes

	mu       sync.Mutex
	closed   bool
	lru      *list.List               // front = MRU; elements hold *entry; resident only
	entries  map[string]*list.Element // resident only
	inflight map[string]*flight       // unresolved loads only
}

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic(fmt.Sprintf("blockcache: capacity must be >= 1, got %d", capacity))
	}
	if load == nil {
		panic("blockcache: nil Loader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity:   capacity,
		load:       load,
		ctx:        ctx,
		cancel:     cancel,
		closing:    make(chan struct{}),
		closedDone: make(chan struct{}),
		lru:        list.New(),
		entries:    make(map[string]*list.Element),
		inflight:   make(map[string]*flight),
	}
}

// Get implements Cache.
func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if el, ok := c.entries[key]; ok {
		c.lru.MoveToFront(el)
		v := el.Value.(*entry).val
		c.mu.Unlock()
		return bytes.Clone(v), nil
	}
	f, ok := c.inflight[key]
	if !ok {
		f = &flight{done: make(chan struct{}), waiters: make(map[*waiter]struct{})}
		c.inflight[key] = f
		go c.run(key, f)
	}
	w := &waiter{ctx: ctx}
	f.waiters[w] = struct{}{}
	c.mu.Unlock()
	return c.await(ctx, f, w)
}

// await blocks until w is woken and returns its settled outcome. The select is
// only a wake-up; the outcome is whatever was settled first under c.mu.
func (c *cache) await(ctx context.Context, f *flight, w *waiter) ([]byte, error) {
	select {
	case <-f.done:
	case <-c.closing:
	case <-ctx.Done():
	}

	c.mu.Lock()
	if !w.settled {
		// Only reachable via ctx.Done(): resolution and Close settle every
		// registered waiter under mu before closing their channel, so the load is
		// still in flight and the cache is open => rule 7(b).
		delete(f.waiters, w)
		w.settled, w.err = true, ctx.Err()
	}
	val, err := w.val, w.err
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return bytes.Clone(val), nil
}

// run performs the single Loader call for a flight and resolves it.
func (c *cache) run(key string, f *flight) {
	b, err := c.callLoader(key)
	var val []byte
	if err == nil {
		// Rule 4: the cache keeps its own copy, never the Loader's slice.
		val = bytes.Clone(b)
		if val == nil {
			val = []byte{}
		}
	}
	c.resolve(key, f, val, err)
}

// callLoader invokes the Loader under the cache-scoped context, converting a
// panic into an error.
func (c *cache) callLoader(key string) (b []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			b, err = nil, fmt.Errorf("blockcache: loader panicked: %v", r)
		}
	}()
	return c.load(c.ctx, key)
}

// resolve records a flight's outcome: admits a successful value (unless the
// cache is closed), settles every still-registered waiter, and closes f.done.
func (c *cache) resolve(key string, f *flight, val []byte, err error) {
	c.mu.Lock()
	if c.inflight[key] == f {
		delete(c.inflight, key)
	}
	// If closed, the result arrived after the cache ctx was cancelled (closed is
	// set before cancel): discard it. Close already settled all waiters.
	if !c.closed {
		if err == nil {
			c.admit(key, val)
		}
		for w := range f.waiters {
			w.settled = true
			switch {
			case w.ctx.Err() != nil:
				// The caller's ctx was Done while the load was in flight: rule 7(b).
				w.err = w.ctx.Err()
			case err != nil:
				w.err = err
			default:
				w.val = val
			}
		}
		clear(f.waiters)
	}
	c.mu.Unlock()
	close(f.done)
}

// admit stores val as the MRU entry for key, evicting the LRU entry first if
// full. Caller holds c.mu.
func (c *cache) admit(key string, val []byte) {
	if el, ok := c.entries[key]; ok { // defensive; unreachable (a key is never resident and in flight)
		el.Value.(*entry).val = val
		c.lru.MoveToFront(el)
		return
	}
	if c.lru.Len() >= c.capacity {
		back := c.lru.Back()
		c.lru.Remove(back)
		delete(c.entries, back.Value.(*entry).key)
	}
	c.entries[key] = c.lru.PushFront(&entry{key: key, val: val})
}

// Len implements Cache.
func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// Close implements Cache.
func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		d := c.closedDone
		c.mu.Unlock()
		<-d
		return nil
	}
	c.closed = true
	flights := make([]*flight, 0, len(c.inflight))
	for _, f := range c.inflight {
		for w := range f.waiters {
			w.settled, w.err = true, ErrClosed
		}
		clear(f.waiters)
		flights = append(flights, f)
	}
	close(c.closing)
	c.mu.Unlock()

	c.cancel()
	for _, f := range flights {
		<-f.done
	}

	c.mu.Lock()
	clear(c.entries)
	c.lru.Init()
	c.mu.Unlock()
	close(c.closedDone)
	return nil
}
