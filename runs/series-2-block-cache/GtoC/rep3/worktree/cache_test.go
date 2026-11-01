package blockcache

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func implCtx(t *testing.T) context.Context {
	t.Helper()
	return t.Context()
}

func implMustGet(t *testing.T, ctx context.Context, c Cache, key string) []byte {
	t.Helper()
	b, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get(%q): unexpected error %v", key, err)
	}
	return b
}

// TestImpl_StarterCancelWhileLoading: the starter returns ctx.Err() while the
// Loader is still blocked, then a later Get hits without a second load.
func TestImpl_StarterCancelWhileLoading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := implCtx(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		var calls atomic.Int32
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			close(entered)
			<-release
			return []byte("payload"), nil
		}
		c := New(4, load)
		t.Cleanup(func() { c.Close() })

		caller, cancel := context.WithCancel(ctx)
		var got []byte
		var gotErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			got, gotErr = c.Get(caller, "a")
		}()

		<-entered
		cancel()
		<-done
		if !errors.Is(gotErr, context.Canceled) {
			t.Errorf("starter Get error = %v, want context.Canceled", gotErr)
		}
		if got != nil {
			t.Errorf("starter Get bytes = %q, want nil", got)
		}
		if c.Len() != 0 {
			t.Errorf("Len while loading = %d, want 0", c.Len())
		}

		close(release)
		synctest.Wait()

		b := implMustGet(t, ctx, c, "a")
		if string(b) != "payload" {
			t.Errorf("later Get = %q, want payload", b)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("Loader called %d times, want 1", got)
		}
	})
}

// TestImpl_FollowerSurvivesStarterCancel: cancelling the starter does not
// cancel the flight; the follower receives the success bytes.
func TestImpl_FollowerSurvivesStarterCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := implCtx(t)
		release := make(chan struct{})
		var calls atomic.Int32
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("shared"), nil
		}
		c := New(4, load)
		t.Cleanup(func() { c.Close() })

		starterCtx, cancel := context.WithCancel(ctx)
		var starterErr error
		starterDone := make(chan struct{})
		go func() {
			defer close(starterDone)
			_, starterErr = c.Get(starterCtx, "k")
		}()

		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Errorf("Loader entered %d times before follower, want 1", got)
		}

		var follower []byte
		var followerErr error
		followerDone := make(chan struct{})
		go func() {
			defer close(followerDone)
			follower, followerErr = c.Get(ctx, "k")
		}()
		synctest.Wait()

		cancel()
		<-starterDone
		if !errors.Is(starterErr, context.Canceled) {
			t.Errorf("starter error = %v, want context.Canceled", starterErr)
		}

		close(release)
		<-followerDone
		synctest.Wait()
		if followerErr != nil {
			t.Fatalf("follower error = %v, want nil", followerErr)
		}
		if string(follower) != "shared" {
			t.Fatalf("follower = %q, want shared", follower)
		}

		hit := implMustGet(t, ctx, c, "k")
		if string(hit) != "shared" {
			t.Fatalf("third Get = %q, want shared", hit)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader called %d times, want 1", got)
		}
	})
}

// TestImpl_AllWaitersCancelOrphanPopulates: an orphaned load still fills the cache.
func TestImpl_AllWaitersCancelOrphanPopulates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := implCtx(t)
		release := make(chan struct{})
		var calls atomic.Int32
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("orphan"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		const n = 4
		cancels := make([]context.CancelFunc, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			caller, cancel := context.WithCancel(ctx)
			cancels[i] = cancel
			go func(i int) {
				defer wg.Done()
				_, errs[i] = c.Get(caller, "o")
			}(i)
		}
		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Errorf("Loader entered %d times, want 1", got)
		}
		for i := 0; i < n; i++ {
			cancels[i]()
		}
		wg.Wait()
		for i := 0; i < n; i++ {
			if !errors.Is(errs[i], context.Canceled) {
				t.Errorf("waiter[%d] error = %v, want context.Canceled", i, errs[i])
			}
		}

		close(release)
		synctest.Wait()
		b := implMustGet(t, ctx, c, "o")
		if string(b) != "orphan" {
			t.Fatalf("later Get = %q, want orphan", b)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader called %d times, want 1", got)
		}
	})
}

// TestImpl_CloseWhileLoading: Close drops entries before the Loader returns,
// waiters see ErrClosed, and the success result is discarded.
func TestImpl_CloseWhileLoading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := implCtx(t)
		var returned atomic.Bool
		sawCancel := make(chan struct{})
		releaseAfterObserve := make(chan struct{})
		load := func(lctx context.Context, key string) ([]byte, error) {
			<-lctx.Done()
			close(sawCancel)
			// Stay blocked until the test has observed Len == 0 and a rejected Get.
			<-releaseAfterObserve
			returned.Store(true)
			return []byte("discarded"), nil
		}
		c := New(4, load)
		t.Cleanup(func() { c.Close() })

		var gotErr error
		waiterDone := make(chan struct{})
		go func() {
			defer close(waiterDone)
			_, gotErr = c.Get(ctx, "a")
		}()
		synctest.Wait()

		closeDone := make(chan error, 1)
		go func() {
			closeDone <- c.Close()
		}()

		<-sawCancel
		if l := c.Len(); l != 0 {
			t.Errorf("Len during close = %d, want 0", l)
		}
		if _, err := c.Get(ctx, "fresh"); !errors.Is(err, ErrClosed) {
			t.Errorf("Get during close = %v, want ErrClosed", err)
		}
		if returned.Load() {
			t.Errorf("Loader returned before the mid-close checks")
		}

		close(releaseAfterObserve)
		<-waiterDone
		if !errors.Is(gotErr, ErrClosed) {
			t.Errorf("waiter error = %v, want ErrClosed", gotErr)
		}
		if err := <-closeDone; err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
		if !returned.Load() {
			t.Fatalf("Close returned before Loader returned")
		}
		if l := c.Len(); l != 0 {
			t.Fatalf("Len after Close = %d, want 0", l)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("second Close = %v, want nil", err)
		}
	})
}

// TestImpl_ConcurrentClose: several Close callers all return nil.
func TestImpl_ConcurrentClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := implCtx(t)
		load := func(_ context.Context, key string) ([]byte, error) {
			return []byte("v"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })
		implMustGet(t, ctx, c, "a")

		const n = 8
		errs := make([]error, n)
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				errs[i] = c.Close()
			}(i)
		}
		wg.Wait()
		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Errorf("Close[%d] = %v, want nil", i, errs[i])
			}
		}
		if _, err := c.Get(ctx, "a"); !errors.Is(err, ErrClosed) {
			t.Fatalf("Get after Close = %v, want ErrClosed", err)
		}
	})
}

// TestImpl_SecondCloseDuringJoin: a second Close waits for the same join.
func TestImpl_SecondCloseDuringJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := implCtx(t)
		entered := make(chan struct{})
		hold := make(chan struct{})
		load := func(lctx context.Context, key string) ([]byte, error) {
			close(entered)
			<-lctx.Done()
			<-hold
			return []byte("late"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		go func() {
			_, _ = c.Get(ctx, "k")
		}()
		<-entered

		firstDone := make(chan error, 1)
		go func() {
			firstDone <- c.Close()
		}()
		// First Close has cancelled and is joining; the Loader is past
		// ctx.Done and parked on hold, so the join is still in progress.
		synctest.Wait()

		secondDone := make(chan error, 1)
		go func() {
			secondDone <- c.Close()
		}()
		synctest.Wait()
		select {
		case err := <-secondDone:
			t.Errorf("second Close returned before the join finished: %v", err)
		default:
		}
		select {
		case err := <-firstDone:
			t.Errorf("first Close returned before the Loader was released: %v", err)
		default:
		}

		close(hold)
		if err := <-firstDone; err != nil {
			t.Fatalf("first Close = %v, want nil", err)
		}
		if err := <-secondDone; err != nil {
			t.Fatalf("second Close = %v, want nil", err)
		}
		if _, err := c.Get(ctx, "k"); !errors.Is(err, ErrClosed) {
			t.Fatalf("Get after Close = %v, want ErrClosed", err)
		}
	})
}

// TestImpl_ErrorWithBytes: accompanying bytes are discarded; a later Get reloads.
func TestImpl_ErrorWithBytes(t *testing.T) {
	ctx := implCtx(t)
	boom := errors.New("nope")
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		n := calls.Add(1)
		if n == 1 {
			return []byte("nope"), boom
		}
		return []byte("ok"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	b, err := c.Get(ctx, "a")
	if !errors.Is(err, boom) {
		t.Fatalf("first error = %v, want %v", err, boom)
	}
	if b != nil {
		t.Fatalf("first bytes = %q, want nil", b)
	}
	if c.Len() != 0 {
		t.Fatalf("Len after error = %d, want 0", c.Len())
	}
	b = implMustGet(t, ctx, c, "a")
	if string(b) != "ok" {
		t.Fatalf("second Get = %q, want ok", b)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Loader called %d times, want 2", got)
	}
}

// TestImpl_PerWaiterCopies: overlapped waiters each receive a distinct backing array.
func TestImpl_PerWaiterCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := implCtx(t)
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			<-release
			return []byte("copy-me"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		var a, b []byte
		var errA, errB error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			a, errA = c.Get(ctx, "k")
		}()
		go func() {
			defer wg.Done()
			b, errB = c.Get(ctx, "k")
		}()
		synctest.Wait()
		close(release)
		wg.Wait()
		if errA != nil || errB != nil {
			t.Fatalf("errors = %v, %v", errA, errB)
		}
		if !bytes.Equal(a, b) || string(a) != "copy-me" {
			t.Fatalf("results = %q, %q", a, b)
		}
		a[0] = 'X'
		if b[0] == 'X' {
			t.Fatalf("mutating one waiter result changed the other")
		}
		hit := implMustGet(t, ctx, c, "k")
		if string(hit) != "copy-me" {
			t.Fatalf("later hit = %q, want copy-me", hit)
		}
	})
}

// TestImpl_InFlightExcludedFromLen: an in-flight miss does not count toward Len.
func TestImpl_InFlightExcludedFromLen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := implCtx(t)
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			<-release
			return []byte("v"), nil
		}
		c := New(1, load)
		t.Cleanup(func() { c.Close() })

		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = c.Get(ctx, "only")
		}()
		synctest.Wait()
		if l := c.Len(); l != 0 {
			t.Errorf("Len while in flight = %d, want 0", l)
		}
		close(release)
		<-done
		synctest.Wait()
		if l := c.Len(); l != 1 {
			t.Fatalf("Len after success = %d, want 1", l)
		}
	})
}

// TestImpl_EvictWhileOtherKeyInFlight: an in-flight key is not the eviction victim;
// completion evicts the current LRU resident.
func TestImpl_EvictWhileOtherKeyInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := implCtx(t)
		var mu sync.Mutex
		counts := map[string]int{}
		releaseC := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			mu.Lock()
			counts[key]++
			mu.Unlock()
			if key == "c" {
				<-releaseC
			}
			return []byte("v:" + key), nil
		}
		count := func(key string) int {
			mu.Lock()
			defer mu.Unlock()
			return counts[key]
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		implMustGet(t, ctx, c, "a")
		implMustGet(t, ctx, c, "b") // a is LRU

		doneC := make(chan struct{})
		go func() {
			defer close(doneC)
			_, _ = c.Get(ctx, "c")
		}()
		synctest.Wait()
		if l := c.Len(); l != 2 {
			t.Errorf("Len while c in flight = %d, want 2", l)
		}
		implMustGet(t, ctx, c, "b") // b stays MRU, a stays LRU
		if got := count("c"); got != 1 {
			t.Errorf("Loader(c) = %d, want 1", got)
		}

		close(releaseC)
		<-doneC
		synctest.Wait()

		implMustGet(t, ctx, c, "b")
		implMustGet(t, ctx, c, "c")
		implMustGet(t, ctx, c, "a") // must reload
		if got := count("a"); got != 2 {
			t.Fatalf("Loader(a) = %d, want 2 (evicted)", got)
		}
		if got := count("b"); got != 1 {
			t.Fatalf("Loader(b) = %d, want 1", got)
		}
		if got := count("c"); got != 1 {
			t.Fatalf("Loader(c) = %d, want 1", got)
		}
	})
}

// TestImpl_DistinctKeysInFlight: later completion is MRU, so a following load
// evicts the earlier completion.
func TestImpl_DistinctKeysInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := implCtx(t)
		releaseA := make(chan struct{})
		releaseB := make(chan struct{})
		var callsA, callsB, callsC atomic.Int32
		load := func(_ context.Context, key string) ([]byte, error) {
			switch key {
			case "a":
				callsA.Add(1)
				<-releaseA
			case "b":
				callsB.Add(1)
				<-releaseB
			case "c":
				callsC.Add(1)
			}
			return []byte("v:" + key), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		doneA := make(chan struct{})
		doneB := make(chan struct{})
		go func() {
			defer close(doneA)
			_, _ = c.Get(ctx, "a")
		}()
		go func() {
			defer close(doneB)
			_, _ = c.Get(ctx, "b")
		}()
		synctest.Wait()
		if callsA.Load() != 1 || callsB.Load() != 1 {
			t.Fatalf("calls a=%d b=%d, want 1 and 1", callsA.Load(), callsB.Load())
		}

		close(releaseB)
		<-doneB
		synctest.Wait()
		close(releaseA)
		<-doneA
		synctest.Wait()

		implMustGet(t, ctx, c, "c") // evicts b, a remains
		implMustGet(t, ctx, c, "a") // hit
		implMustGet(t, ctx, c, "b") // reload
		if callsA.Load() != 1 {
			t.Fatalf("Loader(a) = %d, want 1", callsA.Load())
		}
		if callsB.Load() != 2 {
			t.Fatalf("Loader(b) = %d, want 2", callsB.Load())
		}
		if callsC.Load() != 1 {
			t.Fatalf("Loader(c) = %d, want 1", callsC.Load())
		}
	})
}

// TestImpl_NilAndEmptyPayload: nil and empty payloads round-trip distinctly.
func TestImpl_NilAndEmptyPayload(t *testing.T) {
	ctx := implCtx(t)
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		calls.Add(1)
		if key == "nil" {
			return nil, nil
		}
		return []byte{}, nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	b, err := c.Get(ctx, "nil")
	if err != nil {
		t.Fatalf("nil Get error = %v", err)
	}
	if b != nil {
		t.Fatalf("nil Get = %q, want nil", b)
	}
	b, err = c.Get(ctx, "nil")
	if err != nil || b != nil {
		t.Fatalf("nil hit = %q, %v, want nil, nil", b, err)
	}

	empty, err := c.Get(ctx, "empty")
	if err != nil {
		t.Fatalf("empty Get error = %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty Get = %v, want non-nil empty", empty)
	}
	empty2, err := c.Get(ctx, "empty")
	if err != nil || empty2 == nil || len(empty2) != 0 {
		t.Fatalf("empty hit = %v, %v", empty2, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Loader called %d times, want 2", got)
	}
}

// TestImpl_AlreadyDoneCold: an already-cancelled cold Get does not call Loader.
func TestImpl_AlreadyDoneCold(t *testing.T) {
	ctx := implCtx(t)
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		calls.Add(1)
		return []byte("no"), nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })

	caller, cancel := context.WithCancel(ctx)
	cancel()
	b, err := c.Get(caller, "cold")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if b != nil {
		t.Fatalf("bytes = %q, want nil", b)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("Loader called %d times, want 0", got)
	}
}

// TestImpl_AlreadyDoneJoinsFlight: an already-cancelled Get joins an in-flight load
// and does not start a second Loader call.
func TestImpl_AlreadyDoneJoinsFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := implCtx(t)
		release := make(chan struct{})
		var calls atomic.Int32
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("hit"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		go func() {
			_, _ = c.Get(ctx, "k")
		}()
		synctest.Wait()

		caller, cancel := context.WithCancel(ctx)
		cancel()
		b, err := c.Get(caller, "k")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if b != nil {
			t.Fatalf("bytes = %q, want nil", b)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader called %d times, want 1", got)
		}

		close(release)
		synctest.Wait()
		hit := implMustGet(t, ctx, c, "k")
		if string(hit) != "hit" {
			t.Fatalf("later Get = %q, want hit", hit)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader called %d times after hit, want 1", got)
		}
	})
}

// TestImpl_LoaderPanic: a panicking Loader yields a non-nil error and nil bytes,
// and a later Get reloads.
func TestImpl_LoaderPanic(t *testing.T) {
	ctx := implCtx(t)
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		if calls.Add(1) == 1 {
			panic("boom-value")
		}
		return []byte("recovered"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	b, err := c.Get(ctx, "p")
	if err == nil {
		t.Fatal("panic Get error = nil, want non-nil")
	}
	if b != nil {
		t.Fatalf("panic Get bytes = %q, want nil", b)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("boom-value")) {
		t.Fatalf("error %q does not contain panic value", err.Error())
	}
	b = implMustGet(t, ctx, c, "p")
	if string(b) != "recovered" {
		t.Fatalf("reload = %q, want recovered", b)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Loader called %d times, want 2", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

// TestImpl_LoaderGoexit: Goexit still publishes a failure so a later Get reloads.
func TestImpl_LoaderGoexit(t *testing.T) {
	ctx := implCtx(t)
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		if calls.Add(1) == 1 {
			runtime.Goexit()
		}
		return []byte("after-goexit"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	b, err := c.Get(ctx, "g")
	if err == nil {
		t.Fatal("Goexit Get error = nil, want non-nil")
	}
	if b != nil {
		t.Fatalf("Goexit Get bytes = %q, want nil", b)
	}
	b = implMustGet(t, ctx, c, "g")
	if string(b) != "after-goexit" {
		t.Fatalf("reload = %q, want after-goexit", b)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Loader called %d times, want 2", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

// TestImpl_LoaderUsesCacheContext: the Loader sees the cache context, not the caller's.
func TestImpl_LoaderUsesCacheContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type ctxKey struct{}
		parent := implCtx(t)
		caller, cancel := context.WithCancel(context.WithValue(parent, ctxKey{}, "caller-value"))

		release := make(chan struct{})
		var calls atomic.Int32
		var sawValue atomic.Bool
		var loaderCtxDone atomic.Bool
		load := func(lctx context.Context, key string) ([]byte, error) {
			calls.Add(1)
			if lctx.Value(ctxKey{}) != nil {
				sawValue.Store(true)
			}
			<-release
			if lctx.Err() != nil {
				loaderCtxDone.Store(true)
			}
			return []byte("from-cache-ctx"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		var got []byte
		var gotErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			got, gotErr = c.Get(caller, "k")
		}()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Errorf("Loader calls before cancel = %d, want 1", calls.Load())
		}
		if sawValue.Load() {
			t.Errorf("Loader observed the caller's context value")
		}

		// Caller cancel returns the waiter and must not cancel or release the Loader.
		cancel()
		<-done
		if !errors.Is(gotErr, context.Canceled) {
			t.Errorf("Get error = %v, want context.Canceled", gotErr)
		}
		if got != nil {
			t.Errorf("Get bytes = %q, want nil", got)
		}
		synctest.Wait()
		if calls.Load() != 1 {
			t.Errorf("Loader calls after caller cancel = %d, want 1", calls.Load())
		}

		close(release)
		synctest.Wait()
		if loaderCtxDone.Load() {
			t.Errorf("Loader context was canceled by the caller's cancel")
		}
		hit := implMustGet(t, parent, c, "k")
		if string(hit) != "from-cache-ctx" {
			t.Fatalf("hit = %q, want from-cache-ctx", hit)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader called %d times, want 1", got)
		}
	})
}
