package blockcache

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestPerWaiterCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		release := make(chan struct{})
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-release
			return []byte("data"), nil
		})
		defer c.Close()

		const n = 5
		res := make([][]byte, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { res[i], _ = c.Get(ctx, "k") })
		}
		synctest.Wait()
		close(release)
		wg.Wait()

		res[0][0] = 'X'
		for i := 1; i < n; i++ {
			if string(res[i]) != "data" {
				t.Errorf("waiter %d saw %q after waiter 0 mutated", i, res[i])
			}
			if &res[i][0] == &res[0][0] {
				t.Errorf("waiter %d shares backing array with waiter 0", i)
			}
		}
		if b, _ := c.Get(ctx, "k"); string(b) != "data" {
			t.Errorf("hit = %q, want data", b)
		}
	})
}

func TestLoaderSliceNotRetained(t *testing.T) {
	ctx := testCtx(t)
	var last []byte
	c := New(2, func(context.Context, string) ([]byte, error) {
		last = []byte("orig")
		return last, nil
	})
	defer c.Close()
	mustGet(t, ctx, c, "k")
	copy(last, "XXXX")
	if b := mustGet(t, ctx, c, "k"); string(b) != "orig" {
		t.Fatalf("hit = %q, want orig", b)
	}
}

func TestErrorWins(t *testing.T) {
	ctx := testCtx(t)
	boom := errors.New("boom")
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("x"), boom
	})
	defer c.Close()
	b, err := c.Get(ctx, "k")
	if b != nil || !errors.Is(err, boom) {
		t.Fatalf("Get = (%q, %v), want (nil, boom)", b, err)
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
	c.Get(ctx, "k")
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestErrorSingleFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		boom := errors.New("boom")
		var calls atomic.Int32
		release := make(chan struct{})
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
			wg.Go(func() { _, errs[i] = c.Get(ctx, "k") })
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, boom) {
				t.Errorf("waiter %d err = %v, want boom", i, err)
			}
		}
		if calls.Load() != 1 {
			t.Errorf("calls = %d, want 1", calls.Load())
		}
	})
}

func TestOrphanedLoadPopulates(t *testing.T) {
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
		errc := make(chan error, 1)
		go func() { _, err := c.Get(ctx, "k"); errc <- err }()
		synctest.Wait()
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Errorf("Get err = %v, want context.Canceled", err)
		}
		close(release)
		synctest.Wait()
		if c.Len() != 1 {
			t.Errorf("Len = %d, want 1", c.Len())
		}
		if b, err := c.Get(t.Context(), "k"); err != nil || string(b) != "v" {
			t.Errorf("Get = (%q, %v)", b, err)
		}
		if calls.Load() != 1 {
			t.Errorf("calls = %d, want 1", calls.Load())
		}
	})
}

func TestCloseWakesWaiterEvenWithDoneCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var returned atomic.Bool
		c := New(2, func(ctx context.Context, _ string) ([]byte, error) {
			<-ctx.Done()
			returned.Store(true)
			return []byte("late"), nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() { _, err := c.Get(ctx, "k"); errc <- err }()
		synctest.Wait()
		if err := c.Close(); err != nil {
			t.Errorf("Close = %v", err)
		}
		cancel()
		if !returned.Load() {
			t.Errorf("Close returned before Loader returned")
		}
		if err := <-errc; !errors.Is(err, ErrClosed) {
			t.Errorf("waiter err = %v, want ErrClosed", err)
		}
		if c.Len() != 0 {
			t.Errorf("Len = %d, want 0", c.Len())
		}
	})
}

func TestCloseDiscardsLateSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-release // ignores cancellation until released
			return []byte("v"), nil
		})
		go c.Get(context.Background(), "k")
		synctest.Wait()
		done := make(chan struct{})
		go func() { c.Close(); close(done) }()
		synctest.Wait()
		select {
		case <-done:
			t.Errorf("Close returned before Loader")
		default:
		}
		close(release)
		<-done
		if c.Len() != 0 {
			t.Errorf("Len = %d, want 0", c.Len())
		}
	})
}

func TestConcurrentClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var loaderDone atomic.Bool
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-release
			loaderDone.Store(true)
			return nil, nil
		})
		go c.Get(context.Background(), "k")
		synctest.Wait()
		var wg sync.WaitGroup
		var early atomic.Int32
		for range 2 {
			wg.Go(func() {
				if err := c.Close(); err != nil {
					t.Errorf("Close = %v", err)
				}
				if !loaderDone.Load() {
					early.Add(1)
				}
			})
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		if early.Load() != 0 {
			t.Errorf("%d Close calls returned before the Loader", early.Load())
		}
	})
}

func TestCloseNoInflightReturnsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := New(1, func(context.Context, string) ([]byte, error) { return []byte("v"), nil })
		mustGet(t, t.Context(), c, "k")
		c.Close()
	})
}

func TestGetAfterCloseNoLoader(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("v"), nil
	})
	mustGet(t, ctx, c, "a")
	c.Close()
	for _, k := range []string{"a", "b"} {
		if _, err := c.Get(ctx, k); err != ErrClosed {
			t.Fatalf("Get(%q) = %v, want ErrClosed", k, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestGetRacingClose(t *testing.T) {
	ctx := testCtx(t)
	for range 200 {
		var calls atomic.Int32
		c := New(4, func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			return []byte("v"), nil
		})
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				b, err := c.Get(ctx, fmt.Sprint(i%3))
				if err != nil && err != ErrClosed {
					t.Errorf("Get err = %v", err)
				}
				if err == nil && string(b) != "v" {
					t.Errorf("Get = %q", b)
				}
			})
		}
		c.Close()
		n := calls.Load()
		if c.Len() != 0 {
			t.Fatalf("Len after Close = %d", c.Len())
		}
		wg.Wait()
		if calls.Load() != n {
			t.Fatalf("Loader called after Close returned")
		}
		if c.Len() != 0 {
			t.Fatalf("Len after Close = %d", c.Len())
		}
	}
}

func TestPendingNotCountedNorEvictable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(1, func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			if key == "a" {
				<-release
			}
			return []byte(key), nil
		})
		defer c.Close()
		mustGet(t, ctx, c, "b")
		go c.Get(ctx, "a")
		synctest.Wait()
		if c.Len() != 1 {
			t.Errorf("Len = %d, want 1", c.Len())
		}
		mustGet(t, ctx, c, "b")
		if calls.Load() != 2 {
			t.Errorf("calls = %d, want 2 (b must hit)", calls.Load())
		}
		close(release)
		synctest.Wait()
		mustGet(t, ctx, c, "a")
		if calls.Load() != 2 {
			t.Errorf("calls = %d, want 2 (a must hit)", calls.Load())
		}
		mustGet(t, ctx, c, "b")
		if calls.Load() != 3 {
			t.Errorf("calls = %d, want 3 (b must have been evicted)", calls.Load())
		}
	})
}

func TestLoadMarksMRU(t *testing.T) {
	ctx := testCtx(t)
	var mu sync.Mutex
	counts := map[string]int{}
	c := New(2, func(_ context.Context, key string) ([]byte, error) {
		mu.Lock()
		counts[key]++
		mu.Unlock()
		return []byte(key), nil
	})
	defer c.Close()
	for _, k := range []string{"a", "b", "c", "b", "a"} {
		mustGet(t, ctx, c, k)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["a"] != 2 || counts["b"] != 1 || counts["c"] != 1 {
		t.Fatalf("counts = %v, want a:2 b:1 c:1", counts)
	}
}

func TestLenBoundUnderConcurrency(t *testing.T) {
	ctx := testCtx(t)
	const capacity = 4
	c := New(capacity, func(_ context.Context, key string) ([]byte, error) {
		return []byte(key), nil
	})
	defer c.Close()
	stop := make(chan struct{})
	var poller sync.WaitGroup
	poller.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := c.Len(); n > capacity {
				t.Errorf("Len = %d > %d", n, capacity)
				return
			}
		}
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 500 {
				if _, err := c.Get(ctx, fmt.Sprint(rand.IntN(20))); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	poller.Wait()
}

func TestLoaderPanic(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		if calls.Add(1) == 1 {
			panic("kaboom")
		}
		return []byte("ok"), nil
	})
	_, err := c.Get(ctx, "k")
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("err = %v, want panic error", err)
	}
	if b := mustGet(t, ctx, c, "k"); string(b) != "ok" {
		t.Fatalf("reload = %q", b)
	}
	c.Close()
}

func TestLoaderGoexit(t *testing.T) {
	ctx := testCtx(t)
	c := New(2, func(context.Context, string) ([]byte, error) {
		runtime.Goexit()
		return nil, nil
	})
	if _, err := c.Get(ctx, "k"); !errors.Is(err, errLoaderExited) {
		t.Fatalf("err = %v, want errLoaderExited", err)
	}
	c.Close()
}

func TestNewPanicsNegative(t *testing.T) {
	assertPanics(t, "New(-1, load)", func() {
		New(-1, func(context.Context, string) ([]byte, error) { return nil, nil })
	})
}

// Rule 7(b): ctx cancelled while the load is in flight, then the load resolves
// before the parked waiter is rescheduled. The waiter must still get ctx.Err().
func TestCtxDoneThenResolveBeforeWakeup(t *testing.T) {
	for _, loadErr := range []error{nil, errors.New("boom")} {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			c := New(2, func(context.Context, string) ([]byte, error) {
				<-release
				return []byte("v"), loadErr
			})
			defer c.Close()
			const n = 20
			ctx, cancel := context.WithCancel(t.Context())
			errs := make([]error, n)
			var wg sync.WaitGroup
			for i := range n {
				wg.Go(func() { _, errs[i] = c.Get(ctx, "k") })
			}
			synctest.Wait()
			cancel()
			close(release)
			wg.Wait()
			for i, err := range errs {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("loadErr=%v waiter %d err = %v, want context.Canceled", loadErr, i, err)
				}
			}
		})
	}
}

// Rule 7(c): the load resolves first, then ctx is cancelled before the waiter
// necessarily runs. The waiter must get the result.
func TestResolveThenCtxDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-release
			return []byte("v"), nil
		})
		defer c.Close()
		const n = 20
		ctx, cancel := context.WithCancel(t.Context())
		res := make([][]byte, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { res[i], errs[i] = c.Get(ctx, "k") })
		}
		synctest.Wait()
		close(release)
		for c.Len() == 0 {
			runtime.Gosched()
		}
		cancel()
		wg.Wait()
		for i := range n {
			if errs[i] != nil || string(res[i]) != "v" {
				t.Errorf("waiter %d = (%q, %v), want (v, nil)", i, res[i], errs[i])
			}
		}
	})
}

// Rule 8: once Close has begun, every completing Loader sees a cancelled ctx.
func TestCompletionAfterCloseSeesCancelledCtx(t *testing.T) {
	for range 200 {
		var sawLive atomic.Bool
		entered := make(chan struct{})
		var c Cache
		c = New(1, func(ctx context.Context, _ string) ([]byte, error) {
			close(entered)
			for !c.(*cache).isClosed() {
				runtime.Gosched()
			}
			if ctx.Err() == nil {
				sawLive.Store(true)
			}
			return []byte("v"), nil
		})
		go c.Get(context.Background(), "k")
		<-entered
		c.Close()
		if sawLive.Load() {
			t.Fatal("Loader observed closed cache with live ctx")
		}
		if c.Len() != 0 {
			t.Fatalf("Len = %d", c.Len())
		}
	}
}

func (c *cache) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
