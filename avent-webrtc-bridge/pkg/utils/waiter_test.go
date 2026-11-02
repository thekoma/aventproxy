package utils

import (
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
