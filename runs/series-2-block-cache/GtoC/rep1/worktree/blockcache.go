package blockcache

import (
	"bytes"
	"context"
	"errors"
	"sync"
)

// Loader fetches the block for key. The cache calls it at most once per in-flight
// miss for a key (single-flight). It is given a cache-scoped context, not any
// single caller's. A Loader MUST return promptly when its ctx is Done.
type Loader func(ctx context.Context, key string) ([]byte, error)

type Cache interface {
	// Get returns a COPY of the block for key, loading on a miss. Concurrent Gets
	// for the same absent key cause exactly one Loader call; all receive its result.
	Get(ctx context.Context, key string) ([]byte, error)
	// Len is the number of resident entries. Resident-only; it may lag a completion
	// by one critical section and is never above capacity.
	Len() int
	// Close cancels in-flight loads, waits for their Loader calls to return, wakes
	// every waiter with ErrClosed, drops all entries, and returns nil. Idempotent.
	//
	// Close must not be called from a Loader, or from a goroutine that a Loader
	// waits on. Close waits for that Loader to return, so the call deadlocks.
	// The spec does not define this case; the note documents the inherent join.
	Close() error
}

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity < 1")
	}
	if load == nil {
		panic("blockcache: nil Loader")
	}
	loadCtx, loadCancel := context.WithCancel(context.Background())
	return &cache{
		capacity:   capacity,
		load:       load,
		loadCtx:    loadCtx,
		loadCancel: loadCancel,
		resident:   make(map[string]*entry),
		flights:    make(map[string]*flight),
	}
}

// ErrClosed is returned by Get after Close, and to waiters woken by Close.
var ErrClosed = errors.New("blockcache: closed")

// cache is the concrete Cache.
//
// mu guards closed, joined, resident, the LRU links, flights, and each
// flight's finished / result / err. mu is never held across a Loader call,
// a channel wait (including <-joined), or wg.Wait. Holding mu across
// <-joined deadlocks: the first Close closes joined only after wg.Wait, and
// wg.Wait returns only after the loader's publish takes mu.
//
// wg counts Loader goroutines that have been committed and have not finished.
// Add runs under mu only when !closed, in the same critical section as the
// flight insert, immediately before that section unlocks, and is always
// paired with a go statement that defers Done. The first Close sets closed
// under mu and only then, after releasing mu, calls wg.Wait. If the leader's
// critical section happens-before Close's, Add has happened. If Close's
// happens-before the leader's, the leader sees closed and does not Add. Only
// the Close that created joined calls wg.Wait.
//
// loadCancel runs only after closed = true has been published (written under
// mu, then mu unlocked). A goroutine that observes loadCtx.Done() and then
// locks mu therefore observes closed == true.
type cache struct {
	capacity int
	load     Loader

	// loadCtx is cancelled by the first Close, and only then. Every Loader
	// call receives this context, never a caller's context and never a child
	// that is also cancelled when a caller cancels.
	loadCtx    context.Context
	loadCancel context.CancelFunc

	// joined is the idempotent Close join. Nil until the first Close.
	// That Close creates it under mu, and after wg.Wait closes it once.
	// A later Close copies the pointer under mu and receives outside mu.
	// Not a sync.Once: Once.Do waits on an internal mutex, and
	// testing/synctest does not treat a mutex wait as durably blocked, so
	// a second Close could not be observed with synctest.Wait. A channel
	// receive is durably blocked.
	joined chan struct{}

	// wg counts Loader goroutines that have been committed and have not
	// finished. Add only under mu, only when !closed, immediately before
	// the critical section that committed the flight unlocks, and always
	// paired with a go statement that defers Done. Wait only from the
	// Close that created joined, after closed is set and mu is released.
	wg sync.WaitGroup

	mu     sync.Mutex // guards the fields below, plus flight.finished/result/err
	closed bool

	// resident maps key -> entry. len(resident) <= capacity at every unlock,
	// including under a concurrent Len. The map is allocated in New and
	// cleared, never niled, so a missed closed-check cannot panic on write.
	resident map[string]*entry

	// head is MRU, tail is LRU. Every resident entry is on this list and
	// every list node is in resident. Recency updates happen only under mu.
	head, tail *entry

	// flights maps key -> in-flight load. A flight is not resident, does not
	// count toward capacity, and is not evictable. At most one flight per key.
	// A key is never both resident and in flights.
	flights map[string]*flight
}

var _ Cache = (*cache)(nil)

// entry is one resident block. data is immutable once the entry is linked
// into resident. Concurrent readers of those bytes are race-free. A caller
// may snapshot the slice header under mu and clone outside the lock.
// Eviction unlinks the entry; it does not mutate the bytes.
type entry struct {
	key  string
	data []byte
	prev *entry
	next *entry
}

// flight is one in-flight load. done is a wake-up only, closed exactly once
// by the loader after finished, result, and err are published under mu.
// Waiters must not infer priority from which select case fired. Close does
// not close done. finished is the source of truth for "resolved".
// Publication order under mu is: delete the flight, set result / err /
// finished, close(done).
type flight struct {
	key      string
	done     chan struct{}
	finished bool
	result   []byte // cache-owned copy, or nil
	err      error
}

// clone returns a fresh copy. bytes.Clone(nil) is nil. bytes.Clone of an
// empty non-nil slice is empty and non-nil. Callers can tell those apart.
func clone(b []byte) []byte {
	return bytes.Clone(b)
}

// decide is the only waiter outcome rule.
//
//	closed                 -> ErrClosed, even if the flight also finished and even if callerErr != nil
//	finished               -> the load result (clone on success, nil slice on error), even if callerErr != nil
//	callerErr != nil       -> callerErr (load still in flight, cache still open)
//	otherwise              -> panic
//
// The panic names a non-conforming context (Done closed, Err nil) so it is
// not mistaken for a cache bug. A loop-back would busy-spin if loadCtx stayed
// done while closed were false, so the branch panics instead of retrying.
func decide(closed, finished bool, result []byte, loadErr, callerErr error) ([]byte, error) {
	if closed {
		return nil, ErrClosed
	}
	if finished {
		if loadErr != nil {
			return nil, loadErr
		}
		return clone(result), nil
	}
	if callerErr != nil {
		return nil, callerErr
	}
	panic("blockcache: non-conforming context: Done closed but Err is nil")
}

func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if e, ok := c.resident[key]; ok {
		data := e.data
		c.touch(e)
		c.mu.Unlock()
		return clone(data), nil
	}
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		return c.wait(ctx, f)
	}
	f := &flight{key: key, done: make(chan struct{})}
	c.flights[key] = f
	c.wg.Add(1)
	c.mu.Unlock()
	go c.runLoad(f)
	return c.wait(ctx, f)
}

func (c *cache) wait(ctx context.Context, f *flight) ([]byte, error) {
	select {
	case <-f.done:
	case <-ctx.Done():
	case <-c.loadCtx.Done():
	}
	c.mu.Lock()
	closed, finished := c.closed, f.finished
	result, loadErr := f.result, f.err
	c.mu.Unlock()
	return decide(closed, finished, result, loadErr, ctx.Err())
}

func (c *cache) runLoad(f *flight) {
	defer c.wg.Done()
	data, err := c.load(c.loadCtx, f.key)
	c.publish(f, data, err)
}

// publish stores a successful load or discards it. The store clone runs once,
// before mu, only when err == nil. data is not stored. The closed re-check is
// after the clone, under mu. If the cache closed during the clone, owned is
// dropped and the closed branch runs. There is no second clone under mu.
func (c *cache) publish(f *flight, data []byte, err error) {
	var owned []byte
	if err == nil {
		owned = clone(data)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.flights[f.key] == f {
		delete(c.flights, f.key)
	}
	f.finished = true
	if c.closed {
		// drop owned; do not admit. Waiters that see closed return
		// ErrClosed from decide and do not read f.err. Set it anyway
		// so the published flight is self-consistent.
		f.result = nil
		f.err = ErrClosed
		close(f.done)
		return
	}
	if err != nil {
		// includes (non-nil bytes, non-nil err): error wins, bytes discarded
		f.result = nil
		f.err = err
		close(f.done)
		return
	}
	c.admit(f.key, owned)
	f.result = owned
	f.err = nil
	close(f.done)
}

// admit inserts key as MRU. Caller holds mu. The cache is open and key is
// not resident. Evicts the LRU resident until there is room. Flights are not
// on the list and are not deleted here.
func (c *cache) admit(key string, owned []byte) {
	if _, ok := c.resident[key]; ok {
		panic("blockcache: admit of resident key")
	}
	for len(c.resident) >= c.capacity {
		if c.tail == nil {
			panic("blockcache: resident and LRU list disagree")
		}
		c.evict(c.tail)
	}
	e := &entry{key: key, data: owned}
	c.resident[key] = e
	c.linkMRU(e)
}

func (c *cache) evict(e *entry) {
	c.unlink(e)
	delete(c.resident, e.key)
}

// linkMRU links e, which is not currently linked, as the most-recent entry.
// Caller holds mu.
func (c *cache) linkMRU(e *entry) {
	e.prev = nil
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	} else {
		c.tail = e
	}
	c.head = e
}

// unlink removes a linked entry from the LRU list. Caller holds mu.
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

// touch marks a resident hit most-recently-used. Caller holds mu.
func (c *cache) touch(e *entry) {
	if e == c.head {
		return
	}
	c.unlink(e)
	c.linkMRU(e)
}

func (c *cache) Len() int {
	c.mu.Lock()
	n := len(c.resident)
	c.mu.Unlock()
	return n
}

// Close cancels in-flight loads, waits for their Loader calls to return, wakes
// every waiter with ErrClosed, drops all entries, and returns nil. Idempotent.
//
// A second Close copies joined under mu and receives outside mu. It does not
// call wg.Wait or loadCancel. The receive must not run under mu: the first
// Close closes joined only after wg.Wait, and wg.Wait returns only after
// publish takes mu.
//
// Close must not be called from a Loader, or from a goroutine that a Loader
// waits on. Close waits for that Loader to return, so the call deadlocks.
func (c *cache) Close() error {
	c.mu.Lock()
	if c.joined != nil {
		ch := c.joined
		c.mu.Unlock()
		<-ch
		return nil
	}
	ch := make(chan struct{})
	c.joined = ch
	c.closed = true
	clear(c.resident)
	c.head, c.tail = nil, nil
	clear(c.flights)
	c.mu.Unlock()
	c.loadCancel()
	c.wg.Wait()
	close(ch)
	return nil
}
