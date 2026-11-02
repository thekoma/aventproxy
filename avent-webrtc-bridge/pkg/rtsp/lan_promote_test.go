package rtsp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/core"

	"github.com/rs/zerolog"
)

// testLog holds what the package logs during its tests.
var testLog syncBuffer

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns the logged lines that contain s.
func (b *syncBuffer) lines(s string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, line := range strings.Split(b.buf.String(), "\n") {
		if strings.Contains(line, s) {
			out = append(out, line)
		}
	}
	return out
}

func TestMain(m *testing.M) {
	// Set before any test starts, so no goroutine reads core.Logger as it changes.
	core.Logger = zerolog.New(&testLog).Level(zerolog.InfoLevel)
	os.Exit(m.Run())
}

// cloudStreamCoolingDown starts a stream on the cloud because its camera's
// LAN cool-down has wait left. probe is the Start of every LAN source built
// for it. The camera gets a name no other test run logs.
func cloudStreamCoolingDown(t *testing.T, wait time.Duration, probe func(context.Context) error) (*CameraStream, net.Conn, *attempts) {
	t.Helper()
	server := newLANServer()
	server.lanCooldowns["testdev"] = time.Now().Add(wait)
	cs, clientConn := newLANStream(t, server, "hd")
	cs.camera.DeviceName = fmt.Sprintf("Promo Cam %d", time.Now().UnixNano())
	a := fakeAttempts(t, cs, probe)
	t.Cleanup(cs.stopStream)

	begin(cs)
	if active, _, lan := streamState(cs); !active || lan != nil || a.cloudStarted.Load() != 1 {
		t.Fatalf("active=%v lan=%v cloud started %d; want the stream on the cloud", active, lan, a.cloudStarted.Load())
	}
	return cs, clientConn, a
}

// signal reports a call without blocking on a second one.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func receive(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(what)
	}
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

func promoteTimer(cs *CameraStream) *time.Timer {
	cs.mutex.RLock()
	defer cs.mutex.RUnlock()
	return cs.promoteTimer
}

// expectClosed checks that the stream closed the client's connection.
func expectClosed(t *testing.T, clientConn net.Conn) {
	t.Helper()
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := clientConn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("client connection not closed: %v", err)
	}
}

// expectOpen checks that the stream left the client's connection alone.
func expectOpen(t *testing.T, clientConn net.Conn) {
	t.Helper()
	_ = clientConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := clientConn.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("client connection touched: %v", err)
	}
}

func TestPromotionMovesCloudStreamToLAN(t *testing.T) {
	probed := make(chan struct{}, 1)
	cs, clientConn, a := cloudStreamCoolingDown(t, 100*time.Millisecond, func(context.Context) error {
		signal(probed)
		return nil
	})

	receive(t, probed, "no LAN probe once the cool-down ended")
	expectClosed(t, clientConn)
	if active, connecting, _ := streamState(cs); active || connecting {
		t.Fatalf("active=%v connecting=%v, want the cloud stream torn down", active, connecting)
	}
	if !a.lan.isStopped() {
		t.Fatal("the probe must be stopped")
	}
	if a.lanFwd == a.cloudFwd {
		t.Fatal("the probe fed the cloud stream's forwarder")
	}

	// The reconnecting client gets a new stream, and it takes the LAN.
	next, err := cs.server.getOrCreateStream(cs.camera, "hd", cs.user)
	if err != nil || next == cs {
		t.Fatalf("getOrCreateStream = %p, %v; want a new stream", next, err)
	}
	b := fakeAttempts(t, next, nil)
	t.Cleanup(next.stopStream)
	begin(next)
	if b.lanBuilt.Load() != 1 || b.cloudStarted.Load() != 0 {
		t.Fatalf("new stream: lan built %d, cloud started %d; want 1 and 0", b.lanBuilt.Load(), b.cloudStarted.Load())
	}
}

func TestPromotionFailureRestartsCooldown(t *testing.T) {
	probed := make(chan struct{}, 1)
	cs, clientConn, a := cloudStreamCoolingDown(t, 100*time.Millisecond, func(context.Context) error {
		signal(probed)
		return fmt.Errorf("%w: %w", errNoFirstFrame, context.DeadlineExceeded)
	})

	receive(t, probed, "no LAN probe once the cool-down ended")
	waitUntil(t, func() bool { return promoteTimer(cs) != nil }, "no new promotion after the failed probe")

	if active, _, lan := streamState(cs); !active || lan != nil {
		t.Fatalf("active=%v lan=%v, want the stream still on the cloud", active, lan)
	}
	expectOpen(t, clientConn)
	cs.server.cooldownMutex.Lock()
	left := time.Until(cs.server.lanCooldowns["testdev"])
	cs.server.cooldownMutex.Unlock()
	if left < lanCooldownPeriod-time.Minute {
		t.Fatalf("cool-down has %v left, want a fresh %v", left, lanCooldownPeriod)
	}
	if !a.lan.isStopped() {
		t.Fatal("the failed probe must be stopped")
	}
}

func TestPromotionCancelledWhenStreamStops(t *testing.T) {
	t.Run("before the timer fires", func(t *testing.T) {
		cs, _, a := cloudStreamCoolingDown(t, 50*time.Millisecond, nil)
		if promoteTimer(cs) == nil {
			t.Fatal("a cloud stream kept off the LAN only by the cool-down must schedule a promotion")
		}

		cs.stopStream()
		if promoteTimer(cs) != nil {
			t.Fatal("stopping the stream must cancel its promotion")
		}
		time.Sleep(150 * time.Millisecond)
		if n := a.lanBuilt.Load(); n != 0 {
			t.Fatalf("LAN source built %d times after the stream stopped, want none", n)
		}
	})

	t.Run("while the probe runs", func(t *testing.T) {
		started := make(chan struct{}, 1)
		cancelled := make(chan struct{}, 1)
		release := make(chan struct{})
		cs, clientConn, a := cloudStreamCoolingDown(t, 50*time.Millisecond, func(ctx context.Context) error {
			signal(started)
			<-ctx.Done()
			signal(cancelled)
			<-release
			return nil // a first frame that came just as the stream stopped
		})
		receive(t, started, "no LAN probe once the cool-down ended")

		cs.stopStream()
		select {
		case <-cancelled:
		case <-time.After(100 * time.Millisecond):
			t.Fatal("the probe still running 100 ms after the stream stopped")
		}

		// The stream restarts on the cloud before the stale probe reports back.
		cs.server.cooldownMutex.Lock()
		cs.server.lanCooldowns["testdev"] = time.Now().Add(time.Minute)
		cs.server.cooldownMutex.Unlock()
		begin(cs)
		restarted := promoteTimer(cs)
		close(release)
		waitUntil(t, a.lan.isStopped, "the stale probe was never stopped")

		if active, _, lan := streamState(cs); !active || lan != nil {
			t.Fatalf("active=%v lan=%v, the stale probe touched the restarted stream", active, lan)
		}
		if promoteTimer(cs) != restarted || restarted == nil {
			t.Fatal("the stale probe changed the restarted stream's promotion")
		}
		expectOpen(t, clientConn)
		if n := a.lanBuilt.Load(); n != 1 {
			t.Fatalf("LAN source built %d times, want only the probe", n)
		}
	})
}

func TestPromotionIsNotLoggedAsAnError(t *testing.T) {
	cs, clientConn, _ := cloudStreamCoolingDown(t, 50*time.Millisecond, nil)
	expectClosed(t, clientConn)

	name := cs.camera.DeviceName
	lines := testLog.lines(name)
	moved := false
	for _, line := range lines {
		if strings.Contains(line, `"level":"error"`) || strings.Contains(line, "WebRTC error") {
			t.Fatalf("the move back to the LAN was logged as an error: %s", line)
		}
		moved = moved || strings.Contains(line, `"level":"info"`) &&
			strings.Contains(line, "LAN available again for "+name+", moving viewers")
	}
	if !moved {
		t.Fatalf("no info line about the LAN being back for %s in %q", name, lines)
	}
}
