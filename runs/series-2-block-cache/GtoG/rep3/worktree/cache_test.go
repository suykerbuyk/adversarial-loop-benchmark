package blockcache

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestAlreadyCancelledJoinsInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		load := func(ctx context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("block:" + key), nil
		}
		c := New(4, load)
		t.Cleanup(func() { c.Close() })

		started := make(chan struct{})
		var live []byte
		var liveErr error
		go func() {
			close(started)
			live, liveErr = c.Get(context.Background(), "k")
		}()
		<-started
		synctest.Wait()

		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		b, err := c.Get(cancelled, "k")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Get error = %v, want context.Canceled", err)
		}
		if b != nil {
			t.Fatalf("cancelled Get bytes = %q, want nil", b)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader entered %d times before release, want 1", got)
		}

		close(release)
		synctest.Wait()
		if liveErr != nil {
			t.Fatalf("live Get: unexpected error %v", liveErr)
		}
		if string(live) != "block:k" {
			t.Fatalf("live Get = %q, want %q", live, "block:k")
		}

		hit, err := c.Get(context.Background(), "k")
		if err != nil {
			t.Fatalf("following Get: unexpected error %v", err)
		}
		if string(hit) != "block:k" {
			t.Fatalf("following Get = %q, want %q", hit, "block:k")
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader entered %d times, want 1", got)
		}
	})
}

func TestAllWaitersCancelledBeforeParkStillCaches(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int32
	load := func(ctx context.Context, key string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return []byte("kept"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b, err := c.Get(ctx, "k")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Get error = %v, want context.Canceled", err)
	}
	if b != nil {
		t.Fatalf("Get bytes = %q, want nil", b)
	}

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Loader did not start")
	}
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for c.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 after orphaned success", c.Len())
	}

	hit, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("following Get: unexpected error %v", err)
	}
	if string(hit) != "kept" {
		t.Fatalf("following Get = %q, want %q", hit, "kept")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Loader entered %d times, want 1", got)
	}
}

func TestOrphanedErrorNotCached(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	boom := errors.New("boom")
	var calls atomic.Int32
	load := func(ctx context.Context, key string) ([]byte, error) {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			<-release
			return nil, boom
		}
		return []byte("ok"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Get error = %v, want context.Canceled", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Loader did not start")
	}
	close(release)

	// The first retry may still join the failed flight. Keep going until a
	// Get observes the reload, which is the proof the error was not stored.
	deadline := time.Now().Add(5 * time.Second)
	var b []byte
	var err error
	for {
		b, err = c.Get(context.Background(), "k")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reload Get: %v", err)
		}
	}
	if string(b) != "ok" {
		t.Fatalf("reload Get = %q, want %q", b, "ok")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Loader entered %d times, want 2", got)
	}
}

func TestCloseUnblocksLoaderOnCtxDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		load := func(ctx context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-ctx.Done()
			return []byte("late:" + key), nil
		}
		c := New(2, load)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var got []byte
		var gotErr error
		done := make(chan struct{})
		go func() {
			got, gotErr = c.Get(ctx, "k")
			close(done)
		}()

		synctest.Wait()
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		<-done
		if !errors.Is(gotErr, ErrClosed) {
			t.Fatalf("in-flight Get error = %v, want ErrClosed", gotErr)
		}
		if got != nil {
			t.Fatalf("in-flight Get bytes = %q, want nil", got)
		}

		b, err := c.Get(context.Background(), "k")
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Get after Close: %v, want ErrClosed", err)
		}
		if b != nil {
			t.Fatalf("Get after Close bytes = %q, want nil", b)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader entered %d times, want 1", got)
		}
	})
}

func TestCloseWakesWaiterBeforeLoaderReturns(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int32
	load := func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		close(entered)
		<-release
		return []byte("should-not-store"), nil
	}
	c := New(2, load)

	var got []byte
	var gotErr error
	waiterDone := make(chan struct{})
	go func() {
		got, gotErr = c.Get(context.Background(), "k")
		close(waiterDone)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Loader did not start")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()

	select {
	case <-waiterDone:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter was not woken before the loader returned")
	}
	if !errors.Is(gotErr, ErrClosed) {
		t.Fatalf("waiter error = %v, want ErrClosed", gotErr)
	}
	if got != nil {
		t.Fatalf("waiter bytes = %q, want nil", got)
	}

	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before the loader was released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the loader returned")
	}

	b, err := c.Get(context.Background(), "k")
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: %v, want ErrClosed", err)
	}
	if b != nil {
		t.Fatalf("Get after Close bytes = %q, want nil", b)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Loader entered %d times, want 1", got)
	}
}

func TestConcurrentDoubleClose(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	load := func(ctx context.Context, key string) ([]byte, error) {
		once.Do(func() { close(entered) })
		<-release
		return []byte("x"), nil
	}
	c := New(2, load)

	var wg sync.WaitGroup
	wg.Add(4)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Get(context.Background(), "k")
		}(i)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Loader did not start")
	}

	closeErrs := make([]error, 2)
	go func() {
		defer wg.Done()
		closeErrs[0] = c.Close()
	}()
	go func() {
		defer wg.Done()
		closeErrs[1] = c.Close()
	}()

	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range closeErrs {
		if err != nil {
			t.Fatalf("Close[%d]: %v, want nil", i, err)
		}
	}
	for i, err := range errs {
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Get[%d]: %v, want ErrClosed", i, err)
		}
	}
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: %v, want ErrClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("third Close: %v, want nil", err)
	}
}

func TestCapacityOneEvictsAndInFlightDoesNotCount(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	entered := make(chan struct{})
	var mu sync.Mutex
	counts := map[string]int{}
	load := func(_ context.Context, key string) ([]byte, error) {
		mu.Lock()
		counts[key]++
		mu.Unlock()
		if key == "c" {
			close(entered)
			<-release
		}
		return []byte("v:" + key), nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })

	if _, err := c.Get(ctx, "a"); err != nil {
		t.Fatalf("Get(a): %v", err)
	}
	if _, err := c.Get(ctx, "b"); err != nil {
		t.Fatalf("Get(b): %v", err)
	}
	if c.Len() != 1 {
		t.Fatalf("Len() = %d after two admits, want 1", c.Len())
	}

	admitted := make(chan struct{})
	go func() {
		_, _ = c.Get(ctx, "c")
		close(admitted)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Loader(c) did not start")
	}
	if c.Len() != 1 {
		t.Fatalf("Len() = %d while c is in flight, want 1", c.Len())
	}
	close(release)
	select {
	case <-admitted:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight admit did not finish")
	}
	if c.Len() != 1 {
		t.Fatalf("Len() = %d after admit, want 1", c.Len())
	}

	if _, err := c.Get(ctx, "b"); err != nil {
		t.Fatalf("reload b: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["a"] != 1 {
		t.Fatalf("Loader(a) = %d, want 1", counts["a"])
	}
	if counts["b"] != 2 {
		t.Fatalf("Loader(b) = %d, want 2 (evicted by c, then reloaded)", counts["b"])
	}
}

func TestLoaderErrorWithBytesNotCached(t *testing.T) {
	boom := errors.New("partial")
	var calls atomic.Int32
	load := func(ctx context.Context, key string) ([]byte, error) {
		if calls.Add(1) == 1 {
			return []byte("x"), boom
		}
		return []byte("ok"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	b, err := c.Get(context.Background(), "k")
	if !errors.Is(err, boom) {
		t.Fatalf("first Get error = %v, want %v", err, boom)
	}
	if b != nil {
		t.Fatalf("first Get bytes = %q, want nil", b)
	}
	if c.Len() != 0 {
		t.Fatalf("Len() = %d, want 0 after error", c.Len())
	}
	b, err = c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if string(b) != "ok" {
		t.Fatalf("second Get = %q, want %q", b, "ok")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Loader entered %d times, want 2", got)
	}
}

func TestNilPayloadCachedAsEmpty(t *testing.T) {
	var calls atomic.Int32
	load := func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		return nil, nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	b1, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if b1 == nil || len(b1) != 0 {
		t.Fatalf("first Get = %v, want non-nil empty", b1)
	}
	b1 = append(b1, 'Z')

	b2, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if b2 == nil || len(b2) != 0 {
		t.Fatalf("second Get = %v, want non-nil empty", b2)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Loader entered %d times, want 1", got)
	}
}

func TestLoaderSliceMutationDoesNotLeak(t *testing.T) {
	var calls atomic.Int32
	payload := []byte("stable")
	load := func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		return payload, nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	if _, err := c.Get(context.Background(), "k"); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	payload[0] = 'X'

	b, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if string(b) != "stable" {
		t.Fatalf("second Get = %q, want %q", b, "stable")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Loader entered %d times, want 1", got)
	}
}

func TestConcurrentDistinctKeysBounded(t *testing.T) {
	load := func(ctx context.Context, key string) ([]byte, error) {
		return []byte("v:" + key), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	var wg sync.WaitGroup
	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	errCh := make(chan error, len(keys))
	wg.Add(len(keys))
	for _, k := range keys {
		go func(k string) {
			defer wg.Done()
			if _, err := c.Get(context.Background(), k); err != nil {
				errCh <- err
			}
			if l := c.Len(); l > 2 {
				errCh <- errors.New("Len exceeded capacity")
			}
		}(k)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if l := c.Len(); l > 2 || l < 0 {
		t.Fatalf("final Len() = %d, want 0..2", l)
	}
}

func TestCancelledCtxLosesToClose(t *testing.T) {
	entered := make(chan struct{})
	load := func(ctx context.Context, key string) ([]byte, error) {
		close(entered)
		<-ctx.Done()
		return []byte("x"), nil
	}
	c := New(1, load)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiterDone := make(chan struct{})
	var gotErr error
	go func() {
		_, gotErr = c.Get(ctx, "k")
		close(waiterDone)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Loader did not start")
	}

	// The waiter is already parked. Cancelling its ctx must not outrank Close.
	cancel()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-waiterDone:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return")
	}
	if !errors.Is(gotErr, ErrClosed) {
		t.Fatalf("Get error = %v, want ErrClosed", gotErr)
	}
}

func TestParkedCancelReturnsBeforeLoader(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	load := func(ctx context.Context, key string) ([]byte, error) {
		close(entered)
		<-release
		return []byte("late"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	var got []byte
	var gotErr error
	done := make(chan struct{})
	go func() {
		got, gotErr = c.Get(ctx, "k")
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Loader did not start")
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parked Get did not return after cancel")
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("Get error = %v, want context.Canceled", gotErr)
	}
	if got != nil {
		t.Fatalf("Get bytes = %q, want nil", got)
	}
	if c.Len() != 0 {
		t.Fatalf("Len() = %d while load is still in flight, want 0", c.Len())
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for c.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	hit, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("following Get: %v", err)
	}
	if string(hit) != "late" {
		t.Fatalf("following Get = %q, want %q", hit, "late")
	}
}

func TestParkedDeadlineReturnsBeforeLoader(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	load := func(ctx context.Context, key string) ([]byte, error) {
		close(entered)
		<-release
		return []byte("late"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	b, err := c.Get(ctx, "k")
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get error = %v, want DeadlineExceeded", err)
	}
	if b != nil {
		t.Fatalf("Get bytes = %q, want nil", b)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Get returned after %s, want a bound near the deadline", elapsed)
	}
	select {
	case <-entered:
	default:
		t.Fatal("Loader was not entered")
	}
	close(release)
}

func TestParkedCancelInSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		load := func(ctx context.Context, key string) ([]byte, error) {
			<-release
			return []byte("late"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		ctx, cancel := context.WithCancel(t.Context())
		var got []byte
		var gotErr error
		done := make(chan struct{})
		go func() {
			got, gotErr = c.Get(ctx, "k")
			close(done)
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("parked Get still blocked after cancel")
		}
		if !errors.Is(gotErr, context.Canceled) {
			t.Fatalf("Get error = %v, want context.Canceled", gotErr)
		}
		if got != nil {
			t.Fatalf("Get bytes = %q, want nil", got)
		}
		close(release)
		synctest.Wait()
		hit, err := c.Get(context.Background(), "k")
		if err != nil {
			t.Fatalf("following Get: %v", err)
		}
		if string(hit) != "late" {
			t.Fatalf("following Get = %q, want %q", hit, "late")
		}
	})
}

func TestParkedThenCancelledReturnsCtxErr(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		load := func(ctx context.Context, key string) ([]byte, error) {
			<-release
			return []byte("ok"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var got []byte
		var gotErr error
		done := make(chan struct{})
		go func() {
			got, gotErr = c.Get(ctx, "k")
			close(done)
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("parked Get still blocked after cancel")
		}
		if !errors.Is(gotErr, context.Canceled) {
			t.Fatalf("Get error = %v, want context.Canceled", gotErr)
		}
		if got != nil {
			t.Fatalf("Get bytes = %q, want nil", got)
		}
		close(release)
		synctest.Wait()
		hit, err := c.Get(t.Context(), "k")
		if err != nil {
			t.Fatalf("following Get: %v", err)
		}
		if string(hit) != "ok" {
			t.Fatalf("following Get = %q, want %q", hit, "ok")
		}
	})
}

func TestGetAfterCloseDoesNotCallLoader(t *testing.T) {
	var calls atomic.Int32
	load := func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		return []byte("x"), nil
	}
	c := New(1, load)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: %v, want ErrClosed", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("Loader entered %d times, want 0", got)
	}
}

func TestIdleClose(t *testing.T) {
	load := func(ctx context.Context, key string) ([]byte, error) {
		return []byte("x"), nil
	}
	c := New(1, load)
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if c.Len() != 0 {
		t.Fatalf("Len() = %d after Close, want 0", c.Len())
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSeveralKeysInFlight(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		<-release
		return []byte(key), nil
	}
	c := New(4, load)
	t.Cleanup(func() { c.Close() })

	keys := []string{"a", "b", "c"}
	var wg sync.WaitGroup
	results := make([][]byte, len(keys))
	errs := make([]error, len(keys))
	wg.Add(len(keys))
	for i, k := range keys {
		go func(i int, k string) {
			defer wg.Done()
			results[i], errs[i] = c.Get(context.Background(), k)
		}(i, k)
	}
	time.Sleep(30 * time.Millisecond)
	if c.Len() != 0 {
		t.Fatalf("Len() = %d while loads are in flight, want 0", c.Len())
	}
	close(release)
	wg.Wait()
	for i, k := range keys {
		if errs[i] != nil {
			t.Fatalf("Get(%q): %v", k, errs[i])
		}
		if string(results[i]) != k {
			t.Fatalf("Get(%q) = %q", k, results[i])
		}
	}
	if got := calls.Load(); got != int32(len(keys)) {
		t.Fatalf("Loader entered %d times, want %d", got, len(keys))
	}
}

func TestHitDuringUnrelatedFlight(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int32
	load := func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		if key == "slow" {
			close(entered)
			<-release
		}
		return []byte("v:" + key), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	if _, err := c.Get(context.Background(), "hot"); err != nil {
		t.Fatalf("Get(hot): %v", err)
	}
	go func() { _, _ = c.Get(context.Background(), "slow") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow load did not start")
	}
	b, err := c.Get(context.Background(), "hot")
	if err != nil {
		t.Fatalf("hit during flight: %v", err)
	}
	if string(b) != "v:hot" {
		t.Fatalf("hit = %q, want %q", b, "v:hot")
	}
	before := calls.Load()
	close(release)
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != before {
		t.Fatalf("hit called Loader; calls went from %d to %d", before, calls.Load())
	}
}

func TestEmptyPayloadCached(t *testing.T) {
	var calls atomic.Int32
	load := func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		return []byte{}, nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })
	b, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b == nil || len(b) != 0 {
		t.Fatalf("Get = %v, want non-nil empty", b)
	}
	if _, err := c.Get(context.Background(), "k"); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Loader entered %d times, want 1", got)
	}
}

func TestWaiterCopiesAreIndependent(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	load := func(ctx context.Context, key string) ([]byte, error) {
		close(entered)
		<-release
		return []byte("shared"), nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })

	const n = 4
	results := make([][]byte, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], _ = c.Get(context.Background(), "k")
		}(i)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Loader did not start")
	}
	close(release)
	wg.Wait()
	for i := range results {
		if string(results[i]) != "shared" {
			t.Fatalf("Get[%d] = %q", i, results[i])
		}
		results[i][0] = 'X'
	}
	for i := 1; i < n; i++ {
		if bytes.Equal(results[0], results[i]) && results[0][0] == results[i][0] && string(results[i]) == "Xshared" {
			// Each mutation is local; equality of content after the same mutation is fine
			// only if the slices are distinct. Check identity via a further mutation.
		}
	}
	results[0][1] = 'Q'
	if results[1][1] == 'Q' {
		t.Fatal("waiter copies alias each other")
	}
	hit, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("hit: %v", err)
	}
	if string(hit) != "shared" {
		t.Fatalf("hit = %q, want %q (waiter mutation leaked)", hit, "shared")
	}
}
