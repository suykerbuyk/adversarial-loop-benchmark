package blockcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func bubbleCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestPerWaiterCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		release := make(chan struct{})
		c := New(4, func(_ context.Context, _ string) ([]byte, error) {
			<-release
			return []byte("data"), nil
		})
		defer c.Close()

		const n = 5
		res := make([][]byte, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { res[i], errs[i] = c.Get(ctx, "k") })
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
				t.Errorf("res[%d] = %q, mutation leaked", i, res[i])
			}
			if &res[i][0] == &res[0][0] {
				t.Errorf("res[%d] shares backing array with res[0]", i)
			}
		}
		if b := mustGet(t, ctx, c, "k"); string(b) != "data" {
			t.Errorf("hit = %q, mutation leaked into cache", b)
		}
	})
}

func TestLoaderSliceNotRetained(t *testing.T) {
	ctx := testCtx(t)
	shared := []byte("orig")
	c := New(2, func(_ context.Context, _ string) ([]byte, error) { return shared, nil })
	defer c.Close()
	mustGet(t, ctx, c, "k")
	copy(shared, "XXXX")
	if b := mustGet(t, ctx, c, "k"); string(b) != "orig" {
		t.Fatalf("hit = %q, want orig (Loader slice retained)", b)
	}
}

func TestBytesWithErrorDiscarded(t *testing.T) {
	ctx := testCtx(t)
	boom := errors.New("boom")
	var calls atomic.Int32
	c := New(2, func(_ context.Context, _ string) ([]byte, error) {
		calls.Add(1)
		return []byte("x"), boom
	})
	defer c.Close()
	b, err := c.Get(ctx, "k")
	if !errors.Is(err, boom) || b != nil {
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

func TestHitWithCancelledCtxMarksMRU(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := New(2, func(_ context.Context, k string) ([]byte, error) {
		calls.Add(1)
		return []byte("v:" + k), nil
	})
	defer c.Close()
	mustGet(t, ctx, c, "a")
	mustGet(t, ctx, c, "b")
	b, err := c.Get(cancelledCtx(), "a")
	if err != nil || string(b) != "v:a" {
		t.Fatalf("cancelled-ctx hit = (%q, %v)", b, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	mustGet(t, ctx, c, "c") // evicts b
	mustGet(t, ctx, c, "a") // hit
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 (a should have survived)", calls.Load())
	}
	mustGet(t, ctx, c, "b")
	if calls.Load() != 4 {
		t.Fatalf("calls = %d, want 4 (b should have been evicted)", calls.Load())
	}
}

func TestWaiterCtxCancelledWhileBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(2, func(_ context.Context, _ string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		})
		defer c.Close()

		wctx, cancel := context.WithCancel(t.Context())
		var err error
		done := make(chan struct{})
		go func() { _, err = c.Get(wctx, "k"); close(done) }()
		synctest.Wait()
		cancel()
		<-done
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want Canceled", err)
		}
		close(release)
		synctest.Wait()
		if c.Len() != 1 {
			t.Errorf("Len = %d, want 1 (orphaned load must admit)", c.Len())
		}
		mustGet(t, bubbleCtx(t), c, "k")
		if calls.Load() != 1 {
			t.Errorf("calls = %d, want 1", calls.Load())
		}
	})
}

func TestPreCancelledColdMiss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(2, func(_ context.Context, _ string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		})
		defer c.Close()
		if _, err := c.Get(cancelledCtx(), "k"); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want Canceled", err)
		}
		synctest.Wait()
		if calls.Load() != 1 {
			t.Errorf("calls = %d, want 1", calls.Load())
		}
		close(release)
		synctest.Wait()
		if c.Len() != 1 {
			t.Errorf("Len = %d, want 1", c.Len())
		}
		mustGet(t, bubbleCtx(t), c, "k")
		if calls.Load() != 1 {
			t.Errorf("calls = %d, want 1 (live Get should hit)", calls.Load())
		}
	})
}

func TestLoaderGetsCacheCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var lctx context.Context
		entered := make(chan struct{})
		release := make(chan struct{})
		c := New(2, func(ctx context.Context, _ string) ([]byte, error) {
			lctx = ctx
			close(entered)
			<-release
			return []byte("v"), nil
		})
		wctx, cancel := context.WithCancel(t.Context())
		go c.Get(wctx, "k")
		<-entered
		cancel()
		synctest.Wait()
		if lctx.Err() != nil {
			t.Errorf("Loader ctx cancelled by caller ctx: %v", lctx.Err())
		}
		close(release)
		synctest.Wait()
		c.Close()
		if lctx.Err() == nil {
			t.Errorf("Loader ctx not Done after Close")
		}
	})
}

func TestOpenCacheLoaderErrorPublishedAsIs(t *testing.T) {
	ctx := testCtx(t)
	c := New(2, func(_ context.Context, _ string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	})
	defer c.Close()
	if _, err := c.Get(ctx, "k"); err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestWaitPriority(t *testing.T) {
	loaderErr := errors.New("loader")
	cases := []struct {
		name     string
		resolved bool
		val      []byte
		err      error
		closed   bool
		wantVal  string
		wantErr  error
	}{
		{"resolved value beats closed and ctx", true, []byte("v"), nil, true, "v", nil},
		{"resolved ErrClosed", true, nil, ErrClosed, true, "", ErrClosed},
		{"closed beats ctx", false, nil, nil, true, "", ErrClosed},
		{"ctx while in flight", false, nil, nil, false, "", context.Canceled},
		{"resolved error beats ctx", true, nil, loaderErr, false, "", loaderErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(1, func(context.Context, string) ([]byte, error) { return nil, nil }).(*cache)
			defer c.Close()
			cl := &call{done: make(chan struct{}), val: tc.val, err: tc.err}
			if tc.resolved {
				close(cl.done)
			}
			if tc.closed {
				c.mu.Lock()
				c.closed = true
				close(c.closing)
				c.mu.Unlock()
				close(c.closeDone) // let the deferred Close return
			}
			b, err := c.wait(cancelledCtx(), cl)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if string(b) != tc.wantVal {
				t.Fatalf("val = %q, want %q", b, tc.wantVal)
			}
		})
	}
}

func TestResolvedBeforeClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		release := make(chan struct{})
		c := New(2, func(_ context.Context, _ string) ([]byte, error) {
			<-release
			return []byte("v"), nil
		})
		var b []byte
		var err error
		var wg sync.WaitGroup
		wg.Go(func() { b, err = c.Get(ctx, "k") })
		synctest.Wait()
		close(release)
		wg.Wait()
		c.Close()
		if err != nil || string(b) != "v" {
			t.Errorf("Get = (%q, %v)", b, err)
		}
		if c.Len() != 0 {
			t.Errorf("Len = %d after Close", c.Len())
		}
	})
}

func TestCloseJoinsLoadersAndDiscards(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		var returned atomic.Bool
		c := New(2, func(lctx context.Context, _ string) ([]byte, error) {
			<-lctx.Done()
			time.Sleep(time.Second) // ignore cancel for a while, then report success
			returned.Store(true)
			return []byte("late"), nil
		})
		const n = 3
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { _, errs[i] = c.Get(ctx, "k") })
		}
		synctest.Wait()

		var closed1, closed2 atomic.Bool
		go func() { c.Close(); closed1.Store(true) }()
		synctest.Wait()
		go func() { c.Close(); closed2.Store(true) }()
		synctest.Wait()
		if closed1.Load() || closed2.Load() {
			t.Errorf("Close returned before Loader returned")
		}
		wg.Wait() // waiters are woken before the join
		for i, err := range errs {
			if !errors.Is(err, ErrClosed) {
				t.Errorf("waiter %d err = %v, want ErrClosed", i, err)
			}
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if !returned.Load() || !closed1.Load() || !closed2.Load() {
			t.Errorf("returned=%v closed1=%v closed2=%v", returned.Load(), closed1.Load(), closed2.Load())
		}
		if c.Len() != 0 {
			t.Errorf("Len = %d, want 0 (late success must be discarded)", c.Len())
		}
	})
}

func TestGetAfterCloseResidentKey(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := New(2, func(_ context.Context, _ string) ([]byte, error) {
		calls.Add(1)
		return []byte("v"), nil
	})
	mustGet(t, ctx, c, "k")
	c.Close()
	if _, err := c.Get(ctx, "k"); err != ErrClosed {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestPendingNotCountedNorEvictable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(1, func(_ context.Context, k string) ([]byte, error) {
			calls.Add(1)
			if k == "b" {
				<-release
			}
			return []byte("v:" + k), nil
		})
		defer c.Close()
		mustGet(t, ctx, c, "a")
		go c.Get(ctx, "b")
		synctest.Wait()
		if c.Len() != 1 {
			t.Errorf("Len = %d, want 1", c.Len())
		}
		mustGet(t, ctx, c, "a")
		if calls.Load() != 2 {
			t.Errorf("calls = %d, want 2 (a should hit)", calls.Load())
		}
		close(release)
		synctest.Wait()
		if c.Len() != 1 {
			t.Errorf("Len = %d, want 1", c.Len())
		}
		mustGet(t, ctx, c, "a")
		if calls.Load() != 3 {
			t.Errorf("calls = %d, want 3 (a should have been evicted)", calls.Load())
		}
	})
}

func TestCompletionOrderRecency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		rel := map[string]chan struct{}{"x": make(chan struct{}), "y": make(chan struct{})}
		var mu sync.Mutex
		counts := map[string]int{}
		c := New(2, func(_ context.Context, k string) ([]byte, error) {
			mu.Lock()
			counts[k]++
			mu.Unlock()
			if ch, ok := rel[k]; ok {
				<-ch
			}
			return []byte("v:" + k), nil
		})
		defer c.Close()
		go c.Get(ctx, "x")
		go c.Get(ctx, "y")
		synctest.Wait()
		close(rel["y"])
		synctest.Wait()
		close(rel["x"])
		synctest.Wait()
		mustGet(t, ctx, c, "z") // evicts y (LRU)
		mustGet(t, ctx, c, "x")
		mustGet(t, ctx, c, "y")
		mu.Lock()
		defer mu.Unlock()
		if counts["x"] != 1 || counts["y"] != 2 {
			t.Errorf("counts = %v, want x:1 y:2", counts)
		}
	})
}

func TestLoaderPanic(t *testing.T) {
	ctx := testCtx(t)
	sentinel := errors.New("sentinel")
	var calls atomic.Int32
	c := New(2, func(_ context.Context, _ string) ([]byte, error) {
		switch calls.Add(1) {
		case 1:
			panic(sentinel)
		case 2:
			panic("plain")
		}
		return []byte("ok"), nil
	})
	_, err := c.Get(ctx, "secretkey")
	if !errors.Is(err, sentinel) || strings.Contains(err.Error(), "secretkey") {
		t.Fatalf("err = %v", err)
	}
	_, err = c.Get(ctx, "secretkey")
	if err == nil || strings.Contains(err.Error(), "secretkey") {
		t.Fatalf("err = %v", err)
	}
	if b := mustGet(t, ctx, c, "secretkey"); string(b) != "ok" {
		t.Fatalf("retry = %q", b)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStress(t *testing.T) {
	const (
		workers = 64
		keys    = 32
		capN    = 8
		iters   = 200
	)
	injected := errors.New("injected")
	var injectErrors atomic.Bool
	injectErrors.Store(true)
	var inProgress [keys]atomic.Int32
	keyName := func(i int) string { return fmt.Sprintf("k%d", i) }
	keyIdx := func(k string) int {
		var i int
		fmt.Sscanf(k, "k%d", &i)
		return i
	}

	c := New(capN, func(ctx context.Context, k string) ([]byte, error) {
		i := keyIdx(k)
		if inProgress[i].Add(1) > 1 {
			t.Errorf("single-flight violated for %s", k)
		}
		defer inProgress[i].Add(-1)
		select {
		case <-time.After(time.Duration(rand.IntN(200)) * time.Microsecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if injectErrors.Load() && rand.IntN(5) == 0 {
			return nil, injected
		}
		return []byte("v:" + k), nil
	})

	stopPoll := make(chan struct{})
	var pollWG sync.WaitGroup
	pollWG.Go(func() {
		for {
			select {
			case <-stopPoll:
				return
			default:
			}
			if l := c.Len(); l > capN {
				t.Errorf("Len = %d > %d", l, capN)
			}
		}
	})

	check := func(k string, b []byte, err error, ctx context.Context) {
		switch {
		case err == nil:
			if string(b) != "v:"+k {
				t.Errorf("Get(%s) = %q", k, b)
			}
		case errors.Is(err, injected), errors.Is(err, ErrClosed):
		case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		default:
			t.Errorf("Get(%s) unexpected err %v", k, err)
		}
	}
	burst := func() {
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				for range iters {
					k := keyName(rand.IntN(keys))
					ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rand.IntN(300))*time.Microsecond)
					b, err := c.Get(ctx, k)
					check(k, b, err, ctx)
					cancel()
				}
			})
		}
		wg.Wait()
	}

	deadline := time.AfterFunc(20*time.Second, func() { panic("stress test deadlocked") })
	defer deadline.Stop()

	// Phase 1: errors injected.
	burst()
	// Phase 2: errors off; every key must load successfully (no cached errors).
	injectErrors.Store(false)
	live := context.Background()
	for i := range keys {
		k := keyName(i)
		b, err := c.Get(live, k)
		if err != nil || !bytes.Equal(b, []byte("v:"+k)) {
			t.Errorf("phase 2 Get(%s) = (%q, %v)", k, b, err)
		}
	}
	// Phase 3: Close mid-burst.
	go func() {
		time.Sleep(5 * time.Millisecond)
		c.Close()
	}()
	burst()
	c.Close()
	close(stopPoll)
	pollWG.Wait()
	if c.Len() != 0 {
		t.Errorf("Len = %d after Close", c.Len())
	}
	for i := range keys {
		if _, err := c.Get(live, keyName(i)); err != ErrClosed {
			t.Errorf("post-Close Get err = %v", err)
		}
	}
}

// TestCloseBetweenWakeAndDecision pins the window in wait between a waiter
// waking on its own ctx.Done and taking mu. If Close runs in that window, the
// waiter is a current waiter of Close and must get ErrClosed, not ctx.Err().
// It uses the real Close, not hand-built closed state.
func TestCloseBetweenWakeAndDecision(t *testing.T) {
	t.Run("Close fully completes in the window", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := New(2, func(lctx context.Context, _ string) ([]byte, error) {
				<-lctx.Done()
				return []byte("late"), nil // success after cancel: must be discarded
			}).(*cache)
			wctx, cancel := context.WithCancel(t.Context())
			var closeErr error
			c.afterWake = func() { closeErr = c.Close() }

			var err error
			done := make(chan struct{})
			go func() { _, err = c.Get(wctx, "k"); close(done) }()
			synctest.Wait()
			cancel()
			<-done
			if closeErr != nil {
				t.Errorf("Close = %v", closeErr)
			}
			if !errors.Is(err, ErrClosed) {
				t.Errorf("err = %v, want ErrClosed", err)
			}
			if c.Len() != 0 {
				t.Errorf("Len = %d, want 0", c.Len())
			}
		})
	})
	t.Run("Close has started but is still joining", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			c := New(2, func(_ context.Context, _ string) ([]byte, error) {
				<-release // ignore cancellation until released
				return []byte("late"), nil
			}).(*cache)
			wctx, cancel := context.WithCancel(t.Context())
			closeReturned := make(chan struct{})
			c.afterWake = func() {
				go func() { c.Close(); close(closeReturned) }()
				<-c.closing // Close has set closed; the Loader is still running
			}

			var err error
			done := make(chan struct{})
			go func() { _, err = c.Get(wctx, "k"); close(done) }()
			synctest.Wait()
			cancel()
			<-done
			if !errors.Is(err, ErrClosed) {
				t.Errorf("err = %v, want ErrClosed", err)
			}
			select {
			case <-closeReturned:
				t.Errorf("Close returned before the Loader returned")
			default:
			}
			close(release)
			<-closeReturned
			if c.Len() != 0 {
				t.Errorf("Len = %d, want 0", c.Len())
			}
		})
	})
}
