package blockcache

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
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

	b, err := c.Get(ctx, "k")
	if !errors.Is(err, boom) || b != nil {
		t.Fatalf("Get = (%q, %v), want (nil, %v)", b, err, boom)
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0", n)
	}
	c.Get(ctx, "k")
	if n := calls.Load(); n != 2 {
		t.Fatalf("Loader calls = %d, want 2", n)
	}
}

func TestLoaderSliceNotRetained(t *testing.T) {
	ctx := testCtx(t)
	var kept []byte
	c := New(2, func(context.Context, string) ([]byte, error) {
		kept = []byte("orig")
		return kept, nil
	})
	t.Cleanup(func() { c.Close() })

	mustGet(t, ctx, c, "k")
	copy(kept, "XXXX")
	if got := mustGet(t, ctx, c, "k"); string(got) != "orig" {
		t.Fatalf("hit = %q, want %q", got, "orig")
	}
}

func TestDistinctCopiesPerWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
			wg.Go(func() { res[i], _ = c.Get(t.Context(), "k") })
		}
		synctest.Wait()
		close(release)
		wg.Wait()

		res[0][0] = 'X'
		for i := 1; i < n; i++ {
			if string(res[i]) != "data" {
				t.Fatalf("res[%d] = %q, want %q", i, res[i], "data")
			}
		}
		if got, _ := c.Get(t.Context(), "k"); string(got) != "data" {
			t.Fatalf("cache = %q, want %q", got, "data")
		}
	})
}

func TestCallerCancelOrphanedLoadPopulates(t *testing.T) {
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
		go func() {
			_, err := c.Get(ctx, "k")
			errc <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("Get err = %v, want context.Canceled", err)
		}
		close(release)
		synctest.Wait()
		if n := c.Len(); n != 1 {
			t.Fatalf("Len = %d, want 1", n)
		}
		if b, err := c.Get(t.Context(), "k"); err != nil || string(b) != "v" {
			t.Fatalf("Get = (%q, %v), want (v, nil)", b, err)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("Loader calls = %d, want 1", n)
		}
	})
}

// TestResolvedBeatsCtx exercises wait's re-check: with the load already
// resolved and ctx already Done, the result must win regardless of which
// select case fires.
func TestResolvedBeatsCtx(t *testing.T) {
	c := New(1, func(context.Context, string) ([]byte, error) { return nil, nil }).(*cache)
	defer c.Close()
	cl := &call{done: make(chan struct{}), val: []byte("v")}
	close(cl.done)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		b, err := c.wait(ctx, cl)
		if err != nil || string(b) != "v" {
			t.Fatalf("wait = (%q, %v), want (v, nil)", b, err)
		}
	}
}

// TestClosedBeatsCtx: with the cache closed and ctx Done, ErrClosed wins.
func TestClosedBeatsCtx(t *testing.T) {
	c := New(1, func(context.Context, string) ([]byte, error) { return nil, nil }).(*cache)
	c.Close()
	cl := &call{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		if _, err := c.wait(ctx, cl); !errors.Is(err, ErrClosed) {
			t.Fatalf("wait err = %v, want ErrClosed", err)
		}
	}
}

func TestCloseWakesWaitersAndJoinsLoader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var returned atomic.Bool
		c := New(2, func(ctx context.Context, _ string) ([]byte, error) {
			<-ctx.Done()
			time.Sleep(time.Second) // Close must still wait for this.
			returned.Store(true)
			return []byte("late"), nil
		})
		const n = 3
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { _, errs[i] = c.Get(t.Context(), "k") })
		}
		synctest.Wait()
		if err := c.Close(); err != nil {
			t.Fatalf("Close = %v", err)
		}
		if !returned.Load() {
			t.Fatal("Close returned before the Loader returned")
		}
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("waiter %d err = %v, want ErrClosed", i, err)
			}
		}
		if n := c.Len(); n != 0 {
			t.Fatalf("Len = %d, want 0", n)
		}
	})
}

func TestCloseWindowDiscardsResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := New(2, func(_ context.Context, k string) ([]byte, error) {
			if k == "k" {
				<-release // ignores ctx until released
			}
			return []byte("v"), nil
		})
		mustGet(t, t.Context(), c, "a") // resident entry to be dropped
		go c.Get(t.Context(), "k")
		synctest.Wait()

		var closed atomic.Bool
		go func() { c.Close(); closed.Store(true) }()
		synctest.Wait()
		if closed.Load() {
			t.Fatal("Close returned while a Loader was still running")
		}
		close(release)
		synctest.Wait()
		if !closed.Load() {
			t.Fatal("Close did not return after the Loader returned")
		}
		if n := c.Len(); n != 0 {
			t.Fatalf("Len = %d, want 0", n)
		}
	})
}

func TestConcurrentClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-release
			return nil, nil
		})
		go c.Get(t.Context(), "k")
		synctest.Wait()

		var done atomic.Int32
		errs := make(chan error, 2)
		for range 2 {
			go func() { errs <- c.Close(); done.Add(1) }()
		}
		synctest.Wait()
		if n := done.Load(); n != 0 {
			t.Fatalf("%d Close calls returned before the Loader returned", n)
		}
		close(release)
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("Close = %v", err)
			}
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

func TestLenBoundUnderConcurrentPoll(t *testing.T) {
	ctx := testCtx(t)
	c := New(2, func(_ context.Context, k string) ([]byte, error) { return []byte(k), nil })
	t.Cleanup(func() { c.Close() })

	stop := make(chan struct{})
	var bad atomic.Int32
	var pw sync.WaitGroup
	pw.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				if c.Len() > 2 {
					bad.Add(1)
				}
			}
		}
	})
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() { mustGet(t, ctx, c, fmt.Sprint(i)) })
	}
	wg.Wait()
	close(stop)
	pw.Wait()
	if bad.Load() != 0 {
		t.Fatal("Len exceeded capacity")
	}
}

func TestPendingLoadNotCountedOrEvicting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var mu sync.Mutex
		counts := map[string]int{}
		c := New(1, func(_ context.Context, k string) ([]byte, error) {
			mu.Lock()
			counts[k]++
			mu.Unlock()
			if k == "b" {
				<-release
			}
			return []byte(k), nil
		})
		defer c.Close()

		mustGet(t, t.Context(), c, "a")
		go c.Get(t.Context(), "b")
		synctest.Wait()
		mustGet(t, t.Context(), c, "a") // still a hit
		if n := c.Len(); n != 1 {
			t.Fatalf("Len = %d, want 1", n)
		}
		close(release)
		synctest.Wait()
		mustGet(t, t.Context(), c, "b") // hit
		mustGet(t, t.Context(), c, "a") // reload: evicted by b
		mu.Lock()
		defer mu.Unlock()
		if counts["a"] != 2 || counts["b"] != 1 {
			t.Fatalf("counts = %v, want a:2 b:1", counts)
		}
	})
}

func TestLoaderPanic(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := New(1, func(context.Context, string) ([]byte, error) {
		if calls.Add(1) == 1 {
			panic("kaboom")
		}
		return []byte("ok"), nil
	})
	if _, err := c.Get(ctx, "k"); err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("err = %v, want loader panic error", err)
	}
	if b := mustGet(t, ctx, c, "k"); string(b) != "ok" {
		t.Fatalf("Get = %q, want ok", b)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

func TestNewPanicsNegative(t *testing.T) {
	load := func(context.Context, string) ([]byte, error) { return nil, nil }
	assertPanics(t, "New(-1, load)", func() { New(-1, load) })
}

func TestStress(t *testing.T) {
	ctx := testCtx(t)
	c := New(8, func(ctx context.Context, k string) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(rand.Intn(200)) * time.Microsecond):
		}
		if rand.Intn(10) == 0 {
			return nil, errors.New("flaky")
		}
		return []byte(k), nil
	})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 200 {
				k := fmt.Sprint(rand.Intn(20))
				b, err := c.Get(ctx, k)
				if err == nil && string(b) != k {
					t.Errorf("Get(%s) = %q", k, b)
				}
				if c.Len() > 8 {
					t.Error("Len exceeded capacity")
				}
			}
		})
	}
	time.Sleep(10 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	wg.Wait()
	if ctx.Err() != nil {
		t.Fatal("stress test hit its deadline")
	}
	if n := c.Len(); n != 0 {
		t.Fatalf("Len after Close = %d, want 0", n)
	}
}
