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

// Public acceptance suite for the bounded concurrent single-flight block cache.
// Shipped with the task; the implementor sees these. Deterministic: single-flight
// overlap is forced with testing/synctest, and every wait is bounded by a 5s
// context deadline, so a deadlock fails via the deadline and t.Fatal rather than
// hanging. There is no TestMain and no os.Exit; each test cleans up with Close.

// testCtx returns a context bounded by a 5s deadline and registers its cancel.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestHitReturnsCopy covers rule 1 and rule 4: a second Get of a resident key
// does not call the Loader, and mutating the bytes a Get returns does not change
// what a later Get returns.
func TestHitReturnsCopy(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		calls.Add(1)
		return []byte("payload"), nil
	}
	c := New(4, load)
	t.Cleanup(func() { c.Close() })

	b1, err := c.Get(ctx, "a")
	if err != nil {
		t.Fatalf("first Get: unexpected error %v", err)
	}
	if string(b1) != "payload" {
		t.Fatalf("first Get = %q, want %q", b1, "payload")
	}
	// Mutate the returned slice; it must not affect the cache.
	for i := range b1 {
		b1[i] = 'X'
	}

	b2, err := c.Get(ctx, "a")
	if err != nil {
		t.Fatalf("second Get: unexpected error %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Loader called %d times, want 1 (second Get must hit)", got)
	}
	if string(b2) != "payload" {
		t.Fatalf("second Get = %q, want %q (mutation leaked into the cache)", b2, "payload")
	}
}

// TestSingleFlight covers rule 2: 50 concurrent Gets of one cold key cause
// exactly one Loader call, and every waiter receives equal bytes. The overlap is
// made deterministic with testing/synctest: synctest.Wait returns only once all
// 50 Gets are durably blocked on the in-flight load, at which point the Loader
// must have been entered exactly once and still be blocked.
func TestSingleFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		var calls atomic.Int32
		var returned atomic.Bool
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			returned.Store(true)
			return []byte("block:" + key), nil
		}
		c := New(8, load)
		t.Cleanup(func() { c.Close() })

		const n = 50
		results := make([][]byte, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = c.Get(ctx, "k")
			}(i)
		}

		// All 50 Gets are now durably blocked on the single in-flight load.
		// Capture the single-flight evidence while they are parked, then release
		// and drain. These checks use t.Errorf, not t.Fatalf: a fatal here would
		// exit the bubble's root goroutine while 50 goroutines are still parked,
		// which synctest reports as a bubble deadlock and masks the real reason.
		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Errorf("under overlap: Loader entered %d times, want exactly 1 (single-flight violated)", got)
		}
		if returned.Load() {
			t.Errorf("under overlap: Loader already returned, want it still blocked")
		}

		// Release the one load; every waiter resolves from it.
		close(release)
		wg.Wait()
		synctest.Wait()

		if got := calls.Load(); got != 1 {
			t.Errorf("after release: Loader entered %d times, want exactly 1", got)
		}
		want := []byte("block:k")
		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatalf("Get[%d]: unexpected error %v", i, errs[i])
			}
			if !bytes.Equal(results[i], want) {
				t.Fatalf("Get[%d] = %q, want %q", i, results[i], want)
			}
		}
	})
}

// TestErrorNotCached covers rule 3: a Loader error is returned to the waiter and
// is not cached, so a later Get re-loads.
func TestErrorNotCached(t *testing.T) {
	ctx := testCtx(t)
	boom := errors.New("backing store down")
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		if calls.Add(1) == 1 {
			return nil, boom
		}
		return []byte("ok"), nil
	}
	c := New(4, load)
	t.Cleanup(func() { c.Close() })

	b, err := c.Get(ctx, "a")
	if !errors.Is(err, boom) {
		t.Fatalf("first Get error = %v, want %v", err, boom)
	}
	if b != nil {
		t.Fatalf("first Get bytes = %q, want nil on error", b)
	}

	b, err = c.Get(ctx, "a")
	if err != nil {
		t.Fatalf("second Get: unexpected error %v", err)
	}
	if string(b) != "ok" {
		t.Fatalf("second Get = %q, want %q", b, "ok")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Loader called %d times, want 2 (error must not be cached)", got)
	}
}

// TestLRUEviction covers rules 5 and 6: with capacity 2, loading a then b then
// touching a (hit) makes b least-recently-used; loading c evicts b. A later Get
// of a hits (no reload) while a Get of b must reload.
func TestLRUEviction(t *testing.T) {
	ctx := testCtx(t)
	var mu sync.Mutex
	counts := map[string]int{}
	load := func(_ context.Context, key string) ([]byte, error) {
		mu.Lock()
		counts[key]++
		mu.Unlock()
		return []byte("v:" + key), nil
	}
	count := func(key string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[key]
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	mustGet(t, ctx, c, "a") // load a       -> resident {a}
	mustGet(t, ctx, c, "b") // load b       -> resident {a,b}, a is LRU
	mustGet(t, ctx, c, "a") // hit a        -> b is LRU
	mustGet(t, ctx, c, "c") // load c, evict b -> resident {a,c}
	mustGet(t, ctx, c, "a") // hit a        -> proves a survived
	mustGet(t, ctx, c, "b") // reload b     -> proves b was evicted

	if got := count("a"); got != 1 {
		t.Fatalf("Loader(a) called %d times, want 1 (a must not be evicted)", got)
	}
	if got := count("b"); got != 2 {
		t.Fatalf("Loader(b) called %d times, want 2 (b must be evicted then reloaded)", got)
	}
	if got := count("c"); got != 1 {
		t.Fatalf("Loader(c) called %d times, want 1", got)
	}
}

// TestLenBounded covers rule 5: Len is resident-only and never exceeds capacity.
func TestLenBounded(t *testing.T) {
	ctx := testCtx(t)
	load := func(_ context.Context, key string) ([]byte, error) {
		return []byte("v:" + key), nil
	}
	c := New(3, load)
	t.Cleanup(func() { c.Close() })

	for _, k := range []string{"a", "b", "c", "d", "e"} {
		mustGet(t, ctx, c, k)
		if l := c.Len(); l > 3 {
			t.Fatalf("after Get(%q): Len() = %d, want <= 3", k, l)
		}
	}
	if l := c.Len(); l != 3 {
		t.Fatalf("final Len() = %d, want 3", l)
	}
}

// TestClosedReturnsErrClosed covers rule 8: Get after Close returns ErrClosed
// (wrapping is legal; errors.Is is the contract) and Close is idempotent.
func TestClosedReturnsErrClosed(t *testing.T) {
	ctx := testCtx(t)
	load := func(_ context.Context, key string) ([]byte, error) {
		return []byte("v:" + key), nil
	}
	c := New(4, load)
	t.Cleanup(func() { c.Close() })

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v, want nil", err)
	}
	if _, err := c.Get(ctx, "a"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: %v, want ErrClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil", err)
	}
}

// TestNewPanics covers New's preconditions: capacity < 1 or a nil Loader panics.
func TestNewPanics(t *testing.T) {
	load := func(_ context.Context, key string) ([]byte, error) { return nil, nil }
	assertPanics(t, "New(0, load)", func() { New(0, load) })
	assertPanics(t, "New(1, nil)", func() { New(1, nil) })
}

func mustGet(t *testing.T, ctx context.Context, c Cache, key string) []byte {
	t.Helper()
	b, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get(%q): unexpected error %v", key, err)
	}
	return b
}

func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", what)
		}
	}()
	fn()
}
