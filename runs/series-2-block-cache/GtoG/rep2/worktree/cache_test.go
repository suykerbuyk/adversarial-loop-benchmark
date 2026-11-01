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
	"unsafe"
)

func TestCloneBytesNilVersusEmpty(t *testing.T) {
	if cloneBytes(nil) != nil {
		t.Fatal("cloneBytes(nil) must stay nil")
	}
	empty := make([]byte, 0, 4)
	got := cloneBytes(empty)
	if got == nil {
		t.Fatal("cloneBytes(non-nil empty) must be non-nil")
	}
	if len(got) != 0 {
		t.Fatalf("cloneBytes(non-nil empty) len = %d, want 0", len(got))
	}
	if cap(got) != 0 {
		t.Fatalf("cloneBytes cap = %d, want 0 (must not retain the Loader slice)", cap(got))
	}
	if unsafe.SliceData(got) == unsafe.SliceData(empty) {
		t.Fatal("cloneBytes reused the input data pointer")
	}
}

func TestNilVersusEmptyPayload(t *testing.T) {
	ctx := testCtx(t)

	t.Run("non-nil empty", func(t *testing.T) {
		var calls atomic.Int32
		load := func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			return []byte{}, nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		b1, err := c.Get(ctx, "e")
		if err != nil {
			t.Fatalf("first Get: %v", err)
		}
		if b1 == nil || len(b1) != 0 {
			t.Fatalf("first Get = %v (nil=%v), want non-nil empty", b1, b1 == nil)
		}
		b2, err := c.Get(ctx, "e")
		if err != nil {
			t.Fatalf("second Get: %v", err)
		}
		if b2 == nil || len(b2) != 0 {
			t.Fatalf("second Get = %v (nil=%v), want non-nil empty", b2, b2 == nil)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader called %d times, want 1", got)
		}
	})

	t.Run("nil success", func(t *testing.T) {
		var calls atomic.Int32
		load := func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			return nil, nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		b1, err := c.Get(ctx, "n")
		if err != nil {
			t.Fatalf("first Get: %v", err)
		}
		if b1 != nil {
			t.Fatalf("first Get = %q, want nil", b1)
		}
		b2, err := c.Get(ctx, "n")
		if err != nil {
			t.Fatalf("second Get: %v", err)
		}
		if b2 != nil {
			t.Fatalf("second Get = %q, want nil", b2)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader called %d times, want 1", got)
		}
	})

	t.Run("error discards bytes", func(t *testing.T) {
		boom := errors.New("nope")
		var calls atomic.Int32
		load := func(context.Context, string) ([]byte, error) {
			n := calls.Add(1)
			if n == 1 {
				return []byte("x"), boom
			}
			return []byte("y"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		b, err := c.Get(ctx, "bad")
		if !errors.Is(err, boom) {
			t.Fatalf("first Get error = %v, want %v", err, boom)
		}
		if b != nil {
			t.Fatalf("first Get bytes = %q, want nil", b)
		}
		b, err = c.Get(ctx, "bad")
		if err != nil {
			t.Fatalf("second Get: %v", err)
		}
		if string(b) != "y" {
			t.Fatalf("second Get = %q, want %q", b, "y")
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("Loader called %d times, want 2", got)
		}
	})
}

func TestRule7bAlreadyDoneWhileInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		var calls atomic.Int32
		release := make(chan struct{})
		load := func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("block"), nil
		}
		c := New(4, load)
		t.Cleanup(func() { c.Close() })

		var leaderErr error
		var leaderBytes []byte
		done := make(chan struct{})
		go func() {
			leaderBytes, leaderErr = c.Get(ctx, "k")
			close(done)
		}()

		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader entered %d times before cancelled Get, want 1", got)
		}

		dead, deadCancel := context.WithCancel(ctx)
		deadCancel()
		b, err := c.Get(dead, "k")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Get error = %v, want context.Canceled", err)
		}
		if b != nil {
			t.Fatalf("cancelled Get bytes = %q, want nil", b)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader entered %d times, want 1 (must not start a second load)", got)
		}

		close(release)
		<-done
		if leaderErr != nil {
			t.Fatalf("leader Get: %v", leaderErr)
		}
		if string(leaderBytes) != "block" {
			t.Fatalf("leader Get = %q, want %q", leaderBytes, "block")
		}

		hit, err := c.Get(ctx, "k")
		if err != nil {
			t.Fatalf("later Get: %v", err)
		}
		if string(hit) != "block" {
			t.Fatalf("later Get = %q, want %q", hit, "block")
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader entered %d times after admit, want 1", got)
		}
	})
}

func TestRule7bCtxDoneWhileBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		var calls atomic.Int32
		release := make(chan struct{})
		load := func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("block"), nil
		}
		c := New(4, load)
		t.Cleanup(func() { c.Close() })

		waitCtx, waitCancel := context.WithCancel(ctx)
		var waiterErr error
		var waiterBytes []byte
		waiterDone := make(chan struct{})
		go func() {
			waiterBytes, waiterErr = c.Get(waitCtx, "k")
			close(waiterDone)
		}()

		liveCtx := ctx
		var siblingErr error
		var siblingBytes []byte
		siblingDone := make(chan struct{})
		go func() {
			siblingBytes, siblingErr = c.Get(liveCtx, "k")
			close(siblingDone)
		}()

		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader entered %d times, want 1", got)
		}

		waitCancel()
		<-waiterDone
		if !errors.Is(waiterErr, context.Canceled) {
			t.Fatalf("cancelled waiter error = %v, want context.Canceled", waiterErr)
		}
		if waiterBytes != nil {
			t.Fatalf("cancelled waiter bytes = %q, want nil", waiterBytes)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader entered %d times after cancel, want 1", got)
		}

		close(release)
		<-siblingDone
		if siblingErr != nil {
			t.Fatalf("sibling Get: %v", siblingErr)
		}
		if string(siblingBytes) != "block" {
			t.Fatalf("sibling Get = %q, want %q", siblingBytes, "block")
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader entered %d times, want 1", got)
		}
	})
}

func TestAlreadyCancelledColdKey(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	load := func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("nope"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	dead, cancel := context.WithCancel(ctx)
	cancel()
	b, err := c.Get(dead, "cold")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Get error = %v, want context.Canceled", err)
	}
	if b != nil {
		t.Fatalf("Get bytes = %q, want nil", b)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("Loader called %d times, want 0", got)
	}
}

func TestAlreadyCancelledResidentKey(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	load := func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("payload"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	if _, err := c.Get(ctx, "a"); err != nil {
		t.Fatalf("warm Get: %v", err)
	}
	dead, cancel := context.WithCancel(ctx)
	cancel()
	b, err := c.Get(dead, "a")
	if err != nil {
		t.Fatalf("cancelled hit error = %v, want nil", err)
	}
	if string(b) != "payload" {
		t.Fatalf("cancelled hit = %q, want %q", b, "payload")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Loader called %d times, want 1", got)
	}
}

func TestDistinctCopies(t *testing.T) {
	ctx := testCtx(t)
	load := func(context.Context, string) ([]byte, error) {
		return []byte("abcdef"), nil
	}
	c := New(2, load)
	t.Cleanup(func() { c.Close() })

	b1, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	b2, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if len(b1) == 0 || len(b2) == 0 {
		t.Fatal("expected non-empty copies")
	}
	if unsafe.SliceData(b1) == unsafe.SliceData(b2) {
		t.Fatal("two Gets share a backing array")
	}
	b1[0] = 'Z'
	if b2[0] == 'Z' {
		t.Fatal("mutating one copy changed the other")
	}
	b3, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("third Get: %v", err)
	}
	if string(b3) != "abcdef" {
		t.Fatalf("later hit = %q, want %q", b3, "abcdef")
	}
}

func TestOrphanAdmit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		var calls atomic.Int32
		release := make(chan struct{})
		load := func(context.Context, string) ([]byte, error) {
			calls.Add(1)
			<-release
			return []byte("kept"), nil
		}
		c := New(2, load)
		t.Cleanup(func() { c.Close() })

		waitCtx, waitCancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			_, _ = c.Get(waitCtx, "k")
			close(done)
		}()
		synctest.Wait()
		waitCancel()
		<-done

		close(release)
		synctest.Wait()

		b, err := c.Get(ctx, "k")
		if err != nil {
			t.Fatalf("follow-up Get: %v", err)
		}
		if string(b) != "kept" {
			t.Fatalf("follow-up Get = %q, want %q", b, "kept")
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Loader called %d times, want 1", got)
		}
	})
}

func TestCloseDuringInFlight(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	started := make(chan struct{})
	load := func(lctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		close(started)
		<-lctx.Done()
		return []byte("late:" + key), lctx.Err()
	}
	c := New(2, load)

	var wg sync.WaitGroup
	const n = 8
	errs := make([]error, n)
	results := make([][]byte, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.Get(ctx, "k")
		}(i)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("loader did not start")
	}

	var closeErrs [2]error
	var closeWG sync.WaitGroup
	closeWG.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer closeWG.Done()
			closeErrs[i] = c.Close()
		}(i)
	}
	closeWG.Wait()
	for i, err := range closeErrs {
		if err != nil {
			t.Fatalf("Close[%d] = %v, want nil", i, err)
		}
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if !errors.Is(errs[i], ErrClosed) {
			t.Fatalf("Get[%d] error = %v, want ErrClosed", i, errs[i])
		}
		if errors.Is(errs[i], context.Canceled) && !errors.Is(ErrClosed, context.Canceled) {
			t.Fatalf("Get[%d] returned context.Canceled", i)
		}
		if results[i] != nil {
			t.Fatalf("Get[%d] bytes = %q, want nil", i, results[i])
		}
	}
	b, err := c.Get(ctx, "k")
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("follow-up Get error = %v, want ErrClosed", err)
	}
	if b != nil {
		t.Fatalf("follow-up Get bytes = %q, want nil", b)
	}
	if c.Len() != 0 {
		t.Fatalf("Len after Close = %d, want 0", c.Len())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Loader called %d times, want 1", got)
	}
}

func TestCloseOverlapsHit(t *testing.T) {
	ctx := testCtx(t)
	load := func(context.Context, string) ([]byte, error) {
		return []byte("hit"), nil
	}
	c := New(2, load)
	if _, err := c.Get(ctx, "a"); err != nil {
		t.Fatalf("warm: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var getErr error
	var getBytes []byte
	go func() {
		defer wg.Done()
		getBytes, getErr = c.Get(ctx, "a")
	}()
	var closeErr error
	go func() {
		defer wg.Done()
		closeErr = c.Close()
	}()
	wg.Wait()
	if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	if getErr != nil && !errors.Is(getErr, ErrClosed) {
		t.Fatalf("Get error = %v, want nil or ErrClosed", getErr)
	}
	if getErr == nil && string(getBytes) != "hit" {
		t.Fatalf("Get = %q, want %q", getBytes, "hit")
	}
	if c.Len() != 0 {
		t.Fatalf("Len after Close = %d, want 0", c.Len())
	}
}

func TestCapacityOne(t *testing.T) {
	ctx := testCtx(t)
	var mu sync.Mutex
	counts := map[string]int{}
	load := func(_ context.Context, key string) ([]byte, error) {
		mu.Lock()
		counts[key]++
		mu.Unlock()
		return []byte(key), nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })

	mustGet(t, ctx, c, "a")
	mustGet(t, ctx, c, "b")
	mustGet(t, ctx, c, "a")

	mu.Lock()
	defer mu.Unlock()
	if counts["a"] != 2 {
		t.Fatalf("Loader(a) = %d, want 2", counts["a"])
	}
	if counts["b"] != 1 {
		t.Fatalf("Loader(b) = %d, want 1", counts["b"])
	}
}

func TestManyInFlightAboveCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			<-release
			return []byte(key), nil
		}
		const capN = 3
		const keys = 12
		c := New(capN, load)
		t.Cleanup(func() { c.Close() })

		var wg sync.WaitGroup
		errs := make([]error, keys)
		wg.Add(keys)
		for i := 0; i < keys; i++ {
			go func(i int) {
				defer wg.Done()
				_, errs[i] = c.Get(ctx, string(rune('a'+i)))
			}(i)
		}
		synctest.Wait()
		if l := c.Len(); l != 0 {
			t.Fatalf("Len during in-flight = %d, want 0", l)
		}
		close(release)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("Get[%d]: %v", i, err)
			}
		}
		if l := c.Len(); l != capN {
			t.Fatalf("Len = %d, want %d", l, capN)
		}
	})
}

func TestLenDuringInFlightMiss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		release := make(chan struct{})
		load := func(_ context.Context, key string) ([]byte, error) {
			if key == "miss" {
				<-release
			}
			return []byte(key), nil
		}
		c := New(4, load)
		t.Cleanup(func() { c.Close() })

		if _, err := c.Get(ctx, "resident"); err != nil {
			t.Fatalf("warm: %v", err)
		}
		done := make(chan struct{})
		go func() {
			_, _ = c.Get(ctx, "miss")
			close(done)
		}()
		synctest.Wait()
		if l := c.Len(); l != 1 {
			t.Fatalf("Len during miss = %d, want 1", l)
		}
		close(release)
		<-done
		if l := c.Len(); l != 2 {
			t.Fatalf("Len after admit = %d, want 2", l)
		}
	})
}

func TestEmptyStringKey(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	load := func(_ context.Context, key string) ([]byte, error) {
		calls.Add(1)
		return []byte("empty-key:" + key), nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })

	b1 := mustGet(t, ctx, c, "")
	b2 := mustGet(t, ctx, c, "")
	if string(b1) != "empty-key:" || string(b2) != "empty-key:" {
		t.Fatalf("Gets = %q, %q", b1, b2)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Loader called %d times, want 1", got)
	}
}

func TestLoaderBytesNotRetained(t *testing.T) {
	ctx := testCtx(t)
	raw := []byte("mutable")
	load := func(context.Context, string) ([]byte, error) {
		return raw, nil
	}
	c := New(1, load)
	t.Cleanup(func() { c.Close() })

	b, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	raw[0] = 'X'
	if string(b) != "mutable" {
		t.Fatalf("returned copy changed after Loader mutation: %q", b)
	}
	b2, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("hit: %v", err)
	}
	if !bytes.Equal(b2, []byte("mutable")) {
		t.Fatalf("cached copy changed after Loader mutation: %q", b2)
	}
}
