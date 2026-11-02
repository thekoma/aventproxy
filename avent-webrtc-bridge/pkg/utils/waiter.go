// (c) go2rtc

package utils

import (
	"errors"
	"sync"
	"time"
)

var ErrWaitTimeout = errors.New("timed out waiting")

// Waiter support:
// - autotart on first Wait
// - block new waiters after last Done
// - safe Done after finish
type Waiter struct {
	sync.WaitGroup
	mu    sync.Mutex
	state int // state < 0 means finish
	err   error
}

func (w *Waiter) Add(delta int) {
	w.mu.Lock()
	if w.state >= 0 {
		w.state += delta
		w.WaitGroup.Add(delta)
	}
	w.mu.Unlock()
}

func (w *Waiter) Wait() error {
	w.mu.Lock()
	// first wait auto start waiter
	if w.state == 0 {
		w.state++
		w.WaitGroup.Add(1)
	}
	w.mu.Unlock()

	w.WaitGroup.Wait()

	return w.err
}

func (w *Waiter) Done(err error) {
	w.mu.Lock()

	// safe run Done only when have tasks
	release := w.state > 0
	if release {
		w.state--
	}

	// block waiter for any operations after last done
	if w.state == 0 {
		w.state = -1
		w.err = err
	}

	w.mu.Unlock()

	// release after err is set, so a woken Wait never reads it unsynchronised
	if release {
		w.WaitGroup.Done()
	}
}

func (w *Waiter) WaitChan() <-chan error {
	var ch chan error

	w.mu.Lock()

	if w.state >= 0 {
		ch = make(chan error)
		go func() {
			ch <- w.Wait()
		}()
	}

	w.mu.Unlock()

	return ch
}

// WaitTimeout is Wait bounded by d. On timeout the Wait goroutine stays parked until Done.
func (w *Waiter) WaitTimeout(d time.Duration) error {
	ch := make(chan error, 1)
	go func() { ch <- w.Wait() }()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case err := <-ch:
		return err
	case <-timer.C:
		return ErrWaitTimeout
	}
}
