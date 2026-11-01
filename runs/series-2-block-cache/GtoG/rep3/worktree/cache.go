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

// ErrClosed is returned by Get after Close, and to waiters woken by Close.
var ErrClosed = errors.New("blockcache: closed")

type flightState int

const (
	flightPending flightState = iota
	flightOK
	flightErr
	flightDiscard
)

// entry is a resident block. val is cache-owned and never aliased with a
// Loader slice, a flight result, or a slice handed to a caller.
type entry struct {
	key  string
	val  []byte
	elem *list.Element
}

// flight is one in-flight Loader call. done is closed exactly once, after
// state is terminal. val is a private copy when state is flightOK.
type flight struct {
	done  chan struct{}
	val   []byte
	err   error
	state flightState
}

// cache holds at most capacity resident entries. In-flight loads are absent
// from entries and from the LRU list until they resolve successfully.
type cache struct {
	capacity int
	load     Loader

	mu       sync.Mutex
	closed   bool
	root     context.Context
	cancel   context.CancelFunc
	entries  map[string]*entry
	inflight map[string]*flight
	lru      *list.List

	// loads counts in-flight runLoad goroutines. Get's Add sits in the same
	// critical section as the closed check, after that check, and Done is
	// deferred at the start of runLoad. A second Close waits on this same
	// WaitGroup. An Add after Wait has observed zero panics, so do not move
	// Add out of that section or ahead of the closed check.
	loads sync.WaitGroup
}

// New returns a Cache holding at most capacity entries. It panics if
// capacity < 1 or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity < 1")
	}
	if load == nil {
		panic("blockcache: nil Loader")
	}
	root, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity: capacity,
		load:     load,
		root:     root,
		cancel:   cancel,
		entries:  make(map[string]*entry),
		inflight: make(map[string]*flight),
		lru:      list.New(),
	}
}

// Get returns a fresh copy of the block for key. A nil Loader success is a
// cached empty block: the returned slice is non-nil and empty, so a hit is
// distinguishable from an error return, which is a nil slice and a non-nil
// error. Callers may mutate the returned slice.
//
// Get must not be called from a Loader running against this cache. The
// leader's own Get is already waiting on the flight that Loader belongs to,
// and a same-key Get from that Loader waits on the same flight. Close must
// not be called from a Loader either: Close joins the runLoad goroutine
// that is still inside that Loader.
func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if e, ok := c.entries[key]; ok {
		out := clone(e.val)
		c.lru.MoveToBack(e.elem)
		c.mu.Unlock()
		return out, nil
	}
	if f, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		return c.wait(ctx, f)
	}

	f := &flight{done: make(chan struct{})}
	c.inflight[key] = f
	c.loads.Add(1)
	c.mu.Unlock()

	go c.runLoad(key, f)
	return c.wait(ctx, f)
}

// runLoad calls the Loader under the cache-scoped context and publishes the
// result. It does not recover: a panicking Loader is outside the contract.
func (c *cache) runLoad(key string, f *flight) {
	defer c.loads.Done()
	loaded, loadErr := c.load(c.root, key)
	c.mu.Lock()
	c.publish(key, f, loaded, loadErr)
	c.mu.Unlock()
}

// publish records the terminal state of f. Called only with mu held, and only
// from runLoad. Close is the only writer of flightDiscard: it moves every
// pending flight and deletes inflight before it unlocks, so this function
// always returns at state != flightPending once the cache has closed. It must
// no-op in that case. Do not publish a late Loader result here, and do not
// close done again; Close already owns both.
func (c *cache) publish(key string, f *flight, loaded []byte, loadErr error) {
	if f.state != flightPending {
		return
	}
	if c.inflight[key] == f {
		delete(c.inflight, key)
	}
	if loadErr != nil {
		f.state = flightErr
		f.err = loadErr
		f.val = nil
	} else {
		e := c.admit(key, loaded)
		f.state = flightOK
		f.val = clone(e.val)
		f.err = nil
	}
	close(f.done)
}

// admit inserts a successful load as MRU, evicting the LRU resident first
// while the cache is full. Called only from publish, with mu held, and only
// when the cache is not closed.
func (c *cache) admit(key string, loaded []byte) *entry {
	for len(c.entries) >= c.capacity {
		front := c.lru.Front()
		victim := front.Value.(*entry)
		c.lru.Remove(front)
		delete(c.entries, victim.key)
	}
	e := &entry{key: key, val: clone(loaded)}
	e.elem = c.lru.PushBack(e)
	c.entries[key] = e
	return e
}

// wait parks until the flight resolves, the cache closes, or this caller's
// ctx is Done. This goroutine does not select on ctx.Done and does not
// register context.AfterFunc. A non-bubble Done channel in the same select
// as f.done is not a durable block, and closing that channel from outside
// a synctest bubble panics once a bubble goroutine has received on it.
//
// The wake is a per-waiter channel created here. A child started by this
// Get closes it when the caller ctx is Done. In a synctest bubble that
// child is in the bubble, so the close is an inside event and the parent's
// select stays a durable block. The child's other case is f.done, so it
// leaves when the load finishes and does not outlive the flight. A nil
// Done (context.Background) makes the child park on f.done only.
func (c *cache) wait(ctx context.Context, f *flight) ([]byte, error) {
	if ctx.Err() != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.decide(ctx, f)
	}

	wake := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			close(wake)
		case <-f.done:
		}
	}()

	select {
	case <-f.done:
	case <-wake:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.decide(ctx, f)
}

// decide is the only rule-7/8 result function for an in-flight key. It does
// not touch the LRU: recency is written only on the hit path and in admit.
func (c *cache) decide(ctx context.Context, f *flight) ([]byte, error) {
	if c.closed || f.state == flightDiscard {
		return nil, ErrClosed
	}
	if f.state == flightOK {
		return clone(f.val), nil
	}
	if f.state == flightErr {
		return nil, f.err
	}
	return nil, ctx.Err()
}

// Len is the number of resident entries.
func (c *cache) Len() int {
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	return n
}

// Close cancels in-flight loads, wakes current waiters with ErrClosed, drops
// resident entries, and joins every Loader call. Idempotent.
//
// Close must not be called from a Loader running against this cache. loads.Wait
// includes the runLoad goroutine that is still inside that Loader, so the
// call would wait for itself. A Loader must also not call Get for its own
// key: that Get joins the flight whose Loader has not returned.
func (c *cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.loads.Wait()
		return nil
	}
	c.closed = true

	var toWake []*flight
	for key, f := range c.inflight {
		delete(c.inflight, key)
		if f.state == flightPending {
			f.state = flightDiscard
			f.val, f.err = nil, nil
			toWake = append(toWake, f)
		}
	}
	c.entries = make(map[string]*entry)
	c.lru.Init()
	c.mu.Unlock()

	c.cancel()
	for _, f := range toWake {
		close(f.done)
	}
	c.loads.Wait()
	return nil
}

// clone returns a distinct non-nil slice. A nil or empty payload becomes a
// non-nil empty slice, so a successful empty load is distinguishable from an
// error return and a later hit does not hand back nil. append of zero elements
// onto a nil slice yields nil, so the empty case cannot use that form.
func clone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
