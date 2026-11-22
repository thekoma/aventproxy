package lan

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"
)

// The startup sequence pauses between two messages and never after the last.
func TestSendPacedPausesOnlyBetweenMessages(t *testing.T) {
	msgs := [][]byte{{1}, {2}, {3}}
	for _, tc := range []struct {
		gap  time.Duration
		want []string
	}{
		{20 * time.Millisecond, []string{"send 1", "wait 20ms", "send 2", "wait 20ms", "send 3"}},
		{0, []string{"send 1", "send 2", "send 3"}},
	} {
		var got []string
		send := func(m []byte) error { got = append(got, "send "+string(rune('0'+m[0]))); return nil }
		wait := func(_ context.Context, d time.Duration) error { got = append(got, "wait "+d.String()); return nil }
		if err := sendPaced(context.Background(), msgs, tc.gap, send, wait); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("gap %s: %v, want %v", tc.gap, got, tc.want)
		}
	}
}

func TestSendPacedStopsOnError(t *testing.T) {
	boom := errors.New("boom")
	sent := 0
	send := func([]byte) error { sent++; return nil }
	wait := func(context.Context, time.Duration) error { return boom }
	if err := sendPaced(context.Background(), [][]byte{{1}, {2}}, time.Millisecond, send, wait); !errors.Is(err, boom) || sent != 1 {
		t.Fatalf("err %v after %d sends, want boom after 1", err, sent)
	}
	failing := func([]byte) error { return boom }
	if err := sendPaced(context.Background(), [][]byte{{1}, {2}}, 0, failing, wait); !errors.Is(err, boom) {
		t.Fatalf("err %v, want boom", err)
	}
}

func TestStartupGap(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0:                     DefaultStartupGap,
		-1:                    0,
		20 * time.Millisecond: 20 * time.Millisecond,
	} {
		if got := NewClient(ClientConfig{StartupGap: in}).startupGap(); got != want {
			t.Errorf("StartupGap %s: %s, want %s", in, got, want)
		}
	}
}

type fakeReader struct {
	deadline time.Time
	err      error
}

func (f *fakeReader) SetReadDeadline(t time.Time) error { f.deadline = t; return nil }
func (f *fakeReader) Read([]byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return 1, nil
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

// The next startup message waits for the monitor's answer, at most the gap.
func TestAwaitReply(t *testing.T) {
	buf := make([]byte, 16)
	r := &fakeReader{}
	start := time.Now()
	if n, err := awaitReply(context.Background(), r, buf, 50*time.Millisecond); n == 0 || err != nil {
		t.Fatalf("answer: %d %v, want a message and nil", n, err)
	}
	if d := r.deadline.Sub(start); d < 50*time.Millisecond || d > time.Second {
		t.Errorf("deadline %s from now, want the gap", d)
	}
	r.err = timeoutErr{}
	if n, err := awaitReply(context.Background(), r, buf, time.Millisecond); n != 0 || err != nil {
		t.Fatalf("no answer: %d %v, want 0 nil", n, err)
	}
	r.err = errors.New("closed")
	if _, err := awaitReply(context.Background(), r, buf, time.Millisecond); err == nil {
		t.Fatal("a broken conversation must end the start")
	}
	// A start that ran out of time stops instead of sending on.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.err = timeoutErr{}
	if _, err := awaitReply(ctx, r, buf, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	// The start's own deadline caps the wait.
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	r.err = nil
	if _, err := awaitReply(ctx, r, buf, time.Hour); err != nil || time.Until(r.deadline) > time.Second {
		t.Fatalf("deadline not capped by ctx: %v", err)
	}
}

// scriptedControl is a control conversation whose monitor answers every setup
// message after delay (or never, when answer is false) and never the login,
// with read deadlines as a KCP session has them.
type scriptedControl struct {
	answer  bool
	delay   time.Duration
	replies chan []byte

	mu       sync.Mutex
	deadline time.Time
	sent     []time.Time
}

func newScriptedControl(answer bool, delay time.Duration) *scriptedControl {
	return &scriptedControl{answer: answer, delay: delay, replies: make(chan []byte, 16)}
}

func (s *scriptedControl) Write(p []byte) (int, error) {
	s.mu.Lock()
	first := len(s.sent) == 0
	s.sent = append(s.sent, time.Now())
	s.mu.Unlock()
	if s.answer && !first {
		time.AfterFunc(s.delay, func() { s.replies <- []byte("ack") })
	}
	return len(p), nil
}

func (s *scriptedControl) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadline = t
	s.mu.Unlock()
	return nil
}

func (s *scriptedControl) Read(p []byte) (int, error) {
	s.mu.Lock()
	d := s.deadline
	s.mu.Unlock()
	var timeout <-chan time.Time
	if !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case b := <-s.replies:
		return copy(p, b), nil
	case <-timeout:
		return 0, timeoutErr{}
	}
}

var _ io.Writer = (*scriptedControl)(nil)

// The whole login against a scripted monitor: with answers it goes on as each
// comes, without them it waits the gap between messages and never after the
// last, and every answer it read reaches OnControl. The conversation is left
// without a read deadline by stream, not by login; see TestStreamClearsTheLoginDeadline.
func TestLoginPacedByTheAnswers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answer   bool
		min, max time.Duration
		answers  int
	}{
		// 7 messages, 6 gaps: each wait ends with the 10 ms answer. The last
		// message's answer comes after the login; drainControl reads it.
		{"answered", true, 50 * time.Millisecond, 400 * time.Millisecond, 5},
		// 6 gaps of 120 ms, none after the last message.
		{"unanswered", false, 6 * DefaultStartupGap, 7 * DefaultStartupGap, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := newScriptedControl(tc.answer, 10*time.Millisecond)
			var got int
			start := time.Now()
			err := login(context.Background(), control, testKey, "token", DefaultStartupGap, func(int) { got++ }, 0)
			took := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
			if len(control.sent) != 7 {
				t.Fatalf("%d messages sent, want the login and six setup messages", len(control.sent))
			}
			if took < tc.min || took > tc.max {
				t.Errorf("login took %v, want %v to %v", took, tc.min, tc.max)
			}
			if got != tc.answers {
				t.Errorf("OnControl told of %d answers, want %d", got, tc.answers)
			}
		})
	}
}
