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
