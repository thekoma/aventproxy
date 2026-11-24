package utils

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitTimeoutExpires(t *testing.T) {
	var w Waiter
	start := time.Now()
	err := w.WaitTimeout(50 * time.Millisecond)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("WaitTimeout = %v, want ErrWaitTimeout", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("WaitTimeout took %v", time.Since(start))
	}

	// The timeout finished the waiter, so nothing stays parked on it.
	done := make(chan error, 1)
	go func() { done <- w.Wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrWaitTimeout) {
			t.Fatalf("Wait after the timeout = %v, want ErrWaitTimeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait after the timeout is still parked")
	}
}

func TestWaitTimeoutReturnsDoneError(t *testing.T) {
	var w Waiter
	want := errors.New("boom")
	go func() {
		time.Sleep(10 * time.Millisecond)
		w.Done(want)
	}()
	if err := w.WaitTimeout(time.Second); err != want {
		t.Fatalf("WaitTimeout = %v, want %v", err, want)
	}
}

// A caller that gives up (a stream that stopped) is not held for the rest of d.
func TestWaitContextEndsWithTheContext(t *testing.T) {
	var w Waiter
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if err := w.WaitContext(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitContext = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("WaitContext took %v after its context ended", time.Since(start))
	}
	if err := w.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after the cancel = %v, want the waiter finished with context.Canceled", err)
	}
}
