package blockcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

var errBoom = errors.New("boom")

func newInternal(t *testing.T, capacity int, load Loader) *cache {
	t.Helper()
	c := New(capacity, load).(*cache)
	t.Cleanup(func() { c.Close() })
	return c
}

// 1. Per-waiter copies.
func TestPerWaiterCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		release := make(chan struct{})
		c := newInternal(t, 2, func(context.Context, string) ([]byte, error) {
			<-release
			return []byte("data"), nil
		})
		const n = 5
		res := make([][]byte, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() { defer wg.Done(); res[i], errs[i] = c.Get(ctx, "k") }()
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("Get[%d]: %v", i, errs[i])
			}
		}
		res[0][0] = 'X'
		for i := 1; i < n; i++ {
			if string(res[i]) != "data" {
				t.Errorf("Get[%d] = %q after mutating Get[0]", i, res[i])
			}
		}
		if b, err := c.Get(ctx, "k"); err != nil || string(b) != "data" {
			t.Errorf("hit = %q, %v", b, err)
		}
	})
}

// 2. Loader slice not retained.
func TestLoaderSliceNotRetained(t *testing.T) {
	ctx := testCtx(t)
	var mu sync.Mutex
	var buf []byte
	c := newInternal(t, 2, func(context.Context, string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		buf = []byte("orig")
		return buf, nil
	})
	if b := mustGet(t, ctx, c, "k"); string(b) != "orig" {
		t.Fatalf("got %q", b)
	}
	mu.Lock()
	copy(buf, "XXXX")
	mu.Unlock()
	if b := mustGet(t, ctx, c, "k"); string(b) != "orig" {
		t.Fatalf("hit = %q, want orig", b)
	}
}

// 3. Error with bytes.
func TestErrorWithBytes(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := newInternal(t, 2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("junk"), errBoom
	})
	b, err := c.Get(ctx, "k")
	if !errors.Is(err, errBoom) || b != nil {
		t.Fatalf("got %q, %v; want nil, boom", b, err)
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
	c.Get(ctx, "k")
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

// 4. Error fan-out.
func TestErrorFanOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		var calls atomic.Int32
		release := make(chan struct{})
		c := newInternal(t, 2, func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return nil, errBoom
		})
		const n = 10
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() { defer wg.Done(); _, errs[i] = c.Get(ctx, "k") }()
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, errBoom) {
				t.Errorf("Get[%d] err = %v", i, err)
			}
		}
		if calls.Load() != 1 {
			t.Errorf("calls = %d, want 1", calls.Load())
		}
	})
}

// 5. LRU and in-flight not counted.
func TestInflightNotCounted(t *testing.T) {
	ctx := testCtx(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	var aCalls atomic.Int32
	c := newInternal(t, 1, func(_ context.Context, key string) ([]byte, error) {
		if key == "b" {
			close(entered)
			<-release
		} else {
			aCalls.Add(1)
		}
		return []byte(key), nil
	})
	mustGet(t, ctx, c, "a")
	done := make(chan error, 1)
	go func() { _, err := c.Get(ctx, "b"); done <- err }()
	<-entered
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	mustGet(t, ctx, c, "a")
	if aCalls.Load() != 1 {
		t.Fatalf("a reloaded while b in flight")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mustGet(t, ctx, c, "a")
	if aCalls.Load() != 2 {
		t.Fatalf("a not evicted by b (calls=%d)", aCalls.Load())
	}
}

// 6. Len under concurrent poll.
func TestLenConcurrentPoll(t *testing.T) {
	ctx := testCtx(t)
	const capacity = 3
	c := newInternal(t, capacity, func(_ context.Context, key string) ([]byte, error) {
		return []byte(key), nil
	})
	stop := make(chan struct{})
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if l := c.Len(); l > capacity {
				t.Errorf("Len = %d > %d", l, capacity)
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() { defer wg.Done(); c.Get(ctx, fmt.Sprint(i%40)) }()
	}
	wg.Wait()
	close(stop)
	<-pollDone
}

// 7. Orphaned load populates the cache.
func TestOrphanPopulates(t *testing.T) {
	ctx := testCtx(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int32
	c := newInternal(t, 2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		close(entered)
		<-release
		return []byte("v"), nil
	})
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := c.Get(cctx, "k"); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for c.Len() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b := mustGet(t, ctx, c, "k"); string(b) != "v" || calls.Load() != 1 {
		t.Fatalf("got %q, calls=%d", b, calls.Load())
	}
}

// 8. ctx priority: cancelled waiter leaves, other waiter gets bytes.
func TestCtxPriority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		release := make(chan struct{})
		c := newInternal(t, 2, func(context.Context, string) ([]byte, error) {
			<-release
			return []byte("v"), nil
		})
		cctx, ccancel := context.WithCancel(ctx)
		var err1, err2 error
		var b2 []byte
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, err1 = c.Get(cctx, "k") }()
		go func() { defer wg.Done(); b2, err2 = c.Get(ctx, "k") }()
		synctest.Wait()
		ccancel()
		synctest.Wait()
		if !errors.Is(err1, context.Canceled) {
			t.Errorf("err1 = %v", err1)
		}
		close(release)
		wg.Wait()
		if err2 != nil || string(b2) != "v" {
			t.Errorf("waiter2 = %q, %v", b2, err2)
		}
	})
}

// gatedCtx wraps a cancellable context. The first Err call made after the
// context is Done signals reached and then blocks until gate is closed. This
// pauses a waiter after it has woken but before it snapshots the cache state,
// so a test can resolve, cancel, or Close inside that window. Err calls made
// while the context is not yet Done (Get's lookup runs one under mu) never block.
type gatedCtx struct {
	context.Context
	once    sync.Once
	reached chan struct{}
	gate    chan struct{}
}

func newGatedCtx(parent context.Context) (*gatedCtx, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	return &gatedCtx{Context: ctx, reached: make(chan struct{}), gate: make(chan struct{})}, cancel
}

func (g *gatedCtx) Err() error {
	if isDone(g.Context.Done()) {
		g.once.Do(func() {
			close(g.reached)
			<-g.gate
		})
	}
	return g.Context.Err()
}

// windowTest starts a Get for "k" under a gatedCtx, waits until the Loader has
// entered, cancels the caller's ctx, and waits until the waiter is paused in
// the window. It returns the in-flight call, the gate, and the Get's result.
func windowTest(t *testing.T, c *cache, entered <-chan struct{}) (cl *call, gate chan struct{}, res <-chan getResult) {
	t.Helper()
	g, cancel := newGatedCtx(testCtx(t))
	t.Cleanup(cancel)
	out := make(chan getResult, 1)
	go func() {
		b, err := c.Get(g, "k")
		out <- getResult{b, err}
	}()
	<-entered
	c.mu.Lock()
	cl = c.inflight["k"]
	c.mu.Unlock()
	if cl == nil {
		t.Fatal("no in-flight call for k")
	}
	cancel()
	<-g.reached
	return cl, g.gate, out
}

type getResult struct {
	b   []byte
	err error
}

// 8b. Resolution in the window while ctx is already Done: the result wins.
func TestResolutionInWindow(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("fail=", fail), func(t *testing.T) {
			release := make(chan struct{})
			entered := make(chan struct{})
			c := newInternal(t, 2, func(context.Context, string) ([]byte, error) {
				close(entered)
				<-release
				if fail {
					return nil, errBoom
				}
				return []byte("v"), nil
			})
			cl, gate, res := windowTest(t, c, entered)
			close(release)
			<-cl.done
			close(gate)
			r := <-res
			if fail {
				if !errors.Is(r.err, errBoom) {
					t.Errorf("err = %v, want boom", r.err)
				}
			} else if r.err != nil || string(r.b) != "v" {
				t.Errorf("got %q, %v; want v", r.b, r.err)
			}
		})
	}
}

// 8c. Cancellation observed while the load is still in flight: ctx.Err(), and
// the orphaned load still populates the cache.
func TestCancelStillInFlight(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int32
	c := newInternal(t, 2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		close(entered)
		<-release
		return []byte("v"), nil
	})
	cl, gate, res := windowTest(t, c, entered)
	close(gate)
	if r := <-res; !errors.Is(r.err, context.Canceled) || r.b != nil {
		t.Fatalf("got %q, %v; want nil, Canceled", r.b, r.err)
	}
	if isDone(cl.done) {
		t.Fatal("call resolved before release; window not exercised")
	}
	close(release)
	<-cl.done
	if b := mustGet(t, testCtx(t), c, "k"); string(b) != "v" || calls.Load() != 1 {
		t.Fatalf("got %q, calls=%d", b, calls.Load())
	}
}

// 8g. Close starts while the waiter is in the window (its ctx.Err call is in
// progress) and the load is still unresolved: ErrClosed, not ctx.Err().
func TestCloseInWindowLoadUnresolved(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	c := newInternal(t, 2, func(context.Context, string) ([]byte, error) {
		close(entered)
		<-release // ignores ctx so the load stays in flight and Close stays joining
		return []byte("v"), nil
	})
	cl, gate, res := windowTest(t, c, entered)
	closeDone := make(chan struct{})
	go func() { defer close(closeDone); c.Close() }()
	<-c.closing
	close(gate)
	if r := <-res; !errors.Is(r.err, ErrClosed) || r.b != nil {
		t.Errorf("got %q, %v; want nil, ErrClosed", r.b, r.err)
	}
	if isDone(cl.done) {
		t.Error("load resolved before release; window not exercised")
	}
	close(release)
	<-closeDone
	if c.Len() != 0 {
		t.Errorf("Len = %d", c.Len())
	}
}

// 8e. Close runs to completion while the waiter is in the window. Whether the
// call resolved successfully before Close (and was admitted, then dropped) or
// resolved after Close's cancel (success or error, discarded), the waiter must
// get ErrClosed: it cannot hand out a block once Close has returned.
func TestCloseCompletesInWindow(t *testing.T) {
	cases := []struct {
		name          string
		resolveBefore bool // release the Loader and let it resolve before Close
		fail          bool
	}{
		{"resolved-before-close", true, false},
		{"resolved-after-cancel", false, false},
		{"failed-after-cancel", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			entered := make(chan struct{})
			var calls atomic.Int32
			c := newInternal(t, 2, func(lctx context.Context, _ string) ([]byte, error) {
				calls.Add(1)
				close(entered)
				select {
				case <-release:
				case <-lctx.Done():
				}
				if tc.fail {
					return nil, errBoom
				}
				return []byte("payload"), nil
			})
			cl, gate, res := windowTest(t, c, entered)
			if tc.resolveBefore {
				close(release)
				<-cl.done
				if c.Len() != 1 {
					t.Fatalf("Len = %d before Close, want 1", c.Len())
				}
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			<-cl.done
			close(gate)
			if r := <-res; !errors.Is(r.err, ErrClosed) || r.b != nil {
				t.Fatalf("got %q, %v; want nil, ErrClosed", r.b, r.err)
			}
			if c.Len() != 0 {
				t.Fatalf("Len = %d", c.Len())
			}
			if _, err := c.Get(testCtx(t), "k"); !errors.Is(err, ErrClosed) {
				t.Fatalf("later Get err = %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls = %d", calls.Load())
			}
		})
	}
}

// wait on a closed cache answers ErrClosed even when the call has resolved
// successfully and the caller's ctx is Done.
func TestWaitClosedBeatsResolved(t *testing.T) {
	c := New(1, func(context.Context, string) ([]byte, error) { return nil, nil }).(*cache)
	c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok := &call{done: make(chan struct{}), val: []byte("v")}
	close(ok.done)
	for _, wctx := range []context.Context{ctx, context.Background()} {
		if b, err := c.wait(wctx, ok); !errors.Is(err, ErrClosed) || b != nil {
			t.Fatalf("got %q, %v; want nil, ErrClosed", b, err)
		}
	}
}

// 8d. Both first-select cases ready: result always wins.
func TestPreResolvedPreCancelled(t *testing.T) {
	c := newInternal(t, 1, func(context.Context, string) ([]byte, error) { return nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok := &call{done: make(chan struct{}), val: []byte("v")}
	close(ok.done)
	bad := &call{done: make(chan struct{}), err: errBoom}
	close(bad.done)
	for i := range 1000 {
		if b, err := c.wait(ctx, ok); err != nil || string(b) != "v" {
			t.Fatalf("iter %d: got %q, %v", i, b, err)
		}
		if _, err := c.wait(ctx, bad); !errors.Is(err, errBoom) {
			t.Fatalf("iter %d: err = %v", i, err)
		}
	}
}

// 8f. Hits racing Close.
func TestHitDuringClose(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := newInternal(t, 2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("v"), nil
	})
	mustGet(t, ctx, c, "k")
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := c.Get(ctx, "k")
			if err == nil && string(b) != "v" || err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("got %q, %v", b, err)
			}
		}()
	}
	c.Close()
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

// 9. Close wakes waiters; the Loader sees ctx.Done.
func TestCloseWakesWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		var sawDone atomic.Bool
		c := New(2, func(lctx context.Context, _ string) ([]byte, error) {
			<-lctx.Done()
			sawDone.Store(true)
			return nil, lctx.Err()
		})
		const n = 5
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() { defer wg.Done(); _, errs[i] = c.Get(ctx, "k") }()
		}
		synctest.Wait()
		if err := c.Close(); err != nil {
			t.Errorf("Close = %v", err)
		}
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, ErrClosed) {
				t.Errorf("Get[%d] = %v", i, err)
			}
		}
		if !sawDone.Load() {
			t.Error("Loader did not see ctx.Done")
		}
	})
}

// 10. Close waits for the Loader. 12. Concurrent Close waits for the same join.
func TestCloseWaitsForLoader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		signal := make(chan struct{})
		var returned atomic.Bool
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-signal // ignores ctx
			returned.Store(true)
			return []byte("v"), nil
		})
		go c.Get(ctx, "k")
		synctest.Wait()
		var closed1, closed2 atomic.Bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if c.Close() != nil {
				t.Error("Close1 non-nil")
			}
			if !returned.Load() {
				t.Error("Close1 returned before Loader")
			}
			closed1.Store(true)
		}()
		go func() {
			defer wg.Done()
			if c.Close() != nil {
				t.Error("Close2 non-nil")
			}
			if !returned.Load() {
				t.Error("Close2 returned before Loader")
			}
			closed2.Store(true)
		}()
		synctest.Wait()
		if closed1.Load() || closed2.Load() {
			t.Error("Close returned while Loader still running")
		}
		close(signal)
		wg.Wait()
		if c.Len() != 0 {
			t.Errorf("Len = %d", c.Len())
		}
	})
}

// 11. Zero-waiter result discarded after Close.
func TestZeroWaiterDiscarded(t *testing.T) {
	ctx := testCtx(t)
	entered := make(chan struct{})
	c := New(2, func(lctx context.Context, _ string) ([]byte, error) {
		close(entered)
		<-lctx.Done()
		return []byte("v"), nil
	}).(*cache)
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := c.Get(cctx, "k"); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	c.Close()
	if c.Len() != 0 {
		t.Fatalf("Len = %d", c.Len())
	}
	c.mu.Lock()
	n := len(c.items) + len(c.inflight)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("maps not empty: %d", n)
	}
	if _, err := c.Get(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close = %v", err)
	}
}

// 13. Get after Close does not call the Loader.
func TestGetAfterCloseNoLoad(t *testing.T) {
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return nil, nil
	})
	c.Close()
	if _, err := c.Get(testCtx(t), "k"); err != ErrClosed {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

// 14. Pre-cancelled ctx.
func TestPreCancelledCtx(t *testing.T) {
	var calls atomic.Int32
	c := newInternal(t, 2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("v"), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "k"); !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("miss: err=%v calls=%d", err, calls.Load())
	}
	mustGet(t, testCtx(t), c, "k")
	if b, err := c.Get(ctx, "k"); err != nil || string(b) != "v" {
		t.Fatalf("hit: %q, %v", b, err)
	}
}

type evilStringer struct{}

func (evilStringer) String() string { panic("String called") }

// 15. Loader panic.
func TestLoaderPanic(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		if calls.Add(1) == 1 {
			panic(evilStringer{})
		}
		return []byte("v"), nil
	})
	_, err := c.Get(ctx, "k")
	if err == nil {
		t.Fatal("want error")
	}
	if msg := err.Error(); !strings.Contains(msg, "evilStringer") {
		t.Fatalf("msg = %q", msg)
	}
	if b := mustGet(t, ctx, c, "k"); string(b) != "v" {
		t.Fatalf("reload = %q", b)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// 16. Empty success.
func TestEmptySuccess(t *testing.T) {
	c := newInternal(t, 2, func(context.Context, string) ([]byte, error) { return nil, nil })
	for range 2 { // miss, then hit
		b, err := c.Get(testCtx(t), "k")
		if err != nil || b == nil || len(b) != 0 {
			t.Fatalf("got %v (nil=%v), %v", b, b == nil, err)
		}
	}
}

// 17. Stress.
func TestStress(t *testing.T) {
	ctx := testCtx(t)
	c := New(4, func(lctx context.Context, key string) ([]byte, error) {
		select {
		case <-lctx.Done():
			return nil, lctx.Err()
		case <-time.After(time.Duration(rand.Intn(200)) * time.Microsecond):
		}
		if rand.Intn(10) == 0 {
			return nil, errBoom
		}
		return []byte(key), nil
	})
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for range 300 {
				key := fmt.Sprint(r.Intn(10))
				gctx := ctx
				var cancel context.CancelFunc = func() {}
				if r.Intn(5) == 0 {
					gctx, cancel = context.WithTimeout(ctx, time.Duration(r.Intn(100))*time.Microsecond)
				}
				b, err := c.Get(gctx, key)
				cancel()
				switch {
				case err == nil:
					if !bytes.Equal(b, []byte(key)) {
						t.Errorf("Get(%s) = %q", key, b)
					}
				case errors.Is(err, ErrClosed), errors.Is(err, errBoom),
					errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				default:
					t.Errorf("unexpected err %v", err)
				}
				if c.Len() > 4 {
					t.Errorf("Len > capacity")
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if c.Len() != 0 {
		t.Fatalf("Len after Close = %d", c.Len())
	}
	if ctx.Err() != nil {
		t.Fatal("deadline hit")
	}
}

// Nil caller ctx (unsupported) must not leave mu locked when it panics.
func TestNilCtxDoesNotLeakLock(t *testing.T) {
	c := New(2, func(context.Context, string) ([]byte, error) { return []byte("v"), nil })
	func() {
		defer func() { recover() }()
		var nilCtx context.Context
		c.Get(nilCtx, "missing")
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Len()
		_ = c.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("mu left locked after nil-ctx panic")
	}
}

// flakyCtx reports Done immediately but Err only from the second call on,
// to exercise wait's loop instead of returning (nil, nil).
type flakyCtx struct {
	context.Context
	done  chan struct{}
	calls atomic.Int32
}

func (f *flakyCtx) Done() <-chan struct{} { return f.done }
func (f *flakyCtx) Err() error {
	if f.calls.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

func TestWaitNeverReturnsNilNil(t *testing.T) {
	c := newInternal(t, 1, func(context.Context, string) ([]byte, error) { return nil, nil })
	f := &flakyCtx{Context: context.Background(), done: make(chan struct{})}
	close(f.done)
	cl := &call{done: make(chan struct{})} // never resolves
	b, err := c.wait(f, cl)
	if !errors.Is(err, context.Canceled) || b != nil {
		t.Fatalf("got %v, %v; want nil, Canceled", b, err)
	}
}

// A Loader that calls runtime.Goexit still resolves its call.
func TestLoaderGoexit(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		if calls.Add(1) == 1 {
			runtime.Goexit()
		}
		return []byte("v"), nil
	}).(*cache)
	if _, err := c.Get(ctx, "k"); !errors.Is(err, errLoaderExited) {
		t.Fatalf("err = %v, want errLoaderExited", err)
	}
	if b := mustGet(t, ctx, c, "k"); string(b) != "v" || calls.Load() != 2 {
		t.Fatalf("reload = %q, calls=%d", b, calls.Load())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
