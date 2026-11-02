package rtsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"
)

// fakeLAN is a lanStreamer whose Start runs start, or succeeds when start is nil.
type fakeLAN struct {
	start func(ctx context.Context) error

	mu      sync.Mutex
	onError func(error)
	stopped bool
}

func (f *fakeLAN) Start(ctx context.Context) error {
	if f.start == nil {
		return nil
	}
	return f.start(ctx)
}

func (f *fakeLAN) Stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
}

func (f *fakeLAN) SetOnError(fn func(error)) {
	f.mu.Lock()
	f.onError = fn
	f.mu.Unlock()
}

func (f *fakeLAN) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// die reports a dead stream the way LANSource does after Start succeeded.
func (f *fakeLAN) die(err error) {
	f.mu.Lock()
	fn := f.onError
	f.mu.Unlock()
	fn(err)
}

// attempts replaces a stream's LAN and cloud paths with fakes and counts them.
type attempts struct {
	lan          *fakeLAN
	lanBuilt     atomic.Int32
	cloudStarted atomic.Int32
	lanFwd       *RTPForwarder // the forwarder the LAN source was built on
	cloudFwd     *RTPForwarder // the forwarder the cloud attempt ran on
}

func fakeAttempts(t *testing.T, cs *CameraStream, lanStart func(context.Context) error) *attempts {
	a := &attempts{lan: &fakeLAN{start: lanStart}}
	cs.lanFactory = func(fwd *RTPForwarder) lanStreamer {
		a.lanBuilt.Add(1)
		a.lanFwd = fwd
		return a.lan
	}
	cs.cloudStart = func() error {
		if cs.webrtcBridge.OnError == nil {
			t.Error("cloud attempt on a bridge with no error handler")
		}
		a.cloudStarted.Add(1)
		a.cloudFwd = cs.webrtcBridge.rtpForwarder
		return nil
	}
	return a
}

// newLANServer is a server whose camera "testdev" may stream over the LAN.
func newLANServer() *RTSPServer {
	s := NewRTSPServer(0, nil)
	s.LanStreaming = true
	s.LANTargets = map[string]LANTarget{"testdev": {LocalKey: "lk", Password: "pw", IP: "192.0.2.10"}}
	return s
}

// newLANStream registers a stream with one client and returns the client's
// end of that client's connection.
func newLANStream(t *testing.T, server *RTSPServer, resolution string) (*CameraStream, net.Conn) {
	t.Helper()
	camera := &storage.CameraInfo{DeviceID: "testdev", DeviceName: "Test Cam"}
	cs := NewCameraStream(camera, resolution, &storage.UserSession{UserKey: "user1"}, nil, server)
	cs.attachBridgeErrorHandler()
	server.streams[cs.streamId] = cs
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})
	cs.clients["s1"] = &RTSPClient{conn: serverConn, session: "s1", stream: cs}
	return cs, clientConn
}

// begin runs startStream as AddClient does, in the caller's goroutine.
func begin(cs *CameraStream) {
	cs.mutex.Lock()
	cs.connecting = true
	cs.mutex.Unlock()
	cs.startStream()
}

func streamState(cs *CameraStream) (active, connecting bool, lan lanStreamer) {
	cs.mutex.RLock()
	defer cs.mutex.RUnlock()
	return cs.active, cs.connecting, cs.lan
}

func TestLANSuccessSkipsCloud(t *testing.T) {
	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	a := fakeAttempts(t, cs, nil)

	begin(cs)

	active, connecting, lan := streamState(cs)
	if !active || connecting {
		t.Fatalf("active=%v connecting=%v, want an active stream", active, connecting)
	}
	if lan != a.lan {
		t.Fatal("cs.lan is not the LAN source that started")
	}
	if n := a.cloudStarted.Load(); n != 0 {
		t.Fatalf("cloud started %d times, want none", n)
	}
	if cs.bridgeStarted {
		t.Fatal("the LAN path must not mark the cloud bridge as started")
	}
	cs.stopStream()
	if !a.lan.isStopped() {
		t.Fatal("stopping the stream must stop its LAN source")
	}
}

func TestLANFailureFallsBackToCloud(t *testing.T) {
	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	cs.bridgeStarted = true // a reused stream: the LAN attempt needs a fresh bridge
	original := cs.webrtcBridge.rtpForwarder
	a := fakeAttempts(t, cs, func(context.Context) error {
		return fmt.Errorf("%w: %w", errNoFirstFrame, context.DeadlineExceeded)
	})

	begin(cs)

	active, _, lan := streamState(cs)
	if n := a.cloudStarted.Load(); n != 1 {
		t.Fatalf("cloud started %d times, want once", n)
	}
	if !active || lan != nil {
		t.Fatalf("active=%v lan=%v, want active on the cloud", active, lan)
	}
	if !server.lanCoolingDown("testdev") {
		t.Fatal("a failed LAN attempt must start the cool-down")
	}
	if !a.lan.isStopped() {
		t.Fatal("a failed LAN source must be stopped")
	}
	if a.lanFwd == original {
		t.Fatal("the LAN attempt ran on the forwarder of a bridge that had already run")
	}
	if a.cloudFwd != a.lanFwd {
		t.Fatal("the cloud fallback must reuse the forwarder clients set up on during the LAN attempt")
	}
}

func TestCooldownSkipsLAN(t *testing.T) {
	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	a := fakeAttempts(t, cs, nil)
	server.lanCooldowns["testdev"] = time.Now().Add(time.Minute)

	if ok, reason := cs.lanEligible(); ok || reason != "cooling down" {
		t.Fatalf("lanEligible() = %v, %q; want false, \"cooling down\"", ok, reason)
	}
	begin(cs)
	if a.lanBuilt.Load() != 0 || a.cloudStarted.Load() != 1 {
		t.Fatalf("lan built %d, cloud started %d; want 0 and 1", a.lanBuilt.Load(), a.cloudStarted.Load())
	}

	// An expired cool-down lets the LAN back in.
	server.lanCooldowns["testdev"] = time.Now().Add(-time.Second)
	if ok, reason := cs.lanEligible(); !ok {
		t.Fatalf("lanEligible() after the cool-down = false, %q", reason)
	}
}

func TestIneligibleStreamsSkipLAN(t *testing.T) {
	cases := []struct {
		reason     string
		resolution string
		configure  func(*RTSPServer)
	}{
		{"disabled", "hd", func(s *RTSPServer) { s.LanStreaming = false }},
		{"talkback on", "hd", func(s *RTSPServer) { s.Talkback = true }},
		{"sd stream", "sd", func(*RTSPServer) {}},
		{"no LAN credentials", "hd", func(s *RTSPServer) {
			s.LANTargets["testdev"] = LANTarget{LocalKey: "lk", IP: "192.0.2.10"}
		}},
		{"no address", "hd", func(s *RTSPServer) {
			s.LANTargets["testdev"] = LANTarget{LocalKey: "lk", Password: "pw"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			server := newLANServer()
			tc.configure(server)
			cs, _ := newLANStream(t, server, tc.resolution)
			a := fakeAttempts(t, cs, nil)

			if ok, reason := cs.lanEligible(); ok || reason != tc.reason {
				t.Fatalf("lanEligible() = %v, %q; want false, %q", ok, reason, tc.reason)
			}
			begin(cs)
			if a.lanBuilt.Load() != 0 || a.cloudStarted.Load() != 1 {
				t.Fatalf("lan built %d, cloud started %d; want 0 and 1", a.lanBuilt.Load(), a.cloudStarted.Load())
			}
		})
	}
}

func TestLANDeathRestartsOnCloud(t *testing.T) {
	server := newLANServer()
	cs, clientConn := newLANStream(t, server, "hd")
	a := fakeAttempts(t, cs, nil)
	begin(cs)

	a.lan.die(errors.New("lan: Test Cam stopped streaming: EOF"))

	_ = clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := clientConn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("client connection not closed after the LAN died: %v", err)
	}
	if active, connecting, lan := streamState(cs); active || connecting || lan != nil {
		t.Fatalf("active=%v connecting=%v lan=%v after the LAN died", active, connecting, lan)
	}
	if !a.lan.isStopped() {
		t.Fatal("the dead LAN source must be stopped")
	}
	if !server.lanCoolingDown("testdev") {
		t.Fatal("a LAN death must start the cool-down")
	}

	// The reconnecting client gets a new stream, and it takes the cloud.
	next, err := server.getOrCreateStream(cs.camera, "hd", cs.user)
	if err != nil || next == cs {
		t.Fatalf("getOrCreateStream = %p, %v; want a new stream", next, err)
	}
	b := fakeAttempts(t, next, nil)
	begin(next)
	if b.lanBuilt.Load() != 0 || b.cloudStarted.Load() != 1 {
		t.Fatalf("new stream: lan built %d, cloud started %d; want 0 and 1", b.lanBuilt.Load(), b.cloudStarted.Load())
	}

	// A client that held the old stream restarts it on the cloud; a late
	// report from the dead source must not tear that down.
	begin(cs)
	if n := a.cloudStarted.Load(); n != 1 {
		t.Fatalf("old stream: cloud started %d times, want once", n)
	}
	a.lan.die(errors.New("late report"))
	if active, _, _ := streamState(cs); !active {
		t.Fatal("a stale LAN source's error stopped the stream that replaced it")
	}
	cs.stopStream()
	next.stopStream()
}

func TestLANDeathBeforeCommitTearsDown(t *testing.T) {
	// The monitor sends one keyframe, then drops the session before tryLAN
	// re-takes the lock: the death must not be lost behind the commit.
	server := newLANServer()
	cs, clientConn := newLANStream(t, server, "hd")
	var a *attempts
	a = fakeAttempts(t, cs, func(context.Context) error {
		a.lan.die(errors.New("lan: Test Cam stopped streaming: EOF"))
		return nil
	})

	begin(cs)

	if active, connecting, lan := streamState(cs); active || connecting || lan != nil {
		t.Fatalf("active=%v connecting=%v lan=%v, want a torn-down stream", active, connecting, lan)
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := clientConn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("client connection not closed after the LAN died: %v", err)
	}
	if !server.lanCoolingDown("testdev") || !a.lan.isStopped() {
		t.Fatal("a LAN death before commit must start the cool-down and stop the source")
	}
}

func TestStopCancelsLANAttempt(t *testing.T) {
	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	started := make(chan struct{})
	a := fakeAttempts(t, cs, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return fmt.Errorf("%w: %w", errNoFirstFrame, ctx.Err())
	})

	done := make(chan struct{})
	go func() {
		begin(cs)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the LAN attempt never started")
	}

	cs.Stop()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("startStream still running 100 ms after Stop")
	}

	if active, connecting, lan := streamState(cs); active || connecting || lan != nil {
		t.Fatalf("active=%v connecting=%v lan=%v after Stop", active, connecting, lan)
	}
	if n := a.cloudStarted.Load(); n != 0 {
		t.Fatalf("cloud started %d times after Stop, want none", n)
	}
	if server.lanCoolingDown("testdev") {
		t.Fatal("a user Stop must not start the cool-down")
	}
	if !a.lan.isStopped() {
		t.Fatal("Stop left the LAN source running")
	}
}

func TestClientLeavingDuringLANAttemptSchedulesShutdown(t *testing.T) {
	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	fakeAttempts(t, cs, func(context.Context) error {
		left := make(chan struct{})
		go func() {
			cs.RemoveClient("s1")
			close(left)
		}()
		select {
		case <-left:
			return nil
		case <-time.After(time.Second):
			return errors.New("cs.mutex held during the LAN attempt")
		}
	})
	t.Cleanup(cs.stopStream)

	begin(cs)

	cs.mutex.RLock()
	defer cs.mutex.RUnlock()
	if !cs.active || cs.lan == nil {
		t.Fatal("expected the stream on the LAN")
	}
	if cs.shutdownTimer == nil {
		t.Fatal("a stream left without clients during the attempt must schedule its shutdown")
	}
}

func TestStreamCreatedWithoutCloud(t *testing.T) {
	camera := &storage.CameraInfo{DeviceID: "d", DeviceName: "n"}

	offline := NewRTSPServer(0, nil) // no MobileClient, no mqttManager
	if _, err := offline.getOrCreateStream(camera, "hd", nil); err != nil {
		t.Fatalf("getOrCreateStream without a cloud: %v", err)
	}

	down := NewRTSPServer(0, nil)
	down.mqttManager = NewMQTTManager(nil) // every GetClient fails
	if _, err := down.getOrCreateStream(camera, "hd", nil); err != nil {
		t.Fatalf("getOrCreateStream with the cloud down: %v", err)
	}
}

// The WAN is down and a LAN blip started the cool-down: when the cloud fails
// as well, the clients reconnect at once and only a cool-down that alone
// kept the stream off the LAN ends, so their next start tries the LAN.
func TestCloudFailureEndsTheCooldown(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*RTSPServer)
		cleared   bool
	}{
		{"LAN eligible but cooling down", func(*RTSPServer) {}, true},
		{"talkback on", func(s *RTSPServer) { s.Talkback = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel() // each waits out the 3 s between cloud attempts
			server := newLANServer()
			tc.configure(server)
			server.lanCooldowns["testdev"] = time.Now().Add(lanCooldownPeriod)
			cs, clientConn := newLANStream(t, server, "hd")
			a := fakeAttempts(t, cs, nil)
			cs.cloudStart = func() error {
				a.cloudStarted.Add(1)
				return errors.New("cloud down")
			}

			begin(cs)

			if a.lanBuilt.Load() != 0 || a.cloudStarted.Load() != 2 {
				t.Fatalf("lan built %d, cloud started %d; want 0 and 2", a.lanBuilt.Load(), a.cloudStarted.Load())
			}
			expectClosed(t, clientConn)
			if active, connecting, _ := streamState(cs); active || connecting {
				t.Fatalf("active=%v connecting=%v after both paths failed", active, connecting)
			}
			if cooling := server.lanCoolingDown("testdev"); cooling == tc.cleared {
				t.Fatalf("cooling down = %v, want %v", cooling, !tc.cleared)
			}
		})
	}
}
