package blockcache

import (
	"context"
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

// ErrClosed is returned by Get after Close. Wrapping is legal; errors.Is is the contract.
var ErrClosed = errClosed

// errClosed is the close sentinel. Callers use ErrClosed and errors.Is.
var errClosed = &sentinel{text: "blockcache: closed"}

// loadFailed is published when a Loader panics or the admit section panics.
// It is not exported. Waiters observe a non-nil error that is not ErrClosed.
// A later Get reloads. The text is not a contract.
var loadFailed = &sentinel{text: "blockcache: load failed"}

// sentinel is an error value. Only ErrClosed is part of the public contract.
type sentinel struct{ text string }

func (e *sentinel) Error() string { return e.text }

// entry is one resident block. val is owned by the cache and never handed out.
// prev points toward the LRU end; next points toward the MRU end.
type entry struct {
	key  string
	val  []byte
	prev *entry
	next *entry
}

// flight is one in-flight Loader call for a key. It is not an entry and does not
// count toward capacity until run admits it. After done is closed, val and err
// are immutable.
type flight struct {
	key  string
	done chan struct{}
	val  []byte
	err  error
}

// cache is the unexported implementation of Cache.
type cache struct {
	capacity int
	load     Loader

	mu       sync.Mutex
	resident map[string]*entry // len(resident) <= capacity, always
	head     *entry            // MRU end; nil if empty
	tail     *entry            // LRU end; nil if empty
	inflight map[string]*flight

	// root is cancelled by Close and is the parent of every Loader ctx.
	// cancel is called once, by the Close that sets closed, after mu is released.
	root   context.Context
	cancel context.CancelFunc

	// closed is set under mu before started is closed, and is never cleared.
	closed bool
	// started is closed once, under mu, in the same section that sets closed.
	// Waiters select on it so they wake without waiting for any Loader to return.
	started chan struct{}
	// joined is closed once, by the unmarked Close that finishes the join, after
	// loads.Wait and the resident drop. A second unmarked Close waits on it.
	// It is not the Get waiter wake signal.
	joined chan struct{}

	// loads counts in-flight Loader goroutines. Add(1) happens under mu before the
	// goroutine starts; Done runs in run's defer, after publish.
	loads sync.WaitGroup

	// cancelOnce guards c.cancel. The owner and a marked re-entrant Close may
	// both reach it; only the first runs it. cancelDone is closed by that
	// first call so the other can proceed to loads.Wait or return.
	cancelOnce sync.Once
	cancelDone chan struct{}

	// joinOnce guards the resident drop and the close of joined. A marked
	// Close may win beginClose and return without joining. The next unmarked
	// Close must still be the one that waits for Loaders and drops entries.
	joinOnce sync.Once
}

// New returns a Cache holding at most capacity entries. It panics if capacity < 1
// or load == nil.
func New(capacity int, load Loader) Cache {
	if capacity < 1 {
		panic("blockcache: capacity < 1")
	}
	if load == nil {
		panic("blockcache: nil loader")
	}
	root, cancel := context.WithCancel(context.Background())
	return &cache{
		capacity:   capacity,
		load:       load,
		resident:   make(map[string]*entry),
		inflight:   make(map[string]*flight),
		root:       root,
		cancel:     cancel,
		started:    make(chan struct{}),
		joined:     make(chan struct{}),
		cancelDone: make(chan struct{}),
	}
}

// copyBytes returns an independent copy of src. A nil src yields nil. A non-nil
// empty src yields a non-nil empty slice. The two stay distinguishable.
func copyBytes(src []byte) []byte {
	if src == nil {
		return nil
	}
	return append([]byte{}, src...)
}

// linkMRU inserts e at the MRU end. Caller holds mu. e must not already be linked.
func (c *cache) linkMRU(e *entry) {
	if c.head == nil {
		c.head = e
		c.tail = e
		return
	}
	e.prev = c.head
	e.next = nil
	c.head.next = e
	c.head = e
}

// unlink removes e from the recency list. Caller holds mu.
// Unlinking the sole node yields the empty invariant. Unlinking the tail leaves
// the new tail with prev == nil. Unlinking the head leaves the new head with next == nil.
func (c *cache) unlink(e *entry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		c.tail = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		c.head = e.prev
	}
	e.prev = nil
	e.next = nil
}

// moveToMRU marks e most-recently-used. Caller holds mu. A sole node stays head and tail.
func (c *cache) moveToMRU(e *entry) {
	if c.head == e {
		return
	}
	c.unlink(e)
	c.linkMRU(e)
}

// evictLRU removes the least-recently-used resident. Caller holds mu and the list
// is non-empty. The victim is saved before unlink so capacity 1 does not read a
// nil tail or delete by a zero key.
func (c *cache) evictLRU() {
	victim := c.tail
	c.unlink(victim)
	delete(c.resident, victim.key)
}

// Len is the number of resident entries. It never exceeds capacity.
func (c *cache) Len() int {
	c.mu.Lock()
	n := len(c.resident)
	c.mu.Unlock()
	return n
}

// Get returns a copy of the block for key, loading on a miss.
// A nil ctx is a caller bug. The panic is raised before any lock is taken and
// before a flight is started, so one bad call cannot pin mu or the join.
func (c *cache) Get(ctx context.Context, key string) ([]byte, error) {
	if ctx == nil {
		panic("blockcache: nil context")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if e := c.resident[key]; e != nil {
		c.moveToMRU(e)
		b := copyBytes(e.val)
		c.mu.Unlock()
		return b, nil
	}
	if f := c.inflight[key]; f != nil {
		c.mu.Unlock()
		return c.wait(ctx, f)
	}
	f := &flight{key: key, done: make(chan struct{})}
	c.inflight[key] = f
	c.loads.Add(1)
	c.mu.Unlock()
	go c.run(f)
	return c.wait(ctx, f)
}

// wait blocks until the flight resolves, the caller context ends, or Close begins.
// After every wake, including a wake from ctx.Done, all three conditions are
// rechecked under mu. select case order is not priority. The order is:
// ErrClosed, then a resolved load, then ctx.Err().
//
// A nil error is returned only when the flight was admitted. publish of a
// discarded success stores (nil, nil) only after Close has set closed, and the
// closed check above wins that window. An open cache never publishes (nil, nil).
func (c *cache) wait(ctx context.Context, f *flight) ([]byte, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		select {
		case <-f.done:
			b, err := flightResult(f)
			c.mu.Unlock()
			return b, err
		default:
		}
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		c.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
		case <-c.started:
		}
	}
}

// flightResult copies a published flight. Caller holds mu and has observed
// f.done closed and closed false. A nil error means the value was admitted;
// a non-nil error returns nil bytes.
func flightResult(f *flight) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return copyBytes(f.val), nil
}

// publish records the flight outcome, removes it from inflight, and closes done.
// Caller holds mu. f is still in inflight. publish does not touch mu.
// f.done is closed in exactly this place.
func (c *cache) publish(f *flight, val []byte, err error) {
	f.val = val
	f.err = err
	delete(c.inflight, f.key)
	close(f.done)
}

// run calls the Loader under a child of root, then admits or discards under mu.
// mu is never held across the Loader call. A Loader panic is recovered and
// published as a non-nil error. loads.Done always runs. A panic in the admit
// section rolls the map and list back, unlocks, and publishes a failure.
func (c *cache) run(f *flight) {
	defer c.loads.Done()

	ctx, cancel := context.WithCancel(c.root)
	defer cancel()

	b, err := c.callLoader(ctx, f.key)
	c.admit(f, b, err)
}

// loadCtx is the context passed to the Loader. It is not a *cancelCtx, so
// context.AfterFunc schedules through AfterFunc on this type. This type starts
// that func on a marked goroutine. A *cancelCtx parent would start it unmarked
// from afterFuncCtx.cancel. Deadline and Value pass through to the embedded
// cache-scoped parent. A caller's cancel does not cancel this context.
type loadCtx struct {
	context.Context
	done  chan struct{}
	err   error
	mu    sync.Mutex
	funcs map[*byte]func()
}

func newLoadCtx(parent context.Context) (*loadCtx, context.CancelFunc) {
	lc := &loadCtx{
		Context: parent,
		done:    make(chan struct{}),
		funcs:   make(map[*byte]func()),
	}
	if parent.Err() != nil {
		lc.shutdown(parent.Err())
		return lc, func() {}
	}
	// Parent cancel must reach this context. A watcher does that without
	// context.AfterFunc on parent: that registration runs on the caller's
	// goroutine, which is the flight goroutine, and would observe the reenter
	// mark. The watcher exits when either side is done, so it does not outlive
	// the load.
	go func() {
		select {
		case <-parent.Done():
			lc.shutdown(parent.Err())
		case <-lc.done:
		}
	}()
	return lc, func() { lc.shutdown(context.Canceled) }
}

func (lc *loadCtx) shutdown(err error) {
	if err == nil {
		err = context.Canceled
	}
	lc.mu.Lock()
	if lc.err != nil {
		lc.mu.Unlock()
		return
	}
	lc.err = err
	funcs := lc.funcs
	lc.funcs = nil
	close(lc.done)
	lc.mu.Unlock()
	for _, f := range funcs {
		// Run on a goroutine this cache marks before user code. context.AfterFunc
		// would otherwise start f unmarked (afterFuncCtx.cancel does go f()).
		go func(fn func()) {
			prev := markReenter()
			defer unmarkReenter(prev)
			fn()
		}(f)
	}
}

func (lc *loadCtx) Done() <-chan struct{} { return lc.done }

func (lc *loadCtx) Err() error {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.err
}

// AfterFunc is the method context.AfterFunc looks up. The returned stop
// removes fn if it has not started. The cache starts fn itself, marked, so
// Close from fn can see the reenter slot.
func (lc *loadCtx) AfterFunc(f func()) func() bool {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.err != nil {
		go func() {
			prev := markReenter()
			defer unmarkReenter(prev)
			f()
		}()
		return func() bool { return false }
	}
	k := new(byte)
	lc.funcs[k] = f
	return func() bool {
		lc.mu.Lock()
		defer lc.mu.Unlock()
		_, ok := lc.funcs[k]
		delete(lc.funcs, k)
		return ok
	}
}

// callLoader invokes the Loader and converts a panic into loadFailed.
// The recover runs before mu is acquired, so a Loader panic does not touch the lock.
func (c *cache) callLoader(ctx context.Context, key string) (b []byte, err error) {
	// Build the Loader context before marking. Its parent watcher starts on
	// this goroutine's behalf and must not observe the reenter mark.
	lctx, lcancel := newLoadCtx(ctx)
	defer lcancel()

	prev := markReenter()
	defer unmarkReenter(prev)
	defer func() {
		if rec := recover(); rec != nil {
			b = nil
			err = loadFailed
		}
	}()
	return c.load(lctx, key)
}

// admit stores a successful load or publishes a failure. Caller does not hold mu.
// The map write and the list link are one step. If a later statement panics,
// the deferred recover deletes the key and rebuilds the list before unlocking,
// so a partial insert cannot leave resident[key] set with a nil tail.
func (c *cache) admit(f *flight, b []byte, err error) {
	c.mu.Lock()
	published := false
	defer func() {
		if rec := recover(); rec != nil && !published {
			c.rollback(f.key)
			c.failPublish(f, loadFailed)
		}
		c.mu.Unlock()
	}()

	// Admission is refused once Close has begun or the Loader failed. closed is
	// set before cancel, so a Loader that returns after cancel sees closed.
	// root.Err() is the belt for the same window. A discarded success publishes
	// (nil, nil) only while closed is set; waiters observe closed first and
	// return ErrClosed. An open cache never publishes a nil error for a value
	// that was not stored.
	if c.closed || c.root.Err() != nil || err != nil {
		c.publish(f, nil, err)
		published = true
		return
	}
	if len(c.resident) == c.capacity {
		c.evictLRU()
	}
	stored := copyBytes(b)
	e := &entry{key: f.key, val: stored}
	c.insert(e)
	c.publish(f, stored, nil)
	published = true
}

// insert adds e to resident and links it at the MRU end. Caller holds mu.
func (c *cache) insert(e *entry) {
	c.resident[e.key] = e
	c.linkMRU(e)
}

// rollback removes key and rebuilds the recency list from the surviving map.
// Caller holds mu. A panic between the map write and linkMRU leaves the new
// node absent from the chain; rebuilding drops it and restores head and tail.
func (c *cache) rollback(key string) {
	delete(c.resident, key)
	c.head = nil
	c.tail = nil
	if len(c.resident) == 0 {
		return
	}
	nodes := make([]*entry, 0, len(c.resident))
	for _, e := range c.resident {
		e.prev = nil
		e.next = nil
		nodes = append(nodes, e)
	}
	for _, e := range nodes {
		c.linkMRU(e)
	}
}

// failPublish closes the flight with a non-nil error if publish has not run.
// Caller holds mu. A second call is a no-op because done is already closed.
func (c *cache) failPublish(f *flight, err error) {
	select {
	case <-f.done:
		return
	default:
	}
	if err == nil {
		err = loadFailed
	}
	c.publish(f, nil, err)
}

// Close cancels in-flight loads, waits for their Loader calls to return, wakes
// every waiter with ErrClosed, drops all entries, and returns nil. Idempotent.
//
// cancel runs after mu is released so a Done callback may call Get or Len.
//
// A second Close waits on joined, which the owner closes after loads.Wait and
// the resident drop. That wait deadlocks when this call is what a Loader is
// blocked on. Two cases:
//
//   - The Loader called Close on its own goroutine. callLoader marks that
//     goroutine before the call. Close sees the mark and returns without
//     waiting. The mark is a goroutine-local slot, not a parsed stack trace.
//
//   - context.AfterFunc ran the callback on a new goroutine. The Loader
//     context is a loadCtx, so that callback is started by loadCtx and marked
//     before the user func runs. Close on that goroutine returns without
//     waiting. An ordinary overlapping Close is unmarked and waits for the join.
func (c *cache) Close() error {
	if reenterMarked() {
		return c.closeFromCallback()
	}
	return c.closeJoin()
}

// closeFromCallback runs Close on the Loader goroutine or on a Loader-context
// AfterFunc goroutine. It does not wait on joined. It still performs the
// close transition and cancel if no other call has, so a Loader blocked in
// this Close is not also blocked on ctx.Done that nobody has signaled.
// The unmarked Close that is in loads.Wait, or that arrives next, completes
// the resident drop after this goroutine returns to the Loader.
func (c *cache) closeFromCallback() error {
	c.beginClose()
	c.doCancel()
	return nil
}

// closeJoin is Close from a goroutine that is not inside a Loader or a
// Loader-context callback. One such call joins. Others wait for that join.
// The joiner is not necessarily the call that won beginClose: a marked Close
// may have won that transition and returned without waiting for Loaders.
func (c *cache) closeJoin() error {
	c.beginClose()
	c.doCancel()

	joinedHere := false
	c.joinOnce.Do(func() {
		joinedHere = true
		c.loads.Wait()
		c.mu.Lock()
		c.resident = make(map[string]*entry)
		c.head = nil
		c.tail = nil
		close(c.joined)
		c.mu.Unlock()
	})
	if !joinedHere {
		<-c.joined
	}
	return nil
}

// beginClose sets closed and wakes Get waiters. It reports whether this call
// won that transition. The loser has already observed closed.
func (c *cache) beginClose() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.closed = true
	close(c.started)
	return true
}

// doCancel runs the cache cancel once, on a fresh goroutine. cancel invokes
// context.AfterFunc callbacks synchronously on the caller before it returns.
// Running it on the flight goroutine would clear that goroutine's reenter
// mark for the rest of the Loader call, and a later Close from the Loader
// would wait on joined. The helper is unmarked. Callers wait until cancel
// has returned so a Loader blocked on ctx.Done is released before a marked
// Close returns to it.
func (c *cache) doCancel() {
	c.cancelOnce.Do(func() {
		go func() {
			c.cancel()
			close(c.cancelDone)
		}()
	})
	<-c.cancelDone
}
