package blockcache

import (
	"bytes"
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

// blockingLoader returns a Loader that blocks until release is closed (or its
// ctx is Done, in which case it returns ctx.Err()).
func blockingLoader(calls *atomic.Int32, release <-chan struct{}) Loader {
	return func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		select {
		case <-release:
			return []byte("v:" + key), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func TestPerWaiterCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		var calls atomic.Int32
		release := make(chan struct{})
		c := New(4, blockingLoader(&calls, release))
		defer c.Close()

		const n = 10
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
			if string(res[i]) != "v:k" {
				t.Errorf("res[%d] = %q, want v:k", i, res[i])
			}
		}
		if b, _ := c.Get(ctx, "k"); string(b) != "v:k" {
			t.Errorf("hit = %q, want v:k", b)
		}
	})
}

func TestLoaderSliceNotRetained(t *testing.T) {
	var kept []byte
	c := New(2, func(_ context.Context, _ string) ([]byte, error) {
		kept = []byte("orig")
		return kept, nil
	})
	defer c.Close()
	if _, err := c.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	copy(kept, "XXXX")
	if b, _ := c.Get(context.Background(), "a"); string(b) != "orig" {
		t.Fatalf("got %q, want orig", b)
	}
}

func TestErrorWithBytesAndFanOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("boom")
		var calls atomic.Int32
		release := make(chan struct{})
		c := New(2, func(_ context.Context, _ string) ([]byte, error) {
			if calls.Add(1) == 1 {
				<-release
				return []byte("junk"), boom
			}
			return []byte("ok"), nil
		})
		defer c.Close()

		const n = 5
		var wg sync.WaitGroup
		errs := make([]error, n)
		bs := make([][]byte, n)
		for i := range n {
			wg.Go(func() { bs[i], errs[i] = c.Get(t.Context(), "k") })
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		for i := range n {
			if !errors.Is(errs[i], boom) || bs[i] != nil {
				t.Errorf("waiter %d: (%q, %v), want (nil, boom)", i, bs[i], errs[i])
			}
		}
		if c.Len() != 0 {
			t.Errorf("Len = %d, want 0", c.Len())
		}
		if b, err := c.Get(t.Context(), "k"); err != nil || string(b) != "ok" {
			t.Errorf("reload = (%q, %v)", b, err)
		}
		if calls.Load() != 2 {
			t.Errorf("calls = %d, want 2", calls.Load())
		}
	})
}

func TestCallerCancelOrphanLoadPopulates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		var loaderCtxDone atomic.Bool
		release := make(chan struct{})
		c := New(2, func(ctx context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			loaderCtxDone.Store(ctx.Err() != nil)
			return []byte("v"), nil
		})
		defer c.Close()

		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() { _, err := c.Get(ctx, "k"); errc <- err }()
		synctest.Wait()
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Errorf("waiter err = %v, want Canceled", err)
		}
		close(release)
		synctest.Wait()
		if loaderCtxDone.Load() {
			t.Errorf("loader ctx was cancelled by caller")
		}
		if c.Len() != 1 {
			t.Errorf("Len = %d, want 1", c.Len())
		}
		if b, err := c.Get(t.Context(), "k"); err != nil || string(b) != "v" {
			t.Errorf("hit = (%q, %v)", b, err)
		}
		if calls.Load() != 1 {
			t.Errorf("calls = %d, want 1", calls.Load())
		}
	})
}

func TestResolvedBeatsCtx(t *testing.T) {
	c := New(2, func(_ context.Context, _ string) ([]byte, error) { return []byte("v"), nil }).(*cache)
	defer c.Close()
	cl := &call{done: make(chan struct{}), resolved: true, val: []byte("v")}
	close(cl.done)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if b, err := c.wait(ctx, cl); err != nil || string(b) != "v" {
		t.Fatalf("wait = (%q, %v), want (v, nil)", b, err)
	}
}

func TestClosedBeatsEverything(t *testing.T) {
	c := New(2, func(_ context.Context, _ string) ([]byte, error) { return []byte("v"), nil }).(*cache)
	cl := &call{done: make(chan struct{}), resolved: true, val: []byte("v")}
	close(cl.done)
	c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.wait(ctx, cl); !errors.Is(err, ErrClosed) {
		t.Fatalf("resolved: wait err = %v, want ErrClosed", err)
	}
	// Unresolved load, own ctx Done, cache closed: ErrClosed still wins.
	pending := &call{done: make(chan struct{})}
	if _, err := c.wait(ctx, pending); !errors.Is(err, ErrClosed) {
		t.Fatalf("unresolved: wait err = %v, want ErrClosed", err)
	}
}

func TestCloseJoinsLoadersAndWakesWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var returned atomic.Bool
		c := New(2, func(ctx context.Context, _ string) ([]byte, error) {
			<-ctx.Done()
			time.Sleep(10 * time.Millisecond)
			returned.Store(true)
			return []byte("late"), nil // success after cancel: must be discarded
		})
		const n = 4
		var wg sync.WaitGroup
		errs := make([]error, n)
		cancels := make([]context.CancelFunc, n)
		defer func() {
			for _, cancel := range cancels {
				cancel()
			}
		}()
		for i := range n {
			ctx, cancel := context.WithCancel(t.Context())
			cancels[i] = cancel
			wg.Go(func() { _, errs[i] = c.Get(ctx, fmt.Sprint("k", i%2)) })
		}
		synctest.Wait()

		closeDone := make(chan struct{}, 2)
		go func() { c.Close(); closeDone <- struct{}{} }()
		go func() { c.Close(); closeDone <- struct{}{} }()
		wg.Wait()
		for i := range n {
			if !errors.Is(errs[i], ErrClosed) {
				t.Errorf("waiter %d err = %v, want ErrClosed", i, errs[i])
			}
		}
		<-closeDone
		<-closeDone
		if !returned.Load() {
			t.Errorf("Close returned before Loader returned")
		}
		if c.Len() != 0 {
			t.Errorf("Len = %d, want 0", c.Len())
		}
		if err := c.Close(); err != nil {
			t.Errorf("third Close = %v", err)
		}
	})
}

func TestCloseWaitsForOrphan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var returned atomic.Bool
		c := New(2, func(ctx context.Context, _ string) ([]byte, error) {
			<-ctx.Done()
			time.Sleep(time.Second)
			returned.Store(true)
			return []byte("v"), nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := c.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want Canceled", err)
		}
		c.Close()
		if !returned.Load() {
			t.Errorf("Close returned before orphan Loader returned")
		}
		if c.Len() != 0 {
			t.Errorf("Len = %d, want 0", c.Len())
		}
	})
}

func TestGetAfterCloseNoLoad(t *testing.T) {
	var calls atomic.Int32
	c := New(2, func(_ context.Context, _ string) ([]byte, error) {
		calls.Add(1)
		return []byte("v"), nil
	})
	if _, err := c.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	for _, k := range []string{"a", "b"} {
		if _, err := c.Get(context.Background(), k); !errors.Is(err, ErrClosed) {
			t.Errorf("Get(%s) err = %v", k, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestInflightNotCountedOrEvictable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		c := New(1, func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			if key == "b" {
				<-release
			}
			return []byte(key), nil
		})
		defer c.Close()
		ctx := t.Context()
		mustGet(t, ctx, c, "a")
		done := make(chan struct{})
		go func() { mustGet(t, ctx, c, "b"); close(done) }()
		synctest.Wait()
		if c.Len() != 1 {
			t.Errorf("Len = %d, want 1", c.Len())
		}
		if b := mustGet(t, ctx, c, "a"); !bytes.Equal(b, []byte("a")) || calls.Load() != 2 {
			t.Errorf("a not a hit while b in flight")
		}
		close(release)
		<-done
		if c.Len() != 1 {
			t.Errorf("Len = %d, want 1", c.Len())
		}
		mustGet(t, ctx, c, "a")
		if calls.Load() != 3 {
			t.Errorf("calls = %d, want 3 (a should have been evicted)", calls.Load())
		}
	})
}

func TestReentrantGet(t *testing.T) {
	var c Cache
	c = New(4, func(ctx context.Context, key string) ([]byte, error) {
		if key == "outer" {
			b, err := c.Get(ctx, "inner")
			if err != nil {
				return nil, err
			}
			return append([]byte("o+"), b...), nil
		}
		return []byte(key), nil
	})
	defer c.Close()
	if b := mustGet(t, context.Background(), c, "outer"); string(b) != "o+inner" {
		t.Errorf("outer = %q", b)
	}
}

func TestReentrantInnerReleasedByClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c Cache
		innerErr := make(chan error, 1)
		c = New(4, func(ctx context.Context, key string) ([]byte, error) {
			if key == "outer" {
				_, err := c.Get(context.Background(), "inner")
				innerErr <- err
				return nil, err
			}
			<-ctx.Done()
			return nil, ctx.Err()
		})
		go c.Get(t.Context(), "outer")
		synctest.Wait()
		c.Close()
		if err := <-innerErr; !errors.Is(err, ErrClosed) {
			t.Errorf("inner err = %v, want ErrClosed", err)
		}
	})
}

func TestLenPollAndStress(t *testing.T) {
	const capacity = 8
	c := New(capacity, func(ctx context.Context, key string) ([]byte, error) {
		select {
		case <-time.After(time.Duration(rand.IntN(200)) * time.Microsecond):
		case <-ctx.Done():
		}
		if rand.IntN(10) == 0 {
			return nil, errors.New("flaky")
		}
		return []byte(key), nil
	})
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
	for range 32 {
		wg.Go(func() {
			for range 200 {
				ctx, cancel := context.WithTimeout(context.Background(),
					time.Duration(rand.IntN(300))*time.Microsecond)
				key := fmt.Sprint("k", rand.IntN(20))
				b, err := c.Get(ctx, key)
				cancel()
				if err == nil && string(b) != key {
					t.Errorf("Get(%s) = %q", key, b)
				}
			}
		})
	}
	wg.Go(func() {
		time.Sleep(20 * time.Millisecond)
		c.Close()
	})
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stress did not finish")
	}
	close(stop)
	poll.Wait()
	c.Close()
	if c.Len() != 0 {
		t.Errorf("Len after Close = %d", c.Len())
	}
}
