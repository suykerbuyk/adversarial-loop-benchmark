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

func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestNilVersusEmpty(t *testing.T) {
	ctx := deadline(t)
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		calls.Add(1)
		if key == "nil" {
			return nil, nil
		}
		return []byte{}, nil
	}
	c := New(4, load)
	t.Cleanup(func() { _ = c.Close() })

	b, err := c.Get(ctx, "nil")
	if err != nil {
		t.Fatalf("Get(nil): %v", err)
	}
	if b != nil {
		t.Fatalf("Get(nil) = %q, want nil slice", b)
	}
	b, err = c.Get(ctx, "nil")
	if err != nil {
		t.Fatalf("second Get(nil): %v", err)
	}
	if b != nil {
		t.Fatalf("second Get(nil) = %q, want nil slice (must hit)", b)
	}

	b, err = c.Get(ctx, "empty")
	if err != nil {
		t.Fatalf("Get(empty): %v", err)
	}
	if b == nil || len(b) != 0 {
		t.Fatalf("Get(empty) = %#v, want non-nil empty", b)
	}
	b = append(b, 'X')
	b2, err := c.Get(ctx, "empty")
	if err != nil {
		t.Fatalf("second Get(empty): %v", err)
	}
	if b2 == nil || len(b2) != 0 {
		t.Fatalf("second Get(empty) = %#v, want non-nil empty", b2)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Loader called %d times, want 2", got)
	}
}

func TestErrorWithBytesDiscarded(t *testing.T) {
	ctx := deadline(t)
	boom := errors.New("partial")
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		if calls.Add(1) == 1 {
			return []byte("x"), boom
		}
		return []byte("ok"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { _ = c.Close() })

	b, err := c.Get(ctx, "a")
	if !errors.Is(err, boom) {
		t.Fatalf("first Get error = %v, want %v", err, boom)
	}
	if b != nil {
		t.Fatalf("first Get bytes = %q, want nil", b)
	}
	if l := c.Len(); l != 0 {
		t.Fatalf("Len after error = %d, want 0", l)
	}
	b, err = c.Get(ctx, "a")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if string(b) != "ok" {
		t.Fatalf("second Get = %q, want ok", b)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Loader called %d times, want 2", got)
	}
}

func TestDistinctWaiterCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			<-release
			return []byte("payload"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { _ = c.Close() })

		var wg sync.WaitGroup
		results := make([][]byte, 2)
		errs := make([]error, 2)
		wg.Add(2)
		for i := 0; i < 2; i++ {
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = c.Get(ctx, "k")
			}(i)
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		for i := 0; i < 2; i++ {
			if errs[i] != nil {
				t.Fatalf("Get[%d]: %v", i, errs[i])
			}
		}
		results[0][0] = 'X'
		if !bytes.Equal(results[1], []byte("payload")) {
			t.Fatalf("waiter copies alias: other = %q", results[1])
		}
	})
}

func TestOrphanLoadAdmitted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		ctx, cancel := context.WithCancel(parent)

		var calls atomic.Int32
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("kept"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { _ = c.Close() })

		errc := make(chan error, 1)
		go func() {
			_, err := c.Get(ctx, "k")
			errc <- err
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-errc:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled Get = %v, want Canceled", err)
			}
		default:
			t.Fatal("cancelled Get did not return")
		}
		close(release)
		synctest.Wait()
		if l := c.Len(); l != 1 {
			t.Fatalf("Len after orphan admit = %d, want 1", l)
		}
		b, err := c.Get(parent, "k")
		if err != nil {
			t.Fatalf("later Get: %v", err)
		}
		if string(b) != "kept" {
			t.Fatalf("later Get = %q, want kept", b)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader called %d times, want 1", got)
		}
	})
}

func TestResolvedBeatsCallerCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		ctx, cancel := context.WithCancel(parent)

		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			<-release
			return []byte("fresh"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { _ = c.Close() })

		type result struct {
			b   []byte
			err error
		}
		out := make(chan result, 1)
		go func() {
			b, err := c.Get(ctx, "k")
			out <- result{b, err}
		}()
		synctest.Wait()
		close(release)
		// Let the publisher finish and close flight.done before the waiter is
		// cancelled, so both channels are ready when arbitration runs.
		synctest.Wait()
		cancel()
		got := <-out
		if got.err != nil {
			t.Fatalf("Get err = %v, want nil (resolved beats cancel)", got.err)
		}
		if string(got.b) != "fresh" {
			t.Fatalf("Get = %q, want fresh", got.b)
		}
	})
}

func TestCloseBeatsResolution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		ctx, cancel := context.WithCancel(parent)
		defer cancel()

		release := make(chan struct{})
		var calls atomic.Int32
		load := func(lctx context.Context, key string) ([]byte, error) {
			calls.Add(1)
			select {
			case <-release:
			case <-lctx.Done():
			}
			return []byte("late"), nil
		}
		c := New(2, load)

		type result struct {
			b   []byte
			err error
		}
		out := make(chan result, 1)
		go func() {
			b, err := c.Get(ctx, "k")
			out <- result{b, err}
		}()
		synctest.Wait()
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		got := <-out
		if !errors.Is(got.err, ErrClosed) {
			t.Fatalf("Get err = %v, want ErrClosed", got.err)
		}
		if got.b != nil {
			t.Fatalf("Get bytes = %q, want nil", got.b)
		}
		if l := c.Len(); l != 0 {
			t.Fatalf("Len after Close = %d, want 0", l)
		}
		b, err := c.Get(parent, "k")
		if !errors.Is(err, ErrClosed) || b != nil {
			t.Fatalf("follow-up Get = (%q, %v), want ErrClosed", b, err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader called %d times, want 1", got)
		}
	})
}

func TestCloseDiscardsLateSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()

		started := make(chan struct{})
		load := func(lctx context.Context, key string) ([]byte, error) {
			close(started)
			<-lctx.Done()
			return []byte("discarded"), nil
		}
		c := New(2, load)
		go func() { _, _ = c.Get(ctx, "k") }()
		synctest.Wait()
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if l := c.Len(); l != 0 {
			t.Fatalf("Len after discarded success = %d, want 0", l)
		}
	})
}

func TestZeroWaiterCloseDiscards(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		ctx, cancel := context.WithCancel(parent)

		release := make(chan struct{})
		load := func(lctx context.Context, key string) ([]byte, error) {
			select {
			case <-release:
			case <-lctx.Done():
			}
			return []byte("nope"), nil
		}
		c := New(2, load)
		errc := make(chan error, 1)
		go func() {
			_, err := c.Get(ctx, "k")
			errc <- err
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-errc:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waiter err = %v, want Canceled", err)
			}
		default:
			t.Fatal("waiter did not leave")
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if l := c.Len(); l != 0 {
			t.Fatalf("Len = %d, want 0 (late success discarded)", l)
		}
	})
}

func TestInFlightDoesNotCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		release := make(chan struct{})
		var calls atomic.Int32
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			if key == "b" {
				<-release
			}
			return []byte("v:" + key), nil
		}
		c := New(1, load)
		t.Cleanup(func() { _ = c.Close() })

		if _, err := c.Get(ctx, "a"); err != nil {
			t.Fatalf("Get(a): %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := c.Get(ctx, "b")
			done <- err
		}()
		synctest.Wait()
		if l := c.Len(); l != 1 {
			t.Fatalf("Len while in flight = %d, want 1", l)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("Get(b): %v", err)
		}
		if l := c.Len(); l != 1 {
			t.Fatalf("Len after admit = %d, want 1", l)
		}
		// a was resident and untouched, so admitting b evicts a.
		b, err := c.Get(ctx, "a")
		if err != nil {
			t.Fatalf("reload a: %v", err)
		}
		if string(b) != "v:a" {
			t.Fatalf("reload a = %q", b)
		}
		// a, b, then a again after eviction.
		if got := calls.Load(); got != 3 {
			t.Fatalf("Loader called %d times, want 3 (a evicted by b)", got)
		}
	})
}

func TestCapacityOneHitSaves(t *testing.T) {
	ctx := deadline(t)
	counts := map[string]int{}
	var mu sync.Mutex
	load := func(_ context.Context, key string) ([]byte, error) {
		mu.Lock()
		counts[key]++
		mu.Unlock()
		return []byte(key), nil
	}
	c := New(1, load)
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Get(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "a"); err != nil { // hit, still sole resident
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "a"); err != nil { // reloaded: b evicted a
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["a"] != 2 || counts["b"] != 1 {
		t.Fatalf("counts = %v, want a:2 b:1", counts)
	}
}

func TestConcurrentDistinctKeysLen(t *testing.T) {
	ctx := deadline(t)
	load := func(_ context.Context, key string) ([]byte, error) {
		return []byte(key), nil
	}
	const capN = 4
	c := New(capN, load)
	t.Cleanup(func() { _ = c.Close() })

	var wg sync.WaitGroup
	errc := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i%16))
			if _, err := c.Get(ctx, key); err != nil {
				errc <- err
			}
			if l := c.Len(); l > capN {
				errc <- errors.New("len above capacity")
			}
		}(i)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	if l := c.Len(); l > capN || l == 0 {
		t.Fatalf("final Len = %d, want 1..%d", l, capN)
	}
}

func TestOverlappingCloseBothJoin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The Loader ignores its context and waits on release. That breaks the
	// Loader contract on purpose so Close stays inside wg.Wait long enough
	// for a second Close to arrive. A real Loader must return on ctx.Done.
	release := make(chan struct{})
	entered := make(chan struct{})
	var returned atomic.Bool
	load := func(_ context.Context, key string) ([]byte, error) {
		close(entered)
		<-release
		returned.Store(true)
		return []byte("x"), nil
	}
	c := New(2, load)

	go func() { _, _ = c.Get(ctx, "k") }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("Loader did not start")
	}

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- c.Close() }()
	go func() { secondDone <- c.Close() }()

	// Neither Close may finish before the Loader returns. A short poll is
	// enough to catch a second Close that skips the in-progress join.
	select {
	case err := <-firstDone:
		t.Fatalf("first Close returned before Loader: %v", err)
	case err := <-secondDone:
		t.Fatalf("second Close returned before Loader: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if returned.Load() {
		t.Fatal("Loader returned before release")
	}
	close(release)

	if err := <-firstDone; err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !returned.Load() {
		t.Fatal("Loader had not returned when both Close returned")
	}
	if _, err := c.Get(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: %v, want ErrClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("third Close: %v", err)
	}
}

func TestSingleFlightTwoKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		var calls sync.Map
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Store(key, 1)
			<-release
			return []byte(key), nil
		}
		c := New(4, load)
		t.Cleanup(func() { _ = c.Close() })
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = c.Get(ctx, "a")
		}()
		go func() {
			defer wg.Done()
			_, _ = c.Get(ctx, "b")
		}()
		synctest.Wait()
		n := 0
		calls.Range(func(_, _ any) bool {
			n++
			return true
		})
		if n != 2 {
			t.Fatalf("distinct in-flight keys = %d, want 2", n)
		}
		close(release)
		wg.Wait()
	})
}

func TestHitIgnoresCallerCancel(t *testing.T) {
	ctx := deadline(t)
	load := func(_ context.Context, key string) ([]byte, error) {
		return []byte("hit"), nil
	}
	c := New(1, load)
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Get(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	b, err := c.Get(dead, "a")
	if err != nil {
		t.Fatalf("hit with cancelled ctx: %v", err)
	}
	if string(b) != "hit" {
		t.Fatalf("hit = %q", b)
	}
}

func TestEmptyKey(t *testing.T) {
	ctx := deadline(t)
	load := func(_ context.Context, key string) ([]byte, error) {
		return []byte("empty-key"), nil
	}
	c := New(1, load)
	t.Cleanup(func() { _ = c.Close() })
	b, err := c.Get(ctx, "")
	if err != nil || string(b) != "empty-key" {
		t.Fatalf("Get empty key = (%q, %v)", b, err)
	}
	b, err = c.Get(ctx, "")
	if err != nil || string(b) != "empty-key" {
		t.Fatalf("hit empty key = (%q, %v)", b, err)
	}
}
