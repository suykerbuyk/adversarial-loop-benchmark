package blockcache

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCloseJoinsLoaderBeforeWakingWaiters(t *testing.T) {
	entered := make(chan struct{})
	finish := make(chan struct{})
	returned := make(chan struct{})
	c := New(1, func(ctx context.Context, _ string) ([]byte, error) {
		close(entered)
		<-ctx.Done()
		<-finish
		close(returned)
		return []byte("late"), nil
	})
	defer c.Close()

	got := make(chan error, 1)
	go func() {
		_, err := c.Get(context.Background(), "k")
		got <- err
	}()
	<-entered
	closed := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closed)
	}()

	select {
	case err := <-got:
		t.Fatalf("waiter returned before Loader: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	select {
	case err := <-got:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Get error = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not wake")
	}
	select {
	case <-returned:
	default:
		t.Fatal("waiter woke before Loader returned")
	}
	<-closed
	if c.Len() != 0 {
		t.Fatalf("Len after Close = %d, want 0", c.Len())
	}
}

func TestSuccessfulEmptyBlockIsNonNilAndCopied(t *testing.T) {
	c := New(1, func(context.Context, string) ([]byte, error) { return []byte{}, nil })
	defer c.Close()
	first, err := c.Get(context.Background(), "empty")
	if err != nil || first == nil {
		t.Fatalf("first Get = (%v, %v), want non-nil empty block", first, err)
	}
	second, err := c.Get(context.Background(), "empty")
	if err != nil || second == nil {
		t.Fatalf("second Get = (%v, %v), want non-nil empty block", second, err)
	}
}
