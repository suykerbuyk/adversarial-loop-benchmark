package blockcache

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
)

func TestOrphanedLoadPopulatesCacheAndCopies(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	backing := []byte("value")
	c := New(2, func(context.Context, string) ([]byte, error) {
		close(entered)
		<-release
		return backing, nil
	})
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { _, err := c.Get(ctx, "k"); got <- err }()
	<-entered
	cancel()
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Fatalf("Get error = %v", err)
	}
	close(release)
	for c.Len() == 0 {
		runtime.Gosched()
	}
	backing[0] = 'X'
	b, err := c.Get(context.Background(), "k")
	if err != nil || string(b) != "value" {
		t.Fatalf("cached result = %q, %v", b, err)
	}
	b[0] = 'Y'
	b2, _ := c.Get(context.Background(), "k")
	if string(b2) != "value" {
		t.Fatalf("mutation leaked: %q", b2)
	}
}

func TestCloseJoinsLoadersAndDiscardsLateSuccess(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	c := New(1, func(context.Context, string) ([]byte, error) {
		close(entered)
		<-release // deliberately ignores cancellation
		return []byte("late"), nil
	})
	getErr := make(chan error, 1)
	go func() { _, err := c.Get(context.Background(), "k"); getErr <- err }()
	<-entered
	closeOne, closeTwo := make(chan struct{}), make(chan struct{})
	go func() { c.Close(); close(closeOne) }()
	go func() { c.Close(); close(closeTwo) }()
	if err := <-getErr; !errors.Is(err, ErrClosed) {
		t.Fatalf("Get error = %v", err)
	}
	select {
	case <-closeOne:
		t.Fatal("Close returned before Loader")
	default:
	}
	select {
	case <-closeTwo:
		t.Fatal("concurrent Close returned before Loader")
	default:
	}
	close(release)
	<-closeOne
	<-closeTwo
	if c.Len() != 0 {
		t.Fatalf("Len after Close = %d", c.Len())
	}
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close = %v", err)
	}
}

func TestErrorWinsAndEmptySuccessIsNonNil(t *testing.T) {
	boom := errors.New("failure")
	calls := 0
	c := New(2, func(context.Context, string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte("discard"), boom
		}
		return nil, nil
	})
	defer c.Close()
	if b, err := c.Get(context.Background(), "k"); !errors.Is(err, boom) || b != nil {
		t.Fatalf("error result = %q, %v", b, err)
	}
	b, err := c.Get(context.Background(), "k")
	if err != nil || b == nil || len(b) != 0 {
		t.Fatalf("empty result = %#v, %v", b, err)
	}
	b2, err := c.Get(context.Background(), "k")
	if err != nil || b2 == nil || len(b2) != 0 {
		t.Fatalf("empty hit = %#v, %v", b2, err)
	}
}

func TestGoexitDoesNotStrandFlight(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	c := New(1, func(context.Context, string) ([]byte, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			runtime.Goexit()
		}
		return []byte("ok"), nil
	})
	defer c.Close()
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, errLoaderAborted) {
		t.Fatalf("Goexit result error = %v", err)
	}
	if b, err := c.Get(context.Background(), "k"); err != nil || string(b) != "ok" {
		t.Fatalf("retry = %q, %v", b, err)
	}
}
