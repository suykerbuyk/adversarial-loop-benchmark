package blockcache

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOrphanedSuccessPopulates(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int32
	load := func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []byte("kept"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	caller, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := c.Get(caller, "a")
		got <- err
	}()

	<-entered
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Get err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled Get did not return")
	}

	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for c.Len() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.Len() != 1 {
		t.Fatalf("Len after orphaned success = %d, want 1", c.Len())
	}

	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	b, err := c.Get(ctx, "a")
	if err != nil {
		t.Fatalf("hit after orphan: %v", err)
	}
	if string(b) != "kept" {
		t.Fatalf("hit after orphan = %q, want kept", b)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Loader calls = %d, want 1", got)
	}
}

func TestCloseDiscardsLateSuccess(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	load := func(ctx context.Context, key string) ([]byte, error) {
		close(entered)
		<-release
		if ctx.Err() != nil {
			return []byte("late"), nil
		}
		return []byte("late"), nil
	}
	c := New(2, load)

	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan error, 1)
	go func() {
		_, err := c.Get(caller, "a")
		got <- err
	}()
	<-entered

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()

	select {
	case err := <-got:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("waiter err = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter was not woken by Close")
	}
	if c.Len() != 0 {
		t.Fatalf("Len during Close = %d, want 0", c.Len())
	}

	select {
	case <-closed:
		t.Fatal("Close returned before Loader returned")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close err = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after Loader returned")
	}
	if c.Len() != 0 {
		t.Fatalf("Len after discarded success = %d, want 0", c.Len())
	}
}

func TestOverlappingClose(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	load := func(ctx context.Context, key string) ([]byte, error) {
		close(entered)
		<-ctx.Done()
		<-release
		return []byte("x"), nil
	}
	c := New(1, load)
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c.Get(ctx, "a")
	}()
	<-entered

	errs := make(chan error, 2)
	go func() { errs <- c.Close() }()
	go func() { errs <- c.Close() }()
	time.Sleep(20 * time.Millisecond)
	close(release)

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("Close err = %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("overlapping Close did not return")
		}
	}
}

func TestErrorWithBytesDiscarded(t *testing.T) {
	boom := errors.New("nope")
	var calls atomic.Int32
	load := func(context.Context, string) ([]byte, error) {
		if calls.Add(1) == 1 {
			return []byte("discard-me"), boom
		}
		return []byte("ok"), nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	b, err := c.Get(ctx, "a")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if b != nil {
		t.Fatalf("bytes = %q, want nil", b)
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0 after error", c.Len())
	}
	b, err = c.Get(ctx, "a")
	if err != nil || string(b) != "ok" {
		t.Fatalf("retry = %q, %v", b, err)
	}
}

func TestNilSuccessCached(t *testing.T) {
	var calls atomic.Int32
	load := func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return nil, nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	b, err := c.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if b != nil {
		t.Fatalf("got %q, want nil", b)
	}
	if _, err := c.Get(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestCapacityOneEvicts(t *testing.T) {
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		calls.Add(1)
		return []byte(key), nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mustGet(t, ctx, c, "a")
	mustGet(t, ctx, c, "a")
	mustGet(t, ctx, c, "b")
	mustGet(t, ctx, c, "a")
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3 (a, b, reload a)", got)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
}

func TestInflightDoesNotCountOrEvict(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	load := func(ctx context.Context, key string) ([]byte, error) {
		if key == "fly" {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []byte(key), nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { c.Get(ctx, "fly") }()
	<-entered
	if c.Len() != 0 {
		t.Fatalf("Len with only in-flight = %d, want 0", c.Len())
	}
	mustGet(t, ctx, c, "resident")
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := c.Get(ctx, "fly")
		if err != nil {
			t.Fatal(err)
		}
		if string(b) == "fly" && c.Len() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("in-flight key did not become resident")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLenNeverExceedsCapacity(t *testing.T) {
	start := make(chan struct{})
	load := func(context.Context, string) ([]byte, error) {
		<-start
		return []byte("v"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var over atomic.Bool
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				if c.Len() > 2 {
					over.Store(true)
				}
			}
		}
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Get(ctx, string(rune('a'+i)))
		}(i)
	}
	close(start)
	wg.Wait()
	close(stop)
	if over.Load() {
		t.Fatal("Len exceeded capacity")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

func TestLoaderSliceNotRetained(t *testing.T) {
	var retained []byte
	load := func(context.Context, string) ([]byte, error) {
		retained = []byte("payload")
		return retained, nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.Get(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	for i := range retained {
		retained[i] = 'X'
	}
	b, err := c.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "payload" {
		t.Fatalf("cached %q, want payload", b)
	}
}

func TestWaitersGetDistinctCopies(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	load := func(context.Context, string) ([]byte, error) {
		close(entered)
		<-release
		return []byte("block"), nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const n = 8
	out := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			out[i], errs[i] = c.Get(ctx, "k")
		}(i)
	}
	<-entered
	close(release)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("Get[%d]: %v", i, errs[i])
		}
	}
	for i := range out[0] {
		out[0][i] = 'Z'
	}
	for i := 1; i < n; i++ {
		if !bytes.Equal(out[i], []byte("block")) {
			t.Fatalf("waiter %d mutated by waiter 0: %q", i, out[i])
		}
	}
}

func TestClosePriorityOverCallerCancel(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	load := func(ctx context.Context, key string) ([]byte, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return []byte("x"), nil
	}
	raw := New(1, load)
	c := raw.(*cache)

	// The Get must still be waiting when Close sets closed. Canceling the
	// caller first lets wait observe ctx.Err() while the cache is open, which
	// is a legal rule-7 outcome and races with the ErrClosed assertion.
	// Block inside wait's select until Close has published closed, then cancel
	// the caller so both signals are pending and the re-check prefers ErrClosed.
	caller, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()

	waiting := make(chan struct{})
	proceed := make(chan struct{})
	c.mu.Lock()
	c.testWaitHook = func() {
		close(waiting)
		<-proceed
	}
	c.mu.Unlock()

	got := make(chan error, 1)
	go func() {
		_, err := c.Get(caller, "a")
		got <- err
	}()
	<-entered
	<-waiting

	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()

	deadline := time.Now().Add(5 * time.Second)
	for !c.closedForTest() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !c.closedForTest() {
		t.Fatal("Close did not set closed while Get was waiting")
	}

	// Both the close signal and the caller cancel are pending before wait
	// re-checks. Rule 7 says ErrClosed wins.
	cancelCaller()
	close(proceed)

	select {
	case err := <-got:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed over caller cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get did not return")
	}
	close(release)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
}

func TestMixedConcurrentClose(t *testing.T) {
	load := func(ctx context.Context, key string) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
			return []byte(key), nil
		}
	}
	c := New(4, load)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			gctx := ctx
			var cancelOne context.CancelFunc
			if i%3 == 0 {
				gctx, cancelOne = context.WithCancel(ctx)
				if i%2 == 0 {
					cancelOne()
				} else {
					defer cancelOne()
				}
			}
			_, _ = c.Get(gctx, string(rune('a'+i%7)))
		}(i)
	}
	time.Sleep(2 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if _, err := c.Get(ctx, "z"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close = %v, want ErrClosed", err)
	}
}
