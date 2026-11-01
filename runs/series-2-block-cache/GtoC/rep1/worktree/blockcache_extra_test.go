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

func TestDecide(t *testing.T) {
	payload := []byte("payload")
	sentinel := errors.New("load failed")

	t.Run("closed unfinished nil caller", func(t *testing.T) {
		b, err := decide(true, false, payload, nil, nil)
		if !errors.Is(err, ErrClosed) || b != nil {
			t.Fatalf("got (%q, %v), want (nil, ErrClosed)", b, err)
		}
	})

	t.Run("closed unfinished canceled caller", func(t *testing.T) {
		b, err := decide(true, false, payload, nil, context.Canceled)
		if !errors.Is(err, ErrClosed) || errors.Is(err, context.Canceled) || b != nil {
			t.Fatalf("got (%q, %v), want (nil, ErrClosed)", b, err)
		}
	})

	t.Run("closed finished canceled caller", func(t *testing.T) {
		b, err := decide(true, true, payload, nil, context.Canceled)
		if !errors.Is(err, ErrClosed) || b != nil {
			t.Fatalf("got (%q, %v), want (nil, ErrClosed)", b, err)
		}
	})

	t.Run("finished beats canceled caller", func(t *testing.T) {
		b, err := decide(false, true, payload, nil, context.Canceled)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !bytes.Equal(b, payload) {
			t.Fatalf("got %q, want %q", b, payload)
		}
	})

	t.Run("finished clones", func(t *testing.T) {
		in := []byte("payload")
		b, err := decide(false, true, in, nil, nil)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !bytes.Equal(b, in) {
			t.Fatalf("got %q, want %q", b, in)
		}
		if len(b) > 0 {
			b[0] = 'X'
		}
		if string(in) != "payload" {
			t.Fatalf("input mutated to %q", in)
		}
	})

	t.Run("finished load error", func(t *testing.T) {
		b, err := decide(false, true, payload, sentinel, nil)
		if !errors.Is(err, sentinel) || b != nil {
			t.Fatalf("got (%q, %v), want (nil, sentinel)", b, err)
		}
	})

	t.Run("caller canceled while in flight", func(t *testing.T) {
		b, err := decide(false, false, nil, nil, context.Canceled)
		if !errors.Is(err, context.Canceled) || b != nil {
			t.Fatalf("got (%q, %v), want (nil, context.Canceled)", b, err)
		}
	})

	t.Run("non-conforming context panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("decide did not panic")
			}
		}()
		decide(false, false, nil, nil, nil)
	})
}

// TestOrphanSuccessPopulates covers an orphaned success: the only waiter
// cancels, the Loader still completes, and a later Get is a hit.
func TestOrphanSuccessPopulates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		}
		c := New(1, load)
		t.Cleanup(func() { c.Close() })

		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		bc := make(chan []byte, 1)
		go func() {
			b, err := c.Get(ctx, "k")
			bc <- b
			errc <- err
		}()

		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Errorf("Loader entered %d times, want 1", got)
		}
		cancel()

		b := <-bc
		err := <-errc
		if !errors.Is(err, context.Canceled) || b != nil {
			t.Fatalf("canceled Get = (%q, %v), want (nil, context.Canceled)", b, err)
		}

		close(release)
		synctest.Wait()

		live, cancelLive := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelLive()
		got, err := c.Get(live, "k")
		if err != nil {
			t.Fatalf("hit Get: %v", err)
		}
		if string(got) != "v" {
			t.Fatalf("hit Get = %q, want %q", got, "v")
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("Loader called %d times, want 1", n)
		}
	})
}

// TestOrphanErrorDoesNotPopulate covers an orphaned error: nothing is stored,
// and a later Get calls the Loader again.
func TestOrphanErrorDoesNotPopulate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("boom")
		var calls atomic.Int32
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			if calls.Add(1) == 1 {
				<-release
				return nil, boom
			}
			return []byte("ok"), nil
		}
		c := New(1, load)
		t.Cleanup(func() { c.Close() })

		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		bc := make(chan []byte, 1)
		go func() {
			b, err := c.Get(ctx, "k")
			bc <- b
			errc <- err
		}()

		synctest.Wait()
		cancel()

		b := <-bc
		err := <-errc
		if !errors.Is(err, context.Canceled) || b != nil {
			t.Fatalf("canceled Get = (%q, %v), want (nil, context.Canceled)", b, err)
		}

		close(release)
		synctest.Wait()

		live, cancelLive := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelLive()
		got, err := c.Get(live, "k")
		if err != nil {
			t.Fatalf("reload Get: %v", err)
		}
		if string(got) != "ok" {
			t.Fatalf("reload Get = %q, want %q", got, "ok")
		}
		if n := calls.Load(); n != 2 {
			t.Fatalf("Loader called %d times, want 2", n)
		}
	})
}

// TestPartialCancel covers one of several waiters leaving while the load continues.
func TestPartialCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		ctx1, cancel1 := context.WithCancel(t.Context())
		ctx2, cancel2 := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel2()

		type result struct {
			b   []byte
			err error
		}
		r1 := make(chan result, 1)
		r2 := make(chan result, 1)
		go func() {
			b, err := c.Get(ctx1, "k")
			r1 <- result{b, err}
		}()
		go func() {
			b, err := c.Get(ctx2, "k")
			r2 <- result{b, err}
		}()

		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Errorf("Loader entered %d times, want 1", got)
		}
		cancel1()
		got1 := <-r1
		if !errors.Is(got1.err, context.Canceled) || got1.b != nil {
			t.Fatalf("canceled waiter = (%q, %v), want (nil, context.Canceled)", got1.b, got1.err)
		}

		close(release)
		got2 := <-r2
		if got2.err != nil {
			t.Fatalf("surviving waiter: %v", got2.err)
		}
		if string(got2.b) != "v" {
			t.Fatalf("surviving waiter = %q, want %q", got2.b, "v")
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("Loader called %d times, want 1", n)
		}

		live, cancelLive := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelLive()
		hit, err := c.Get(live, "k")
		if err != nil {
			t.Fatalf("following Get: %v", err)
		}
		if string(hit) != "v" {
			t.Fatalf("following Get = %q, want %q", hit, "v")
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("Loader called %d times after hit, want 1", n)
		}
	})
}

// TestCloseJoinsDiscardsAndWakes covers Close during an in-flight load with a live waiter.
func TestCloseJoinsDiscardsAndWakes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		type result struct {
			b   []byte
			err error
		}
		gotc := make(chan result, 1)
		go func() {
			b, err := c.Get(ctx, "k")
			gotc <- result{b, err}
		}()

		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Errorf("Loader entered %d times, want 1", got)
		}

		closec := make(chan error, 1)
		go func() {
			closec <- c.Close()
		}()
		synctest.Wait()

		select {
		case err := <-closec:
			t.Errorf("Close returned %v while Loader still blocked, want still joining", err)
		default:
		}
		got := <-gotc
		if !errors.Is(got.err, ErrClosed) || got.b != nil {
			t.Errorf("Get during Close = (%q, %v), want (nil, ErrClosed)", got.b, got.err)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("Loader called %d times, want 1", n)
		}
		if l := c.Len(); l != 0 {
			t.Errorf("Len() = %d during join, want 0", l)
		}

		close(release)
		if err := <-closec; err != nil {
			t.Fatalf("Close: %v, want nil", err)
		}
		if l := c.Len(); l != 0 {
			t.Fatalf("Len() after Close = %d, want 0", l)
		}
		b, err := c.Get(ctx, "k")
		if !errors.Is(err, ErrClosed) || b != nil {
			t.Fatalf("Get after Close = (%q, %v), want (nil, ErrClosed)", b, err)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("Loader called %d times, want 1", n)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("second Close: %v, want nil", err)
		}
	})
}

// TestCloseWithNoWaitersStillJoins covers Close after the only waiter has left.
func TestCloseWithNoWaitersStillJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		ctx, cancel := context.WithCancel(t.Context())
		type result struct {
			b   []byte
			err error
		}
		gotc := make(chan result, 1)
		go func() {
			b, err := c.Get(ctx, "k")
			gotc <- result{b, err}
		}()

		synctest.Wait()
		cancel()
		got := <-gotc
		if !errors.Is(got.err, context.Canceled) || got.b != nil {
			t.Fatalf("canceled Get = (%q, %v), want (nil, context.Canceled)", got.b, got.err)
		}

		closec := make(chan error, 1)
		go func() {
			closec <- c.Close()
		}()
		synctest.Wait()

		select {
		case err := <-closec:
			t.Errorf("Close returned %v while Loader still blocked, want still joining", err)
		default:
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("Loader called %d times, want 1", n)
		}

		close(release)
		if err := <-closec; err != nil {
			t.Fatalf("Close: %v, want nil", err)
		}
		if l := c.Len(); l != 0 {
			t.Fatalf("Len() after Close = %d, want 0", l)
		}
		live, cancelLive := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelLive()
		b, err := c.Get(live, "k")
		if !errors.Is(err, ErrClosed) || b != nil {
			t.Fatalf("Get after Close = (%q, %v), want (nil, ErrClosed)", b, err)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("Loader called %d times, want 1 (result must be discarded)", n)
		}
	})
}

func TestErrorWithBytes(t *testing.T) {
	ctx := testCtx(t)
	sentinel := errors.New("nope")
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		if calls.Add(1) == 1 {
			return []byte("nope"), sentinel
		}
		return []byte("ok"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	b, err := c.Get(ctx, "k")
	if !errors.Is(err, sentinel) || b != nil {
		t.Fatalf("first Get = (%q, %v), want (nil, sentinel)", b, err)
	}
	b, err = c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if string(b) != "ok" {
		t.Fatalf("second Get = %q, want %q", b, "ok")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("Loader called %d times, want 2", n)
	}
}

func TestNilAndEmptySuccess(t *testing.T) {
	ctx := testCtx(t)

	t.Run("nil", func(t *testing.T) {
		var calls atomic.Int32
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			return nil, nil
		}
		c := New(1, load)
		t.Cleanup(func() { c.Close() })
		for i := 0; i < 2; i++ {
			b, err := c.Get(ctx, "k")
			if err != nil {
				t.Fatalf("Get %d: %v", i, err)
			}
			if b != nil {
				t.Fatalf("Get %d = %q, want nil", i, b)
			}
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("Loader called %d times, want 1", n)
		}
		if l := c.Len(); l != 1 {
			t.Fatalf("Len() = %d, want 1", l)
		}
	})

	t.Run("empty", func(t *testing.T) {
		var calls atomic.Int32
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			return []byte{}, nil
		}
		c := New(1, load)
		t.Cleanup(func() { c.Close() })
		for i := 0; i < 2; i++ {
			b, err := c.Get(ctx, "k")
			if err != nil {
				t.Fatalf("Get %d: %v", i, err)
			}
			if b == nil || len(b) != 0 {
				t.Fatalf("Get %d = %q (nil=%v), want non-nil empty", i, b, b == nil)
			}
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("Loader called %d times, want 1", n)
		}
		if l := c.Len(); l != 1 {
			t.Fatalf("Len() = %d, want 1", l)
		}
	})
}

func TestCapacityOne(t *testing.T) {
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
	c := New(1, load)
	t.Cleanup(func() { c.Close() })

	mustGet(t, ctx, c, "a")
	mustGet(t, ctx, c, "b")
	mustGet(t, ctx, c, "a")
	mustGet(t, ctx, c, "b")

	if got := count("a"); got != 2 {
		t.Fatalf("Loader(a) called %d times, want 2", got)
	}
	if got := count("b"); got != 2 {
		t.Fatalf("Loader(b) called %d times, want 2", got)
	}
	if l := c.Len(); l != 1 {
		t.Fatalf("Len() = %d, want 1", l)
	}
}

func TestInFlightDoesNotConsumeCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		counts := map[string]int{}
		rel := map[string]chan struct{}{
			"a": make(chan struct{}),
			"b": make(chan struct{}),
		}
		load := func(_ context.Context, key string) ([]byte, error) {
			mu.Lock()
			counts[key]++
			mu.Unlock()
			<-rel[key]
			return []byte("v:" + key), nil
		}
		count := func(key string) int {
			mu.Lock()
			defer mu.Unlock()
			return counts[key]
		}
		c := New(1, load)
		t.Cleanup(func() { c.Close() })

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		doneA := make(chan error, 1)
		doneB := make(chan error, 1)
		go func() {
			_, err := c.Get(ctx, "a")
			doneA <- err
		}()
		go func() {
			_, err := c.Get(ctx, "b")
			doneB <- err
		}()

		synctest.Wait()
		if got := count("a"); got != 1 {
			t.Errorf("Loader(a) entered %d times, want 1", got)
		}
		if got := count("b"); got != 1 {
			t.Errorf("Loader(b) entered %d times, want 1", got)
		}
		if l := c.Len(); l != 0 {
			t.Errorf("Len() while both in flight = %d, want 0", l)
		}

		close(rel["a"])
		if err := <-doneA; err != nil {
			t.Fatalf("Get(a): %v", err)
		}
		if l := c.Len(); l != 1 {
			t.Fatalf("Len() after a admitted = %d, want 1", l)
		}
		if got := count("a"); got != 1 {
			t.Fatalf("Loader(a) = %d, want 1", got)
		}
		select {
		case err := <-doneB:
			t.Fatalf("Get(b) returned %v before release", err)
		default:
		}

		close(rel["b"])
		if err := <-doneB; err != nil {
			t.Fatalf("Get(b): %v", err)
		}
		if l := c.Len(); l != 1 {
			t.Fatalf("Len() after b admitted = %d, want 1", l)
		}
		if got := count("b"); got != 1 {
			t.Fatalf("Loader(b) = %d, want 1", got)
		}

		mustGet(t, ctx, c, "b")
		if got := count("b"); got != 1 {
			t.Fatalf("Get(b) reloaded, count = %d, want 1", got)
		}
		if l := c.Len(); l != 1 {
			t.Fatalf("Len() after hit b = %d, want 1", l)
		}
		mustGet(t, ctx, c, "a")
		if got := count("a"); got != 2 {
			t.Fatalf("Loader(a) = %d, want 2", got)
		}
		if got := count("b"); got != 1 {
			t.Fatalf("Loader(b) = %d, want 1", got)
		}
		if l := c.Len(); l != 1 {
			t.Fatalf("Len() = %d, want 1", l)
		}
	})
}

func TestLongerLRUSequence(t *testing.T) {
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
	c := New(3, load)
	t.Cleanup(func() { c.Close() })

	for _, k := range []string{"a", "b", "c", "a", "d", "c", "e"} {
		got := mustGet(t, ctx, c, k)
		if string(got) != "v:"+k {
			t.Fatalf("Get(%q) = %q, want %q", k, got, "v:"+k)
		}
	}
	if l := c.Len(); l != 3 {
		t.Fatalf("Len() = %d, want 3", l)
	}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		if got := count(k); got != 1 {
			t.Fatalf("after sequence Loader(%s) = %d, want 1", k, got)
		}
	}

	for _, k := range []string{"d", "c", "e"} {
		got := mustGet(t, ctx, c, k)
		if string(got) != "v:"+k {
			t.Fatalf("probe Get(%q) = %q, want %q", k, got, "v:"+k)
		}
	}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		if got := count(k); got != 1 {
			t.Fatalf("after probe Loader(%s) = %d, want 1", k, got)
		}
	}
	if l := c.Len(); l != 3 {
		t.Fatalf("Len() after probe = %d, want 3", l)
	}

	mustGet(t, ctx, c, "a")
	mustGet(t, ctx, c, "b")
	want := map[string]int{"a": 2, "b": 2, "c": 1, "d": 1, "e": 1}
	for k, n := range want {
		if got := count(k); got != n {
			t.Fatalf("Loader(%s) = %d, want %d", k, got, n)
		}
	}
	if l := c.Len(); l != 3 {
		t.Fatalf("final Len() = %d, want 3", l)
	}
}

func TestDistinctBackingArrays(t *testing.T) {
	ctx := testCtx(t)
	load := func(_ context.Context, key string) ([]byte, error) {
		return []byte("payload"), nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })

	b1 := mustGet(t, ctx, c, "k")
	b2 := mustGet(t, ctx, c, "k")
	b1[0] = 'X'
	if string(b2) != "payload" {
		t.Fatalf("second Get = %q after mutating first, want payload", b2)
	}
	b2[0] = 'Y'
	b3 := mustGet(t, ctx, c, "k")
	if string(b3) != "payload" {
		t.Fatalf("third Get = %q, want payload", b3)
	}
}

func TestCoalescedWaitersPrivateCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("payload"), nil
		}
		c := New(1, load)
		t.Cleanup(func() { c.Close() })

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		type result struct {
			b   []byte
			err error
		}
		r1 := make(chan result, 1)
		r2 := make(chan result, 1)
		go func() {
			b, err := c.Get(ctx, "k")
			r1 <- result{b, err}
		}()
		go func() {
			b, err := c.Get(ctx, "k")
			r2 <- result{b, err}
		}()

		synctest.Wait()
		close(release)
		g1 := <-r1
		g2 := <-r2
		if g1.err != nil || g2.err != nil {
			t.Fatalf("Gets: %v, %v", g1.err, g2.err)
		}
		if !bytes.Equal(g1.b, g2.b) || string(g1.b) != "payload" {
			t.Fatalf("coalesced results %q and %q, want payload", g1.b, g2.b)
		}
		g1.b[0] = 'X'
		if string(g2.b) != "payload" {
			t.Fatalf("second coalesced result = %q after mutating first", g2.b)
		}
		g3 := mustGet(t, ctx, c, "k")
		if string(g3) != "payload" {
			t.Fatalf("third Get = %q, want payload", g3)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("Loader called %d times, want 1", n)
		}
	})
}

func TestConcurrentCloseIdle(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		calls.Add(1)
		t.Errorf("Loader called on idle Close")
		return nil, errors.New("unexpected")
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

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
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Close[%d] = %v, want nil", i, err)
		}
	}
	b, err := c.Get(ctx, "k")
	if !errors.Is(err, ErrClosed) || b != nil {
		t.Fatalf("Get after Close = (%q, %v), want (nil, ErrClosed)", b, err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("Loader called %d times, want 0", n)
	}
}

func TestGetDuringJoinWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("v"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		type result struct {
			b   []byte
			err error
		}
		first := make(chan result, 1)
		go func() {
			b, err := c.Get(ctx, "k")
			first <- result{b, err}
		}()
		synctest.Wait()

		closec := make(chan error, 1)
		go func() {
			closec <- c.Close()
		}()
		synctest.Wait()

		got := <-first
		if !errors.Is(got.err, ErrClosed) || got.b != nil {
			t.Errorf("first Get = (%q, %v), want (nil, ErrClosed)", got.b, got.err)
		}

		second := make(chan result, 1)
		go func() {
			b, err := c.Get(ctx, "other")
			second <- result{b, err}
		}()
		got2 := <-second
		if !errors.Is(got2.err, ErrClosed) || got2.b != nil {
			t.Errorf("Get during join = (%q, %v), want (nil, ErrClosed)", got2.b, got2.err)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("Loader called %d times, want 1", n)
		}
		select {
		case err := <-closec:
			t.Errorf("Close returned %v while Loader still blocked", err)
		default:
		}
		if l := c.Len(); l != 0 {
			t.Errorf("Len() = %d, want 0", l)
		}

		close(release)
		if err := <-closec; err != nil {
			t.Fatalf("Close: %v, want nil", err)
		}
	})
}

func TestSecondCloseBlocksWhileJoining(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			<-release
			return []byte("v"), nil
		}
		c := New(1, load)
		t.Cleanup(func() { c.Close() })

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		go func() {
			_, _ = c.Get(ctx, "k")
		}()
		synctest.Wait()

		close1 := make(chan error, 1)
		go func() {
			close1 <- c.Close()
		}()
		synctest.Wait()
		select {
		case err := <-close1:
			t.Errorf("Close #1 returned %v while Loader blocked, want still joining", err)
		default:
		}

		close2 := make(chan error, 1)
		go func() {
			close2 <- c.Close()
		}()
		synctest.Wait()
		select {
		case err := <-close1:
			t.Errorf("Close #1 returned %v before release", err)
		default:
		}
		select {
		case err := <-close2:
			t.Errorf("Close #2 returned %v before release, want blocked on joined", err)
		default:
		}

		close(release)
		if err := <-close1; err != nil {
			t.Fatalf("Close #1: %v, want nil", err)
		}
		if err := <-close2; err != nil {
			t.Fatalf("Close #2: %v, want nil", err)
		}
	})
}

func TestStress(t *testing.T) {
	ctx := testCtx(t)
	const (
		capacity = 4
		keys     = 16
		workers  = 32
	)
	loadErr := errors.New("transient")
	var calls atomic.Int32
	load := func(ctx context.Context, key string) ([]byte, error) {
		n := calls.Add(1)
		if n%17 == 0 {
			return []byte("ignored"), loadErr
		}
		return []byte("v:" + key), nil
	}
	c := New(capacity, load)
	t.Cleanup(func() { c.Close() })

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				key := string(rune('a' + (i+j)%keys))
				b, err := c.Get(ctx, key)
				if err != nil && !errors.Is(err, ErrClosed) && !errors.Is(err, loadErr) {
					t.Errorf("Get(%q) unexpected error %v", key, err)
					return
				}
				if err != nil && b != nil {
					t.Errorf("Get(%q) error %v with non-nil bytes %q", key, err, b)
					return
				}
				if l := c.Len(); l > capacity {
					t.Errorf("Len() = %d, want <= %d", l, capacity)
					return
				}
			}
		}(i)
	}

	// Let some Gets overlap, then close while others are still running.
	time.Sleep(5 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	if l := c.Len(); l != 0 {
		t.Fatalf("Len() after Close = %d, want 0", l)
	}
}
