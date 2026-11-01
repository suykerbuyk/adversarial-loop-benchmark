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

func TestPerWaiterCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var loaderSlice []byte
		c := New(4, func(context.Context, string) ([]byte, error) {
			<-release
			loaderSlice = []byte("data")
			return loaderSlice, nil
		})
		defer c.Close()

		const n = 5
		res := make([][]byte, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i], _ = c.Get(t.Context(), "k")
			}()
		}
		synctest.Wait()
		close(release)
		wg.Wait()

		res[0][0] = 'X'
		loaderSlice[1] = 'Y'
		for i := 1; i < n; i++ {
			if string(res[i]) != "data" {
				t.Errorf("waiter %d got %q", i, res[i])
			}
		}
		if b, _ := c.Get(t.Context(), "k"); string(b) != "data" {
			t.Errorf("hit got %q", b)
		}
	})
}

func TestErrorWithBytesDiscarded(t *testing.T) {
	boom := errors.New("boom")
	var calls atomic.Int32
	c := New(4, func(context.Context, string) ([]byte, error) {
		if calls.Add(1) == 1 {
			return []byte("partial"), boom
		}
		return []byte("ok"), nil
	})
	defer c.Close()
	b, err := c.Get(context.Background(), "k")
	if !errors.Is(err, boom) || b != nil {
		t.Fatalf("got (%q, %v), want (nil, boom)", b, err)
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
	if b, err := c.Get(context.Background(), "k"); err != nil || string(b) != "ok" {
		t.Fatalf("reload got (%q, %v)", b, err)
	}
}

func TestErrorSharedByAllWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("boom")
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(4, func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return nil, boom
		})
		defer c.Close()
		const n = 10
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() { defer wg.Done(); _, errs[i] = c.Get(t.Context(), "k") }()
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		if calls.Load() != 1 {
			t.Errorf("calls = %d", calls.Load())
		}
		for i, e := range errs {
			if !errors.Is(e, boom) {
				t.Errorf("waiter %d err = %v", i, e)
			}
		}
	})
}

func TestCallerCtxCancelWhileInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(4, func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		})
		defer c.Close()
		ctx, cancel := context.WithCancel(t.Context())
		var err error
		done := make(chan struct{})
		go func() { _, err = c.Get(ctx, "k"); close(done) }()
		synctest.Wait()
		cancel()
		<-done
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want Canceled", err)
		}
		close(release)
		synctest.Wait()
		if c.Len() != 1 {
			t.Errorf("Len = %d, want 1 (orphaned load populates)", c.Len())
		}
		if b, err := c.Get(t.Context(), "k"); err != nil || string(b) != "v" || calls.Load() != 1 {
			t.Errorf("hit got (%q, %v), calls %d", b, err, calls.Load())
		}
	})
}

func TestCancelThenReleaseBeforeWaiterRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := New(4, func(context.Context, string) ([]byte, error) {
			<-release
			return []byte("v"), nil
		})
		defer c.Close()
		ctx, cancel := context.WithCancel(t.Context())
		var err error
		done := make(chan struct{})
		go func() { _, err = c.Get(ctx, "k"); close(done) }()
		synctest.Wait()
		cancel()
		close(release)
		<-done
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want Canceled", err)
		}
	})
}

// newWhiteBox returns a cache and a hand-registered waiter on a hand-built flight.
func newWhiteBox(t *testing.T, ctx context.Context) (*cache, *flight, *waiter) {
	t.Helper()
	c := New(4, func(context.Context, string) ([]byte, error) { return nil, nil }).(*cache)
	t.Cleanup(func() { c.Close() })
	f := &flight{done: make(chan struct{}), waiters: map[*waiter]struct{}{}}
	w := &waiter{ctx: ctx}
	c.mu.Lock()
	c.inflight["k"] = f
	f.waiters[w] = struct{}{}
	c.mu.Unlock()
	return c, f, w
}

func TestResolveThenCancelReturnsResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, f, w := newWhiteBox(t, ctx)
	c.resolve("k", f, []byte("v"), nil)
	cancel()
	b, err := c.await(ctx, f, w)
	if err != nil || string(b) != "v" {
		t.Fatalf("got (%q, %v), want (v, nil)", b, err)
	}
}

func TestCancelThenResolveReturnsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, f, w := newWhiteBox(t, ctx)
	cancel()
	c.resolve("k", f, []byte("v"), nil)
	b, err := c.await(ctx, f, w)
	if !errors.Is(err, context.Canceled) || b != nil {
		t.Fatalf("got (%q, %v), want (nil, Canceled)", b, err)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
}

func TestOtherWaitersUnaffectedByOneCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := New(4, func(context.Context, string) ([]byte, error) {
			<-release
			return []byte("v"), nil
		}).(*cache)
		defer c.Close()
		ctx1, cancel1 := context.WithCancel(t.Context())
		var err1, err2 error
		var b2 []byte
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, err1 = c.Get(ctx1, "k") }()
		go func() { defer wg.Done(); b2, err2 = c.Get(t.Context(), "k") }()
		synctest.Wait()
		c.mu.Lock()
		f := c.inflight["k"]
		c.mu.Unlock()
		cancel1()
		synctest.Wait()
		c.mu.Lock()
		if n := len(f.waiters); n != 1 {
			t.Errorf("waiters after one cancel = %d, want 1", n)
		}
		c.mu.Unlock()
		close(release)
		wg.Wait()
		if !errors.Is(err1, context.Canceled) {
			t.Errorf("err1 = %v", err1)
		}
		if err2 != nil || string(b2) != "v" {
			t.Errorf("waiter2 got (%q, %v)", b2, err2)
		}
		c.mu.Lock()
		if n := len(f.waiters); n != 0 {
			t.Errorf("waiters after resolve = %d, want 0", n)
		}
		c.mu.Unlock()
	})
}

func TestLoaderGetsCacheCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var loaderCtx context.Context
		started := make(chan struct{})
		c := New(4, func(ctx context.Context, _ string) ([]byte, error) {
			loaderCtx = ctx
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { c.Get(ctx, "k"); close(done) }()
		<-started
		cancel()
		<-done
		synctest.Wait()
		if loaderCtx.Err() != nil {
			t.Errorf("caller cancel cancelled loader ctx")
		}
		c.Close()
		if loaderCtx.Err() == nil {
			t.Errorf("Close did not cancel loader ctx")
		}
	})
}

func TestCloseWaitsForLoader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var loaderReturned atomic.Bool
		c := New(4, func(ctx context.Context, _ string) ([]byte, error) {
			<-ctx.Done()
			time.Sleep(time.Second)
			loaderReturned.Store(true)
			return []byte("late"), nil
		})
		const n = 3
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() { defer wg.Done(); _, errs[i] = c.Get(t.Context(), "k") }()
		}
		synctest.Wait()
		c.Close()
		if !loaderReturned.Load() {
			t.Errorf("Close returned before Loader")
		}
		wg.Wait()
		for i, e := range errs {
			if !errors.Is(e, ErrClosed) {
				t.Errorf("waiter %d err = %v", i, e)
			}
		}
		if c.Len() != 0 {
			t.Errorf("Len = %d, want 0", c.Len())
		}
	})
}

// TestCloseWithCancelledWaiter: a waiter whose ctx is Done but which is still
// unsettled when Close runs gets ErrClosed (rule 7 priority (a) over (b)).
// White-box so the ordering is deterministic: with real goroutines the waiter
// may legitimately self-settle with ctx.Err() before Close takes the lock.
func TestCloseWithCancelledWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, f, w := newWhiteBox(t, ctx)
	cancel()
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	<-c.closing // Close has settled every waiter and is now joining the flight
	if _, err := c.await(ctx, f, w); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
	c.resolve("k", f, []byte("late"), nil) // the "Loader" returns after cancel
	<-closed
	if c.Len() != 0 {
		t.Errorf("Len = %d, want 0", c.Len())
	}
}

func TestCloseZeroWaiterLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := New(4, func(ctx context.Context, _ string) ([]byte, error) {
			<-ctx.Done()
			return []byte("late"), nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := c.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
		c.Close()
		if c.Len() != 0 {
			t.Errorf("Len = %d, want 0", c.Len())
		}
	})
}

func TestConcurrentClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var loaderReturned atomic.Bool
		c := New(4, func(ctx context.Context, _ string) ([]byte, error) {
			<-ctx.Done()
			time.Sleep(time.Second)
			loaderReturned.Store(true)
			return nil, ctx.Err()
		})
		go c.Get(context.Background(), "k")
		synctest.Wait()
		go c.Close()
		synctest.Wait() // first Close is now joining the Loader
		if err := c.Close(); err != nil {
			t.Errorf("second Close = %v", err)
		}
		if !loaderReturned.Load() {
			t.Errorf("second Close returned before the first's join")
		}
	})
}

func TestGetAfterCloseNoLoader(t *testing.T) {
	var calls atomic.Int32
	c := New(4, func(context.Context, string) ([]byte, error) { calls.Add(1); return nil, nil })
	c.Close()
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("Loader called %d times", calls.Load())
	}
}

func TestInflightNotCountedTowardCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(1, func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			if key == "b" {
				<-release
			}
			return []byte(key), nil
		})
		defer c.Close()
		mustGet(t, t.Context(), c, "a")
		go c.Get(t.Context(), "b")
		synctest.Wait()
		if b := mustGet(t, t.Context(), c, "a"); string(b) != "a" || calls.Load() != 2 {
			t.Errorf("resident a not hit: %q calls=%d", b, calls.Load())
		}
		if c.Len() != 1 {
			t.Errorf("Len = %d", c.Len())
		}
		close(release)
		synctest.Wait()
		mustGet(t, t.Context(), c, "b")
		if calls.Load() != 2 {
			t.Errorf("b not resident after resolve")
		}
		mustGet(t, t.Context(), c, "a")
		if calls.Load() != 3 {
			t.Errorf("a not evicted")
		}
	})
}

func TestLoadMarksMRU(t *testing.T) {
	var calls atomic.Int32
	c := New(2, func(_ context.Context, key string) ([]byte, error) { calls.Add(1); return []byte(key), nil })
	defer c.Close()
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c", "b", "c"} {
		mustGet(t, ctx, c, k)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	mustGet(t, ctx, c, "a")
	if calls.Load() != 4 {
		t.Fatalf("a should have been evicted")
	}
}

func TestLenNeverAboveCapacityUnderPoll(t *testing.T) {
	const capacity = 3
	c := New(capacity, func(_ context.Context, key string) ([]byte, error) { return []byte(key), nil })
	defer c.Close()
	stop := make(chan struct{})
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		for {
			select {
			case <-stop:
				return
			default:
				if n := c.Len(); n > capacity {
					t.Errorf("Len = %d > %d", n, capacity)
					return
				}
			}
		}
	}()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				c.Get(context.Background(), fmt.Sprint((g*31+i)%20))
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-pollDone
}

func TestStressRace(t *testing.T) {
	c := New(4, func(ctx context.Context, key string) ([]byte, error) {
		if rand.Intn(10) == 0 {
			return nil, errors.New("flaky")
		}
		select {
		case <-time.After(time.Duration(rand.Intn(200)) * time.Microsecond):
		case <-ctx.Done():
		}
		return []byte("v:" + key), nil
	})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 300 {
				key := fmt.Sprint(rand.Intn(12))
				ctx, cancel := context.WithCancel(context.Background())
				if rand.Intn(5) == 0 {
					go cancel()
				}
				b, err := c.Get(ctx, key)
				cancel()
				if err == nil && string(b) != "v:"+key {
					t.Errorf("key %s got %q", key, b)
				}
			}
		}()
	}
	wg.Wait()
	c.Close()
	if c.Len() != 0 {
		t.Fatalf("Len after Close = %d", c.Len())
	}
}

func TestLoaderPanic(t *testing.T) {
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		if calls.Add(1) == 1 {
			panic("kaboom")
		}
		return []byte("ok"), nil
	})
	defer c.Close()
	if _, err := c.Get(context.Background(), "k"); err == nil {
		t.Fatal("want error from panicking Loader")
	}
	if b, err := c.Get(context.Background(), "k"); err != nil || string(b) != "ok" {
		t.Fatalf("got (%q, %v)", b, err)
	}
}

func TestNewPanicsNegative(t *testing.T) {
	assertPanics(t, "New(-1, load)", func() {
		New(-1, func(context.Context, string) ([]byte, error) { return nil, nil })
	})
}
