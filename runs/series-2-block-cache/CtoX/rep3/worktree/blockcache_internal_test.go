package blockcache

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestErrorWinsOverBytes(t *testing.T) {
	ctx := testCtx(t)
	boom := errors.New("boom")
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("x"), boom
	})
	t.Cleanup(func() { c.Close() })

	b, err := c.Get(ctx, "a")
	if !errors.Is(err, boom) || b != nil {
		t.Fatalf("Get = (%q, %v), want (nil, boom)", b, err)
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0", n)
	}
	c.Get(ctx, "a")
	if n := calls.Load(); n != 2 {
		t.Fatalf("Loader calls = %d, want 2", n)
	}
}

func TestSingleFlightError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("boom")
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(2, func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return nil, boom
		})
		defer c.Close()

		const n = 10
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { _, errs[i] = c.Get(t.Context(), "k") })
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, boom) {
				t.Errorf("waiter %d: err = %v, want boom", i, err)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("Loader calls = %d, want 1", got)
		}
	})
}

func TestPerWaiterCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var loaderBuf []byte
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-release
			loaderBuf = []byte("data")
			return loaderBuf, nil
		})
		defer c.Close()

		var r1, r2 []byte
		var wg sync.WaitGroup
		wg.Go(func() { r1, _ = c.Get(t.Context(), "k") })
		wg.Go(func() { r2, _ = c.Get(t.Context(), "k") })
		synctest.Wait()
		close(release)
		wg.Wait()

		r1[0] = 'X'
		if string(r2) != "data" {
			t.Fatalf("r2 = %q, mutation of r1 leaked", r2)
		}
		loaderBuf[0] = 'Y' // Loader mutates its own slice after return
		r3, err := c.Get(t.Context(), "k")
		if err != nil || string(r3) != "data" {
			t.Fatalf("hit = (%q, %v), want data", r3, err)
		}
	})
}

func TestLoaderGetsCacheCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gotCtx := make(chan context.Context, 1)
		c := New(2, func(ctx context.Context, _ string) ([]byte, error) {
			gotCtx <- ctx
			<-ctx.Done()
			return nil, ctx.Err()
		})
		callerCtx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := c.Get(callerCtx, "k"); done <- err }()
		lctx := <-gotCtx
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Get err = %v, want context.Canceled", err)
		}
		synctest.Wait()
		if lctx.Err() != nil {
			t.Errorf("Loader ctx cancelled by caller cancel")
		}
		c.Close()
		if lctx.Err() == nil {
			t.Errorf("Loader ctx not cancelled by Close")
		}
	})
}

func TestCallerCancelOrphanedLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(2, func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		})
		defer c.Close()

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := c.Get(ctx, "k"); done <- err }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Get err = %v, want context.Canceled", err)
		}
		close(release)
		synctest.Wait()
		if n := c.Len(); n != 1 {
			t.Fatalf("Len = %d, want 1 (orphaned load must populate)", n)
		}
		b, err := c.Get(t.Context(), "k")
		if err != nil || string(b) != "v" || calls.Load() != 1 {
			t.Fatalf("Get = (%q, %v), calls %d; want hit", b, err, calls.Load())
		}
	})
}

func TestClosePriorityOverCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := New(2, func(ctx context.Context, _ string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := c.Get(ctx, "k"); done <- err }()
		synctest.Wait()
		c.Close()
		cancel()
		if err := <-done; !errors.Is(err, ErrClosed) {
			t.Fatalf("Get err = %v, want ErrClosed", err)
		}
	})
}

func TestCloseWakesWaitersAndJoinsLoader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-release // deliberately ignores ctx for a while
			return []byte("v"), nil
		})
		const n = 5
		errs := make(chan error, n)
		for range n {
			go func() { _, err := c.Get(t.Context(), "k"); errs <- err }()
		}
		synctest.Wait()

		var closed1, closed2 atomic.Bool
		go func() { c.Close(); closed1.Store(true) }()
		synctest.Wait()
		go func() { c.Close(); closed2.Store(true) }()
		synctest.Wait()

		for range n {
			if err := <-errs; !errors.Is(err, ErrClosed) {
				t.Errorf("waiter err = %v, want ErrClosed", err)
			}
		}
		if closed1.Load() || closed2.Load() {
			t.Errorf("Close returned before Loader joined")
		}
		close(release)
		synctest.Wait()
		if !closed1.Load() || !closed2.Load() {
			t.Errorf("Close did not return after Loader joined")
		}
		if l := c.Len(); l != 0 {
			t.Errorf("Len = %d after Close, want 0", l)
		}
	})
}

func TestGetAfterCloseNoLoad(t *testing.T) {
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return nil, nil
	})
	c.Close()
	if _, err := c.Get(testCtx(t), "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("Loader calls = %d, want 0", n)
	}
}

func TestInflightNotCountedNotEvicted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := New(1, func(_ context.Context, key string) ([]byte, error) {
			if key == "b" {
				<-release
			}
			return []byte(key), nil
		}).(*cache)
		defer c.Close()

		if _, err := c.Get(t.Context(), "a"); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { c.Get(t.Context(), "b"); close(done) }()
		synctest.Wait()
		if l := c.Len(); l != 1 {
			t.Errorf("Len = %d with b in flight, want 1", l)
		}
		if b, err := c.Get(t.Context(), "a"); err != nil || string(b) != "a" {
			t.Errorf("hit a = (%q, %v)", b, err)
		}
		close(release)
		<-done
		c.mu.Lock()
		_, hasA := c.entries["a"]
		_, hasB := c.entries["b"]
		c.mu.Unlock()
		if hasA || !hasB {
			t.Errorf("resident a=%v b=%v, want only b", hasA, hasB)
		}
	})
}

func TestLenNeverAboveCapacityUnderPoll(t *testing.T) {
	ctx := testCtx(t)
	const capacity = 4
	c := New(capacity, func(_ context.Context, key string) ([]byte, error) {
		return []byte(key), nil
	}).(*cache)
	t.Cleanup(func() { c.Close() })

	stop := make(chan struct{})
	var poll sync.WaitGroup
	poll.Go(func() {
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
	})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 200 {
				c.Get(ctx, fmt.Sprintf("k%d-%d", g, i%20))
			}
		})
	}
	wg.Wait()
	close(stop)
	poll.Wait()
}

func TestConcurrentStress(t *testing.T) {
	ctx := testCtx(t)
	boom := errors.New("boom")
	c := New(8, func(lctx context.Context, key string) ([]byte, error) {
		select {
		case <-time.After(time.Duration(rand.Intn(200)) * time.Microsecond):
		case <-lctx.Done():
			return nil, lctx.Err()
		}
		if rand.Intn(5) == 0 {
			return nil, boom
		}
		return []byte("v:" + key), nil
	})

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 300 {
				key := fmt.Sprintf("k%d", rand.Intn(32))
				gctx, cancel := context.WithTimeout(ctx, time.Duration(rand.Intn(300))*time.Microsecond)
				b, err := c.Get(gctx, key)
				cancel()
				switch {
				case err == nil:
					if string(b) != "v:"+key {
						t.Errorf("Get(%s) = %q", key, b)
					}
				case errors.Is(err, boom), errors.Is(err, ErrClosed),
					errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
				default:
					t.Errorf("unexpected err %v", err)
				}
				if c.Len() > 8 {
					t.Errorf("Len above capacity")
				}
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	closeErr := make(chan error, 1)
	go func() { closeErr <- c.Close() }()
	select {
	case err := <-closeErr:
		if err != nil {
			t.Fatalf("Close = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Close deadlocked")
	}
	wg.Wait()
	if l := c.Len(); l != 0 {
		t.Fatalf("Len after Close = %d", l)
	}
}

// lagCtx models a context that has been cancelled (Err != nil) but whose waiter
// has not yet been scheduled to observe Done: Done stays open until closed.
type lagCtx struct {
	context.Context
	cancelled atomic.Bool
	done      chan struct{}
}

func (c *lagCtx) Done() <-chan struct{} { return c.done }
func (c *lagCtx) Err() error {
	if c.cancelled.Load() {
		return context.Canceled
	}
	return nil
}

// TestCancelInFlightBeatsLaterResolution: a waiter whose ctx became Done while
// the load was in flight gets ctx.Err() even if the load resolves before the
// waiter runs its post-select checks (rule 7b).
func TestCancelInFlightBeatsLaterResolution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-release
			return []byte("v"), nil
		})
		defer c.Close()

		lctx := &lagCtx{Context: t.Context(), done: make(chan struct{})}
		defer close(lctx.done)
		type res struct {
			b   []byte
			err error
		}
		done := make(chan res, 1)
		go func() { b, err := c.Get(lctx, "k"); done <- res{b, err} }()
		synctest.Wait()
		lctx.cancelled.Store(true) // cancelled while in flight; waiter not yet woken
		close(release)             // load resolves first from the waiter's view
		r := <-done
		if !errors.Is(r.err, context.Canceled) || r.b != nil {
			t.Fatalf("Get = (%q, %v), want (nil, context.Canceled)", r.b, r.err)
		}
		if n := c.Len(); n != 1 {
			t.Fatalf("Len = %d, want 1 (load still populates)", n)
		}
	})
}

// TestCancelAfterResolutionGetsResult: a ctx cancelled only after the load
// resolved does not override the result (rule 7c).
func TestCancelAfterResolutionGetsResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-release
			return []byte("v"), nil
		})
		defer c.Close()

		ctx, cancel := context.WithCancel(t.Context())
		type res struct {
			b   []byte
			err error
		}
		done := make(chan res, 1)
		go func() { b, err := c.Get(ctx, "k"); done <- res{b, err} }()
		synctest.Wait()
		close(release)
		synctest.Wait() // the load has resolved strictly before the cancel below
		cancel()
		r := <-done
		if r.err != nil || string(r.b) != "v" {
			t.Fatalf("Get = (%q, %v), want (v, nil)", r.b, r.err)
		}
	})
}
