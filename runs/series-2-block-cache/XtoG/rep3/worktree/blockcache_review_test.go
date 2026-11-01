package blockcache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCanceledWaiterGetsErrClosedWhenCloseStarts(t *testing.T) {
	entered := make(chan struct{})
	loaderCanceled := make(chan struct{})
	release := make(chan struct{})
	c := New(1, func(ctx context.Context, _ string) ([]byte, error) {
		close(entered)
		<-ctx.Done()
		close(loaderCanceled)
		<-release
		return []byte("late"), nil
	})

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := c.Get(waiterCtx, "key")
		result <- err
	}()
	<-entered

	closeReturned := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closeReturned)
	}()
	<-loaderCanceled
	cancelWaiter()
	select {
	case err := <-result:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Get error = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get did not wake on Close")
	}
	close(release)
	select {
	case <-closeReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join loader")
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("Len after Close = %d, want 0", got)
	}
}

func TestOrphanLoadPopulatesCacheAndReturnsCopies(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c := New(2, func(context.Context, string) ([]byte, error) {
		once.Do(func() { close(entered) })
		<-release
		return []byte("value"), nil
	})
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, "key")
		first <- err
	}()
	<-entered
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Get error = %v, want context.Canceled", err)
	}
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for c.Len() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got, err := c.Get(context.Background(), "key")
	if err != nil || string(got) != "value" {
		t.Fatalf("Get after orphan load = %q, %v; want value, nil", got, err)
	}
	got[0] = 'X'
	again, err := c.Get(context.Background(), "key")
	if err != nil || string(again) != "value" {
		t.Fatalf("cached Get after mutation = %q, %v; want value, nil", again, err)
	}
}
