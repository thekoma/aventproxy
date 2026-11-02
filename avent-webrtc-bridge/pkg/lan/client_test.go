package lan

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// fakeMedia serves its messages, then ends the stream with io.EOF.
type fakeMedia struct {
	msgs     [][]byte
	atEOF    func()
	deadline time.Time
}

func (f *fakeMedia) Read(p []byte) (int, error) {
	if len(f.msgs) == 0 {
		if f.atEOF != nil {
			f.atEOF()
		}
		return 0, io.EOF
	}
	n := copy(p, f.msgs[0])
	f.msgs = f.msgs[1:]
	return n, nil
}

func (f *fakeMedia) SetReadDeadline(t time.Time) error {
	f.deadline = t
	return nil
}

func sealedVideo(t *testing.T) []byte {
	t.Helper()
	payload := []byte{0x65, 0x88, 0x84, 0x00}
	sealed, err := seal(testKey, message(longSubHeader(len(payload), 1), payload))
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// recorder counts what a client hands to its callbacks.
type recorder struct {
	videos, closes int
	err            error
}

func (r *recorder) client() *Client {
	return NewClient(ClientConfig{
		OnVideo:  func(*VideoFrame) { r.videos++ },
		OnClosed: func(err error) { r.closes++; r.err = err },
	})
}

func TestConsumeReportsDeadStream(t *testing.T) {
	var rec recorder
	c := rec.client()
	media := &fakeMedia{msgs: [][]byte{sealedVideo(t)}}

	if err := c.consume(context.Background(), media, testKey); err == nil {
		t.Fatal("consume returned nil for a stream that ended")
	}
	if rec.videos != 1 || rec.closes != 1 || rec.err == nil {
		t.Fatalf("videos=%d closes=%d err=%v; want one frame and one close with an error", rec.videos, rec.closes, rec.err)
	}
	if d := time.Until(media.deadline); d <= 0 || d > mediaIdleTimeout {
		t.Fatalf("read deadline %v ahead, want within %v", d, mediaIdleTimeout)
	}

	// A second loop dying, or the keep-alive, is the same death.
	_ = c.consume(context.Background(), &fakeMedia{}, testKey)
	if rec.closes != 1 {
		t.Fatalf("OnClosed called %d times, want once", rec.closes)
	}
}

func TestConsumeSilentAfterCancel(t *testing.T) {
	var rec recorder
	c := rec.client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = c.consume(ctx, &fakeMedia{msgs: [][]byte{sealedVideo(t)}, atEOF: cancel}, testKey)
	if rec.videos != 1 || rec.closes != 0 {
		t.Fatalf("videos=%d closes=%d; want the frame and no close after cancel", rec.videos, rec.closes)
	}
}

// The read pending at Close fails because of it, as a closed KCP session's
// does; that is the owner's shutdown, not a dead stream.
func TestConsumeSilentAfterClose(t *testing.T) {
	var rec recorder
	c := rec.client()
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.consume(context.Background(), &fakeMedia{atEOF: func() { <-release }}, testKey)
	}()

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if rec.closes != 0 {
		t.Fatalf("OnClosed called %d times after Close, want none", rec.closes)
	}
}

// silentMonitor points Dial at a listener that accepts and never answers.
func silentMonitor(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				conn.Close()
			}()
		}
	}()
	old := dialPort
	dialPort = ln.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { dialPort = old })
}

// The integration discovers the monitor; the bridge never does, so a missing
// address must fail at once rather than dial anything.
func TestStartRefusesWithoutAddress(t *testing.T) {
	silentMonitor(t)
	for _, ip := range []string{"", "monitor.local"} {
		c := NewClient(ClientConfig{DeviceID: "dev", LocalKey: string(testKey), IP: ip})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		start := time.Now()
		err := c.Start(ctx)
		cancel()
		if err == nil {
			t.Fatalf("Start(%q) succeeded", ip)
		}
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Fatalf("Start(%q) took %v; it must not dial", ip, d)
		}
	}
}

func TestCloseAbortsStart(t *testing.T) {
	silentMonitor(t)
	c := NewClient(ClientConfig{DeviceID: "dev", LocalKey: string(testKey), IP: "127.0.0.1"})
	done := make(chan error, 1)
	go func() { done <- c.Start(context.Background()) }()

	time.Sleep(100 * time.Millisecond) // let Start reach the negotiation
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start succeeded against a silent monitor")
		}
	case <-time.After(time.Second):
		t.Fatal("Start still blocked a second after Close")
	}
}
