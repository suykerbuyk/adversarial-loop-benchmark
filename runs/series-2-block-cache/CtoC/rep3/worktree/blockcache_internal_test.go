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

func bubbleCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// 1. The Loader's slice is not retained.
func TestLoaderSliceNotRetained(t *testing.T) {
	ctx := testCtx(t)
	buf := []byte("orig")
	c := New(2, func(context.Context, string) ([]byte, error) { return buf, nil })
	t.Cleanup(func() { c.Close() })
	mustGet(t, ctx, c, "k")
	copy(buf, "XXXX")
	if got := mustGet(t, ctx, c, "k"); string(got) != "orig" {
		t.Fatalf("hit = %q, want %q", got, "orig")
	}
}

// 2. Per-waiter copies under single flight.
func TestPerWaiterCopies(t *testing.T) {
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
			wg.Go(func() { res[i], _ = c.Get(ctx, "k") })
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		for i := range n {
			if string(res[i]) != "data" {
				t.Fatalf("res[%d] = %q", i, res[i])
			}
			for j := range i {
				if &res[i][0] == &res[j][0] {
					t.Fatalf("res[%d] and res[%d] share a backing array", i, j)
				}
			}
		}
		res[0][0] = 'X'
		if string(res[1]) != "data" {
			t.Fatalf("mutation leaked to another waiter")
		}
		if got := mustGet(t, ctx, c, "k"); string(got) != "data" {
			t.Fatalf("hit = %q after mutation", got)
		}
	})
}

// 3. (bytes, err): error wins.
func TestBytesAndErrorErrorWins(t *testing.T) {
	ctx := testCtx(t)
	boom := errors.New("boom")
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("junk"), boom
	})
	t.Cleanup(func() { c.Close() })
	b, err := c.Get(ctx, "k")
	if !errors.Is(err, boom) || b != nil {
		t.Fatalf("Get = %q, %v; want nil, boom", b, err)
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
		ctx := bubbleCtx(t)
		boom := errors.New("boom")
		release := make(chan struct{})
		var calls atomic.Int32
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
				t.Fatalf("errs[%d] = %v", i, err)
			}
		}
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1", calls.Load())
		}
	})
}

// 5. (nil, nil) stores an empty block.
func TestNilNil(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) { calls.Add(1); return nil, nil })
	t.Cleanup(func() { c.Close() })
	for range 2 {
		b, err := c.Get(ctx, "k")
		if err != nil || b == nil || len(b) != 0 {
			t.Fatalf("Get = %#v, %v; want []byte{}, nil", b, err)
		}
	}
	if c.Len() != 1 || calls.Load() != 1 {
		t.Fatalf("Len = %d, calls = %d; want 1, 1", c.Len(), calls.Load())
	}
}

// A panic with an error value is reachable via errors.Is, and the stack is kept.
func TestLoaderPanicErrorValue(t *testing.T) {
	boom := errors.New("boom")
	c := New(2, func(context.Context, string) ([]byte, error) { panic(boom) })
	t.Cleanup(func() { c.Close() })
	_, err := c.Get(testCtx(t), "k")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapping boom", err)
	}
	if !strings.Contains(err.Error(), "goroutine") {
		t.Fatalf("err lacks a stack trace: %v", err)
	}
}

// 6. Loader panic.
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
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v, want panicked error", err)
	}
	if got := mustGet(t, ctx, c, "k"); string(got) != "ok" || calls.Load() != 2 {
		t.Fatalf("reload = %q, calls = %d", got, calls.Load())
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

// 7. Loader Goexit.
func TestLoaderGoexit(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) {
		if calls.Add(1) == 1 {
			runtime.Goexit()
		}
		return []byte("ok"), nil
	})
	t.Cleanup(func() { c.Close() })
	if _, err := c.Get(ctx, "k"); !errors.Is(err, errLoaderExited) {
		t.Fatalf("err = %v, want errLoaderExited", err)
	}
	if got := mustGet(t, ctx, c, "k"); string(got) != "ok" || calls.Load() != 2 {
		t.Fatalf("reload = %q, calls = %d", got, calls.Load())
	}
}

// 8. An orphaned load populates the cache.
func TestOrphanedLoadPopulates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(2, func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		})
		t.Cleanup(func() { c.Close() })
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
		if c.Len() != 1 {
			t.Fatalf("Len = %d, want 1", c.Len())
		}
		mustGet(t, bubbleCtx(t), c, "k")
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1", calls.Load())
		}
	})
}

// 9. A pre-cancelled miss still loads once and warms the cache.
func TestPreCancelledMissWarms(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		c := New(2, func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		})
		t.Cleanup(func() { c.Close() })
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := c.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want Canceled", err)
		}
		close(release)
		synctest.Wait()
		mustGet(t, bubbleCtx(t), c, "k")
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1", calls.Load())
		}
	})
}

// 10, 11. Readiness ties in wait, settled by priority.
func TestWaitPriorityTies(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	resolved := func() *call {
		cl := &call{done: make(chan struct{}), val: []byte("v")}
		close(cl.done)
		return cl
	}
	newClosed := func() *cache {
		c := New(1, func(context.Context, string) ([]byte, error) { return nil, nil }).(*cache)
		c.Close()
		return c
	}
	for range 1000 {
		// closedCh + done + ctx, closed: ErrClosed.
		if _, err := newClosed().wait(cancelled, resolved()); !errors.Is(err, ErrClosed) {
			t.Fatalf("all ready: err = %v, want ErrClosed", err)
		}
		// done + ctx, not closing: result.
		c := New(1, func(context.Context, string) ([]byte, error) { return nil, nil }).(*cache)
		if b, err := c.wait(cancelled, resolved()); err != nil || string(b) != "v" {
			t.Fatalf("done+ctx: %q, %v; want v, nil", b, err)
		}
		// closedCh + ctx only: ErrClosed.
		pending := &call{done: make(chan struct{})}
		if _, err := newClosed().wait(cancelled, pending); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed+ctx: err = %v, want ErrClosed", err)
		}
		// ctx only, not closing: ctx.Err().
		if _, err := c.wait(cancelled, pending); !errors.Is(err, context.Canceled) {
			t.Fatalf("ctx only: err = %v, want Canceled", err)
		}
	}
}

// blockingCache returns a cache whose Loader ignores ctx and blocks on release.
func blockingCache(capacity int, release <-chan struct{}, calls *atomic.Int32) Cache {
	return New(capacity, func(_ context.Context, key string) ([]byte, error) {
		calls.Add(1)
		<-release
		return []byte("v:" + key), nil
	})
}

// 12, 16. (a) over (c), and close-window discard.
func TestCloseBeatsSuccessfulLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		release := make(chan struct{})
		var calls atomic.Int32
		c := blockingCache(2, release, &calls)
		errc := make(chan error, 1)
		go func() { _, err := c.Get(ctx, "k"); errc <- err }()
		synctest.Wait()
		closeErr := make(chan error, 1)
		go func() { closeErr <- c.Close() }()
		synctest.Wait()
		// 19. A Get during the join returns ErrClosed without a Loader call.
		if _, err := c.Get(ctx, "other"); !errors.Is(err, ErrClosed) {
			t.Errorf("Get during join = %v, want ErrClosed", err)
		}
		close(release)
		synctest.Wait()
		if err := <-closeErr; err != nil {
			t.Fatalf("Close = %v", err)
		}
		if err := <-errc; !errors.Is(err, ErrClosed) {
			t.Fatalf("waiter = %v, want ErrClosed", err)
		}
		if c.Len() != 0 {
			t.Fatalf("Len = %d, want 0", c.Len())
		}
		if _, err := c.Get(ctx, "k"); !errors.Is(err, ErrClosed) {
			t.Fatalf("Get after Close = %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1", calls.Load())
		}
	})
}

// 13. (a) over (b): a waiter whose ctx expires during the join gets ErrClosed,
// and gets it at once rather than after the (possibly slow) join.
func TestCloseBeatsCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		c := blockingCache(2, release, &calls)
		ctx, cancel := context.WithCancel(t.Context())
		var returned atomic.Bool
		errc := make(chan error, 1)
		go func() { _, err := c.Get(ctx, "k"); returned.Store(true); errc <- err }()
		synctest.Wait()
		done := make(chan struct{})
		go func() { c.Close(); close(done) }()
		synctest.Wait()
		if returned.Load() {
			t.Errorf("waiter with a live ctx returned before the join finished")
		}
		cancel()
		synctest.Wait()
		if !returned.Load() {
			t.Errorf("waiter with a Done ctx still parked in the join")
		}
		if err := <-errc; !errors.Is(err, ErrClosed) {
			t.Errorf("waiter = %v, want ErrClosed", err)
		}
		close(release)
		<-done
	})
}

// A waiter on a key whose Loader already returned during Close still waits for
// the join of Loaders for other keys before getting ErrClosed.
func TestJoinBeforeWakeAcrossKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		release := make(chan struct{})
		var slowDone atomic.Bool
		c := New(2, func(lctx context.Context, key string) ([]byte, error) {
			<-lctx.Done()
			if key == "slow" {
				<-release
				slowDone.Store(true)
			}
			return nil, lctx.Err()
		})
		go c.Get(ctx, "slow")
		var returned atomic.Bool
		errc := make(chan error, 1)
		go func() { _, err := c.Get(ctx, "fast"); returned.Store(true); errc <- err }()
		synctest.Wait()
		done := make(chan struct{})
		go func() { c.Close(); close(done) }()
		synctest.Wait()
		if returned.Load() {
			t.Errorf("fast waiter woken before the slow Loader returned")
		}
		close(release)
		<-done
		if err := <-errc; !errors.Is(err, ErrClosed) || !slowDone.Load() {
			t.Fatalf("waiter = %v, slowDone = %v", err, slowDone.Load())
		}
	})
}

// A Loader that Gets another in-flight key with its own ctx must not deadlock
// Close (review finding r1-major).
func TestNestedGetDoesNotDeadlockClose(t *testing.T) {
	for _, derived := range []bool{false, true} {
		t.Run(fmt.Sprint("derived=", derived), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := bubbleCtx(t)
				var c Cache
				innerStarted := make(chan struct{})
				c = New(4, func(lctx context.Context, key string) ([]byte, error) {
					if key == "inner" {
						close(innerStarted)
						<-lctx.Done()
						return nil, lctx.Err()
					}
					go c.Get(ctx, "inner") // make "inner" in flight first
					<-innerStarted
					gctx := lctx
					if derived {
						var cancel context.CancelFunc
						gctx, cancel = context.WithTimeout(lctx, time.Hour)
						defer cancel()
					}
					b, err := c.Get(gctx, "inner")
					if !errors.Is(err, ErrClosed) {
						t.Errorf("nested Get = %q, %v; want ErrClosed", b, err)
					}
					return b, err
				})
				errc := make(chan error, 1)
				go func() { _, err := c.Get(ctx, "outer"); errc <- err }()
				synctest.Wait()
				if err := c.Close(); err != nil {
					t.Fatalf("Close = %v", err)
				}
				if err := <-errc; !errors.Is(err, ErrClosed) {
					t.Fatalf("outer waiter = %v, want ErrClosed", err)
				}
				if c.Len() != 0 {
					t.Fatalf("Len = %d, want 0", c.Len())
				}
			})
		})
	}
}

// 14. Waiters are woken only after the Loader has returned.
func TestJoinBeforeWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		var flag atomic.Bool
		var sawErr atomic.Value
		c := New(2, func(lctx context.Context, _ string) ([]byte, error) {
			<-lctx.Done()
			sawErr.Store(lctx.Err())
			time.Sleep(10 * time.Millisecond)
			flag.Store(true)
			return []byte("late"), nil
		})
		res := make(chan bool, 1)
		go func() {
			_, err := c.Get(ctx, "k")
			if !errors.Is(err, ErrClosed) {
				t.Errorf("waiter = %v, want ErrClosed", err)
			}
			res <- flag.Load()
		}()
		synctest.Wait()
		if err := c.Close(); err != nil {
			t.Fatalf("Close = %v", err)
		}
		if !<-res {
			t.Fatalf("waiter woken before Loader returned")
		}
		if !flag.Load() || sawErr.Load() != context.Canceled {
			t.Fatalf("flag = %v, loader ctx err = %v", flag.Load(), sawErr.Load())
		}
	})
}

// 15. Close wakes all waiters across keys.
func TestCloseWakesAll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		c := New(4, func(lctx context.Context, _ string) ([]byte, error) {
			<-lctx.Done()
			return nil, lctx.Err()
		})
		const n = 12
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { _, errs[i] = c.Get(ctx, fmt.Sprint(i%3)) })
		}
		synctest.Wait()
		c.Close()
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("errs[%d] = %v", i, err)
			}
		}
	})
}

// 17. A zero-waiter load finishing during the join is discarded.
func TestZeroWaiterDiscard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		c := blockingCache(2, release, &calls)
		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() { _, err := c.Get(ctx, "k"); errc <- err }()
		synctest.Wait()
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Errorf("waiter = %v, want Canceled", err)
		}
		done := make(chan struct{})
		go func() { c.Close(); close(done) }()
		synctest.Wait()
		close(release)
		<-done
		if c.Len() != 0 {
			t.Fatalf("Len = %d, want 0", c.Len())
		}
	})
}

// 18. Concurrent Closes share the join.
func TestConcurrentCloseSharesJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		release := make(chan struct{})
		var calls atomic.Int32
		c := blockingCache(2, release, &calls)
		go c.Get(ctx, "k")
		synctest.Wait()
		var r1, r2 atomic.Bool
		var wg sync.WaitGroup
		wg.Go(func() {
			if c.Close() != nil {
				t.Errorf("Close 1 non-nil")
			}
			r1.Store(true)
		})
		wg.Go(func() {
			if c.Close() != nil {
				t.Errorf("Close 2 non-nil")
			}
			r2.Store(true)
		})
		synctest.Wait()
		if r1.Load() || r2.Load() {
			t.Errorf("a Close returned before the join")
		}
		close(release)
		wg.Wait()
		if err := c.Close(); err != nil {
			t.Fatalf("third Close = %v", err)
		}
	})
}

// 19. Get after Close calls no Loader.
func TestGetAfterCloseNoLoad(t *testing.T) {
	var calls atomic.Int32
	c := New(2, func(context.Context, string) ([]byte, error) { calls.Add(1); return nil, nil })
	c.Close()
	if _, err := c.Get(testCtx(t), "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0", calls.Load())
	}
}

// 20. An in-flight load is not counted and not evictable.
func TestInflightNotCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := bubbleCtx(t)
		release := make(chan struct{})
		c := New(1, func(_ context.Context, key string) ([]byte, error) {
			if key == "b" {
				<-release
			}
			return []byte(key), nil
		})
		t.Cleanup(func() { c.Close() })
		mustGet(t, ctx, c, "a")
		done := make(chan struct{})
		go func() { c.Get(ctx, "b"); close(done) }()
		synctest.Wait()
		if c.Len() != 1 {
			t.Errorf("Len = %d, want 1", c.Len())
		}
		if b, err := c.Get(ctx, "a"); err != nil || string(b) != "a" {
			t.Errorf("hit a = %q, %v", b, err)
		}
		close(release)
		<-done
		ci := c.(*cache)
		ci.mu.Lock()
		_, hasA := ci.items["a"]
		_, hasB := ci.items["b"]
		ci.mu.Unlock()
		if hasA || !hasB {
			t.Fatalf("hasA = %v, hasB = %v; want false, true", hasA, hasB)
		}
	})
}

// 21. A successful load marks the key MRU.
func TestRecencyOnLoad(t *testing.T) {
	ctx := testCtx(t)
	var mu sync.Mutex
	counts := map[string]int{}
	c := New(2, func(_ context.Context, key string) ([]byte, error) {
		mu.Lock()
		counts[key]++
		mu.Unlock()
		return []byte(key), nil
	})
	t.Cleanup(func() { c.Close() })
	for _, k := range []string{"a", "b", "c", "b", "a"} {
		mustGet(t, ctx, c, k)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["a"] != 2 || counts["b"] != 1 {
		t.Fatalf("counts = %v; want a:2 b:1", counts)
	}
}

// 22. Len stays bounded under a concurrent poll.
func TestLenUnderConcurrentPoll(t *testing.T) {
	ctx := testCtx(t)
	c := New(8, func(_ context.Context, key string) ([]byte, error) { return []byte(key), nil })
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
			if l := c.Len(); l > 8 {
				t.Errorf("Len = %d > 8", l)
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 200 {
				if _, err := c.Get(ctx, fmt.Sprint(rand.IntN(100))); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	<-pollDone
}

// 23. Stress with a Close in the middle.
func TestStressWithClose(t *testing.T) {
	ctx := testCtx(t)
	c := New(8, func(lctx context.Context, key string) ([]byte, error) {
		select {
		case <-time.After(time.Duration(rand.IntN(200)) * time.Microsecond):
			return []byte("v:" + key), nil
		case <-lctx.Done():
			return nil, lctx.Err()
		}
	})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 100 {
				key := fmt.Sprint(rand.IntN(20))
				b, err := c.Get(ctx, key)
				switch {
				case err == nil:
					if string(b) != "v:"+key {
						t.Errorf("Get(%s) = %q", key, b)
					}
				case errors.Is(err, ErrClosed):
				default:
					t.Errorf("unexpected err %v", err)
				}
			}
		})
	}
	time.Sleep(2 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	wg.Wait()
	if c.Len() != 0 {
		t.Fatalf("Len = %d after Close", c.Len())
	}
}

// 24. Panics in New and Get.
func TestMorePanics(t *testing.T) {
	load := func(context.Context, string) ([]byte, error) { return []byte("v"), nil }
	assertPanics(t, "New(-1, load)", func() { New(-1, load) })
	c := New(2, load)
	t.Cleanup(func() { c.Close() })
	var nilCtx context.Context
	assertPanics(t, "Get(nil) miss", func() { c.Get(nilCtx, "k") })
	mustGet(t, testCtx(t), c, "k")
	assertPanics(t, "Get(nil) hit", func() { c.Get(nilCtx, "k") })
}
