package lan

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
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

// chanMedia delivers what is sent on msgs and honours read deadlines, as a
// KCP session does. Closing msgs ends it with io.EOF.
type chanMedia struct {
	msgs     chan []byte
	deadline time.Time
}

func newChanMedia() *chanMedia { return &chanMedia{msgs: make(chan []byte)} }

func (m *chanMedia) SetReadDeadline(t time.Time) error {
	m.deadline = t
	return nil
}

func (m *chanMedia) Read(p []byte) (int, error) {
	timer := time.NewTimer(time.Until(m.deadline))
	defer timer.Stop()
	select {
	case b, ok := <-m.msgs:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, b), nil
	case <-timer.C:
		return 0, os.ErrDeadlineExceeded
	}
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

// recorder counts what a client hands to its callbacks; closed holds one
// entry per OnClosed call.
type recorder struct {
	videos atomic.Int32
	closed chan error
}

func newRecorder() *recorder { return &recorder{closed: make(chan error, 8)} }

func (r *recorder) client() *Client {
	return NewClient(ClientConfig{
		OnVideo:  func(*VideoFrame) { r.videos.Add(1) },
		OnClosed: func(err error) { r.closed <- err },
	})
}

// waitClosed returns the error of the next OnClosed call, failing the test if
// none comes within d.
func (r *recorder) waitClosed(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-r.closed:
		return err
	case <-time.After(d):
		t.Fatalf("OnClosed not called within %v", d)
		return nil
	}
}

func TestConsumeReportsDeadStream(t *testing.T) {
	rec := newRecorder()
	c := rec.client()
	media := &fakeMedia{msgs: [][]byte{sealedVideo(t)}}

	if err := c.consume(context.Background(), media, testKey); err == nil {
		t.Fatal("consume returned nil for a stream that ended")
	}
	if n := rec.videos.Load(); n != 1 {
		t.Fatalf("OnVideo called %d times, want once", n)
	}
	if n := len(rec.closed); n != 1 {
		t.Fatalf("OnClosed called %d times, want once", n)
	}
	if err := <-rec.closed; err == nil {
		t.Fatal("OnClosed got a nil error")
	}
	if d := time.Until(media.deadline); d <= 0 || d > mediaIdleTimeout {
		t.Fatalf("read deadline %v ahead, want within %v", d, mediaIdleTimeout)
	}

	// A second loop dying, or the keep-alive, is the same death.
	_ = c.consume(context.Background(), &fakeMedia{}, testKey)
	if n := len(rec.closed); n != 0 {
		t.Fatalf("OnClosed called %d more times, want once in all", n)
	}
}

func TestConsumeSilentAfterCancel(t *testing.T) {
	rec := newRecorder()
	c := rec.client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = c.consume(ctx, &fakeMedia{msgs: [][]byte{sealedVideo(t)}, atEOF: cancel}, testKey)
	if v, n := rec.videos.Load(), len(rec.closed); v != 1 || n != 0 {
		t.Fatalf("videos=%d closes=%d; want the frame and no close after cancel", v, n)
	}
}

// The read pending at Close fails because of it, as a closed KCP session's
// does; that is the owner's shutdown, not a dead stream.
func TestConsumeSilentAfterClose(t *testing.T) {
	rec := newRecorder()
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
	if n := len(rec.closed); n != 0 {
		t.Fatalf("OnClosed called %d times after Close, want none", n)
	}
}

// consumeAll runs one consume per reader and closes the returned channel when
// all of them have returned.
func consumeAll(c *Client, readers ...mediaReader) <-chan struct{} {
	var wg sync.WaitGroup
	for _, r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.consume(context.Background(), r, testKey)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	return done
}

// A conversation that carries nothing is not a dead stream while another one
// delivers.
func TestQuietConversationBesideALiveOne(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	c := rec.client()
	live, quiet := newChanMedia(), newChanMedia()
	done := consumeAll(c, live, quiet)

	msg := sealedVideo(t)
	for i := range 10 { // every 500 ms for 5 s
		time.Sleep(500 * time.Millisecond)
		select {
		case live.msgs <- msg:
		case <-time.After(time.Second):
			t.Fatalf("live reader stopped before message %d", i)
		}
	}
	if n := len(rec.closed); n != 0 {
		t.Fatalf("OnClosed called %d times with one conversation live: %v", n, <-rec.closed)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	close(live.msgs)
	close(quiet.msgs)
	<-done
	if n := len(rec.closed); n != 0 {
		t.Fatalf("OnClosed called %d times after Close", n)
	}
}

func TestEveryConversationQuietCloses(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	c := rec.client()
	start := time.Now()
	done := consumeAll(c, newChanMedia(), newChanMedia())

	if err := rec.waitClosed(t, mediaIdleTimeout+time.Second); err == nil {
		t.Fatal("OnClosed got a nil error")
	}
	if d := time.Since(start); d < mediaIdleTimeout-50*time.Millisecond {
		t.Fatalf("closed after %v, want about %v", d, mediaIdleTimeout)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("readers still running after the stream was reported dead")
	}
	if n := len(rec.closed); n != 0 {
		t.Fatalf("OnClosed called %d more times, want once", n)
	}
}

// What the monitor pushes on the signalling session is read and dropped, so
// it cannot fill the socket buffer; the monitor closing it is a dead stream.
func TestControlSessionDrained(t *testing.T) {
	rec := newRecorder()
	c := rec.client()
	client, monitor := pipeSessions()
	if err := c.hold(client); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.keepAlive(ctx, client)

	for i := range 3 {
		// net.Pipe is unbuffered: Send returns only once the client read it.
		if err := monitor.Send(CmdHeartbeat, testIV, []byte(`{}`)); err != nil {
			t.Fatalf("push %d not consumed: %v", i, err)
		}
	}
	if n := len(rec.closed); n != 0 {
		t.Fatalf("OnClosed called %d times while the session was alive", n)
	}

	monitor.Close()
	if err := rec.waitClosed(t, time.Second); err == nil {
		t.Fatal("OnClosed got a nil error")
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(rec.closed); n != 0 {
		t.Fatalf("OnClosed called %d more times, want once", n)
	}
}

func TestControlSessionSilentAfterClose(t *testing.T) {
	rec := newRecorder()
	c := rec.client()
	client, monitor := pipeSessions()
	defer monitor.Close()
	if err := c.hold(client); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.keepAlive(ctx, client)

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the drain see its closed session
	if n := len(rec.closed); n != 0 {
		t.Fatalf("OnClosed called %d times after Close, want none", n)
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
