package blockcache

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func bubbleCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

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
	if l := c.Len(); l != 0 {
		t.Fatalf("Len = %d, want 0", l)
	}
	c.Get(ctx, "a")
	if got := calls.Load(); got != 2 {
		t.Fatalf("Loader calls = %d, want 2", got)
	}
}

func TestErrorFanOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		boom := errors.New("boom")
		var calls atomic.Int32
		release := make(chan struct{})
		c := New(2, func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return nil, boom
		})
		t.Cleanup(func() { c.Close() })

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
				t.Errorf("waiter %d: err = %v, want boom", i, err)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("Loader calls = %d, want 1", got)
		}
	})
}

func TestLoaderSliceNotRetained(t *testing.T) {
	ctx := testCtx(t)
	var mu sync.Mutex
	var last []byte
	c := New(2, func(context.Context, string) ([]byte, error) {
		b := []byte("orig")
		mu.Lock()
		last = b
		mu.Unlock()
		return b, nil
	})
	t.Cleanup(func() { c.Close() })

	mustGet(t, ctx, c, "a")
	mu.Lock()
	copy(last, "XXXX")
	mu.Unlock()
	if got := mustGet(t, ctx, c, "a"); string(got) != "orig" {
		t.Fatalf("Get = %q, want %q", got, "orig")
	}
}

func TestWaitersGetDistinctCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		release := make(chan struct{})
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-release
			return []byte("data"), nil
		})
		t.Cleanup(func() { c.Close() })

		const n = 5
		res := make([][]byte, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				b, err := c.Get(ctx, "k")
				if err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				b[0] = byte('0' + i)
				res[i] = b
			})
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		for i, b := range res {
			if want := string(rune('0'+i)) + "ata"; string(b) != want {
				t.Errorf("waiter %d = %q, want %q", i, b, want)
			}
		}
		if b, err := c.Get(ctx, "k"); err != nil || string(b) != "data" {
			t.Errorf("later hit = (%q, %v), want data", b, err)
		}
	})
}

func TestInflightNotCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(1, func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			if key == "b" {
				<-release
			}
			return []byte(key), nil
		})
		t.Cleanup(func() { c.Close() })

		if _, err := c.Get(ctx, "a"); err != nil {
			t.Fatalf("Get a: %v", err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := c.Get(ctx, "b"); err != nil {
				t.Errorf("Get b: %v", err)
			}
		}()
		synctest.Wait()
		if l := c.Len(); l != 1 {
			t.Errorf("Len while b in flight = %d, want 1", l)
		}
		if _, err := c.Get(ctx, "a"); err != nil {
			t.Errorf("Get a hit: %v", err)
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("calls = %d, want 2 (a must still hit)", got)
		}
		close(release)
		<-done
		if l := c.Len(); l != 1 {
			t.Errorf("Len after b = %d, want 1", l)
		}
		c.Get(ctx, "a")
		if got := calls.Load(); got != 3 {
			t.Errorf("calls = %d, want 3 (a must have been evicted)", got)
		}
	})
}

func TestLenNeverAboveCapRace(t *testing.T) {
	ctx := testCtx(t)
	const capacity = 4
	c := New(capacity, func(_ context.Context, key string) ([]byte, error) {
		return []byte(key), nil
	})
	t.Cleanup(func() { c.Close() })

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
	for g := range 8 {
		wg.Go(func() {
			for i := range 200 {
				c.Get(ctx, fmt.Sprintf("k%d-%d", g, i%32))
			}
		})
	}
	wg.Wait()
	close(stop)
	<-pollDone
}

func TestWaiterCtxCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		release := make(chan struct{})
		c := New(2, func(context.Context, string) ([]byte, error) {
			<-release
			return []byte("v"), nil
		})
		t.Cleanup(func() { c.Close() })

		cctx, cancel := context.WithCancel(ctx)
		var cErr, lErr error
		var lb []byte
		var wg sync.WaitGroup
		wg.Go(func() { _, cErr = c.Get(cctx, "k") })
		wg.Go(func() { lb, lErr = c.Get(ctx, "k") })
		synctest.Wait()
		cancel()
		synctest.Wait()
		close(release)
		wg.Wait()
		if !errors.Is(cErr, context.Canceled) {
			t.Errorf("cancelled waiter err = %v, want Canceled", cErr)
		}
		if lErr != nil || string(lb) != "v" {
			t.Errorf("live waiter = (%q, %v), want v", lb, lErr)
		}
	})
}

func TestOrphanedLoadPopulates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(2, func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		})
		t.Cleanup(func() { c.Close() })

		cctx, cancel := context.WithCancel(ctx)
		errCh := make(chan error, 1)
		go func() { _, err := c.Get(cctx, "k"); errCh <- err }()
		synctest.Wait()
		cancel()
		if err := <-errCh; !errors.Is(err, context.Canceled) {
			t.Errorf("waiter err = %v, want Canceled", err)
		}
		close(release)
		synctest.Wait()
		if l := c.Len(); l != 1 {
			t.Errorf("Len = %d, want 1", l)
		}
		if b, err := c.Get(ctx, "k"); err != nil || string(b) != "v" {
			t.Errorf("Get = (%q, %v), want v", b, err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("calls = %d, want 1", got)
		}
	})
}

func TestDeadCtxMiss(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("v"), nil
	})
	t.Cleanup(func() { c.Close() })

	dead, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Get(dead, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("miss with dead ctx: err = %v, want Canceled", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
	mustGet(t, ctx, c, "k")
	if b, err := c.Get(dead, "k"); err != nil || string(b) != "v" {
		t.Fatalf("hit with dead ctx = (%q, %v), want v", b, err)
	}
}

func TestCloseWaitsForLoader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		var flag atomic.Bool
		c := New(2, func(lctx context.Context, _ string) ([]byte, error) {
			<-lctx.Done()
			time.Sleep(time.Second)
			flag.Store(true)
			return nil, lctx.Err()
		})
		go c.Get(ctx, "k")
		synctest.Wait()
		if err := c.Close(); err != nil {
			t.Errorf("Close = %v", err)
		}
		if !flag.Load() {
			t.Errorf("Close returned before the Loader returned")
		}
	})
}

func TestCloseWakesWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		c := New(2, func(lctx context.Context, _ string) ([]byte, error) {
			<-lctx.Done()
			return nil, lctx.Err()
		})
		const n = 5
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { _, errs[i] = c.Get(ctx, "k") })
		}
		synctest.Wait()
		c.Close()
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, ErrClosed) {
				t.Errorf("waiter %d err = %v, want ErrClosed", i, err)
			}
		}
		if l := c.Len(); l != 0 {
			t.Errorf("Len = %d, want 0", l)
		}
	})
}

func TestCloseDiscardsLateResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		var calls atomic.Int32
		c := New(2, func(lctx context.Context, key string) ([]byte, error) {
			calls.Add(1)
			if key == "slow" {
				<-lctx.Done()
				time.Sleep(100 * time.Millisecond)
			}
			return []byte("v"), nil // ignores cancellation
		})
		mustGet(t, ctx, c, "fast")

		// Zero-waiter case: the only waiter leaves before Close.
		cctx, cancel := context.WithCancel(ctx)
		errCh := make(chan error, 1)
		go func() { _, err := c.Get(cctx, "slow"); errCh <- err }()
		synctest.Wait()
		cancel()
		<-errCh

		c.Close()
		if l := c.Len(); l != 0 {
			t.Errorf("Len after Close = %d, want 0", l)
		}
		before := calls.Load()
		if _, err := c.Get(ctx, "slow"); !errors.Is(err, ErrClosed) {
			t.Errorf("Get after Close err = %v, want ErrClosed", err)
		}
		if calls.Load() != before {
			t.Errorf("Loader called after Close")
		}
	})
}

func TestCloseWindowPriority(t *testing.T) {
	for _, closeAt := range []time.Duration{0, 50 * time.Millisecond} {
		t.Run(closeAt.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := New(2, func(lctx context.Context, _ string) ([]byte, error) {
					<-lctx.Done()
					time.Sleep(time.Second)
					return nil, lctx.Err()
				})
				wctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				errCh := make(chan error, 1)
				go func() { _, err := c.Get(wctx, "k"); errCh <- err }()
				synctest.Wait()
				closed := make(chan struct{})
				go func() {
					time.Sleep(closeAt)
					c.Close()
					close(closed)
				}()
				if err := <-errCh; !errors.Is(err, ErrClosed) {
					t.Errorf("waiter err = %v, want ErrClosed", err)
				}
				<-closed
			})
		})
	}
}

func TestGetDuringCloseJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		var calls atomic.Int32
		c := New(2, func(lctx context.Context, _ string) ([]byte, error) {
			calls.Add(1)
			<-lctx.Done()
			time.Sleep(time.Second)
			return nil, lctx.Err()
		})
		go c.Get(context.Background(), "k")
		synctest.Wait()
		var closeReturned atomic.Bool
		closeDone := make(chan struct{})
		go func() {
			c.Close()
			closeReturned.Store(true)
			close(closeDone)
		}()
		synctest.Wait()
		before := calls.Load()
		if _, err := c.Get(ctx, "new"); !errors.Is(err, ErrClosed) {
			t.Errorf("Get(new) err = %v, want ErrClosed", err)
		}
		if _, err := c.Get(ctx, "k"); !errors.Is(err, ErrClosed) {
			t.Errorf("Get(k) err = %v, want ErrClosed", err)
		}
		if calls.Load() != before {
			t.Errorf("Loader called during Close join")
		}
		if closeReturned.Load() {
			t.Errorf("Close returned before the Gets; join window not exercised")
		}
		<-closeDone
	})
}

func TestConcurrentClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var loaderReturned atomic.Bool
		c := New(2, func(lctx context.Context, _ string) ([]byte, error) {
			<-lctx.Done()
			time.Sleep(time.Second)
			loaderReturned.Store(true)
			return nil, lctx.Err()
		})
		go c.Get(context.Background(), "k")
		synctest.Wait()
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				if err := c.Close(); err != nil {
					t.Errorf("Close = %v", err)
				}
				if !loaderReturned.Load() {
					t.Errorf("Close returned before the Loader returned")
				}
			})
		}
		wg.Wait()
	})
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
	t.Cleanup(func() { c.Close() })

	_, err := c.Get(ctx, "a")
	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *PanicError", err)
	}
	if pe.Value != "kaboom" || pe.Key != "a" || len(pe.Stack) == 0 {
		t.Fatalf("PanicError = %+v", pe)
	}
	if l := c.Len(); l != 0 {
		t.Fatalf("Len = %d, want 0", l)
	}
	if b := mustGet(t, ctx, c, "a"); string(b) != "ok" {
		t.Fatalf("Get = %q, want ok", b)
	}
}

func TestSelfGet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		var c Cache
		var selfErr, otherErr error
		var other []byte
		c = New(4, func(lctx context.Context, key string) ([]byte, error) {
			if key == "a" {
				_, selfErr = c.Get(lctx, "a")
				other, otherErr = c.Get(lctx, "b")
			}
			return []byte(key), nil
		})
		t.Cleanup(func() { c.Close() })
		if b, err := c.Get(ctx, "a"); err != nil || string(b) != "a" {
			t.Errorf("outer Get = (%q, %v)", b, err)
		}
		if !errors.Is(selfErr, ErrSelfGet) {
			t.Errorf("self Get err = %v, want ErrSelfGet", selfErr)
		}
		if otherErr != nil || string(other) != "b" {
			t.Errorf("other Get = (%q, %v), want b", other, otherErr)
		}
	})

	t.Run("unrelatedCtx", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var c Cache
			innerErr := make(chan error, 1)
			c = New(4, func(lctx context.Context, key string) ([]byte, error) {
				_, err := c.Get(context.Background(), key)
				innerErr <- err
				return nil, err
			})
			go c.Get(context.Background(), "a")
			synctest.Wait()
			if err := c.Close(); err != nil {
				t.Errorf("Close = %v", err)
			}
			if err := <-innerErr; !errors.Is(err, ErrClosed) {
				t.Errorf("inner Get err = %v, want ErrClosed", err)
			}
		})
	})
}

func TestStress(t *testing.T) {
	ctx := testCtx(t)
	c := New(3, func(lctx context.Context, key string) ([]byte, error) {
		if rand.IntN(5) == 0 {
			return nil, errors.New("transient")
		}
		select {
		case <-time.After(time.Duration(rand.IntN(100)) * time.Microsecond):
		case <-lctx.Done():
			return nil, lctx.Err()
		}
		return []byte(key), nil
	})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 300 {
				key := fmt.Sprintf("k%d", rand.IntN(8))
				gctx, cancel := context.WithTimeout(ctx, time.Duration(rand.IntN(200))*time.Microsecond)
				b, err := c.Get(gctx, key)
				cancel()
				if err == nil && string(b) != key {
					t.Errorf("Get(%s) = %q", key, b)
				}
				if l := c.Len(); l > 3 {
					t.Errorf("Len = %d > 3", l)
				}
			}
		})
	}
	time.Sleep(5 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	wg.Wait()
	if l := c.Len(); l != 0 {
		t.Errorf("Len after Close = %d, want 0", l)
	}
}
