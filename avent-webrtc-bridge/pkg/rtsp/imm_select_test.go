package rtsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/lan"
)

// immAttempts replaces a stream's imm source with a fake and counts it, on
// top of fakeAttempts' LAN and cloud fakes.
type immAttempts struct {
	*attempts
	imm      *fakeLAN
	immBuilt atomic.Int32
	immFwd   *RTPForwarder
}

func fakeIMMAttempts(t *testing.T, cs *CameraStream, lanStart, immStart func(context.Context) error) *immAttempts {
	a := &immAttempts{attempts: fakeAttempts(t, cs, lanStart), imm: &fakeLAN{start: immStart}}
	cs.immFactory = func(fwd *RTPForwarder) lanStreamer {
		a.immBuilt.Add(1)
		a.immFwd = fwd
		return a.imm
	}
	return a
}

// newIMMServer is a server whose camera "testdev" is a 3.3 monitor: never on
// the LAN, and on imm when the option asks for it.
func newIMMServer(transport string) *RTSPServer {
	s := NewRTSPServer(0, nil)
	s.LanStreaming = true
	s.CloudTransport = transport
	s.LANTargets = map[string]LANTarget{"testdev": {LocalKey: "lk", Password: "pw", IP: "192.0.2.10", Protocol: "3.3"}}
	return s
}

// With the default, a dev3 user's streams never see imm.
func TestDefaultTransportNeverTriesIMM(t *testing.T) {
	for _, transport := range []string{"", CloudTransportWebRTC} {
		server := newIMMServer(transport)
		cs, _ := newLANStream(t, server, "hd")
		a := fakeIMMAttempts(t, cs, nil, nil)

		if ok, reason := cs.immEligible(); ok || reason != "" {
			t.Fatalf("immEligible() = %v, %q with transport %q; want false and no reason to log", ok, reason, transport)
		}
		begin(cs)
		if a.immBuilt.Load() != 0 || a.cloudStarted.Load() != 1 {
			t.Fatalf("imm built %d, cloud started %d; want 0 and 1", a.immBuilt.Load(), a.cloudStarted.Load())
		}
		if server.immCoolingDown("testdev") {
			t.Fatal("no imm attempt, so no imm cool-down")
		}
		cs.stopStream()
	}
}

func TestIMMSuccessSkipsWebRTC(t *testing.T) {
	server := newIMMServer(CloudTransportIMM)
	cs, _ := newLANStream(t, server, "hd")
	a := fakeIMMAttempts(t, cs, nil, nil)

	begin(cs)

	active, connecting, src := streamState(cs)
	if !active || connecting || src != a.imm {
		t.Fatalf("active=%v connecting=%v source=%v, want active on the imm source", active, connecting, src)
	}
	if a.lanBuilt.Load() != 0 || a.cloudStarted.Load() != 0 {
		t.Fatalf("lan built %d, cloud started %d; want neither", a.lanBuilt.Load(), a.cloudStarted.Load())
	}
	if a.immFwd != cs.forwarder {
		t.Fatal("the imm source must feed the stream's forwarder")
	}
	if cs.bridgeStarted {
		t.Fatal("the imm path must not mark the webrtc bridge as started")
	}
	cs.stopStream()
	if !a.imm.isStopped() {
		t.Fatal("stopping the stream must stop its imm source")
	}
}

func TestIMMFailureFallsBackToWebRTC(t *testing.T) {
	server := newIMMServer(CloudTransportIMM)
	cs, _ := newLANStream(t, server, "hd")
	cs.bridgeStarted = true
	original := cs.webrtcBridge
	a := fakeIMMAttempts(t, cs, nil, func(context.Context) error {
		return fmt.Errorf("%w: %v", lan.ErrNoAnswer, context.DeadlineExceeded)
	})

	begin(cs)

	active, _, src := streamState(cs)
	if a.cloudStarted.Load() != 1 || !active || src != nil {
		t.Fatalf("cloud started %d, active %v, source %v; want active on webrtc", a.cloudStarted.Load(), active, src)
	}
	if !server.immCoolingDown("testdev") {
		t.Fatal("a failed imm attempt must start the imm cool-down")
	}
	if server.lanCoolingDown("testdev") {
		t.Fatal("an imm failure is not a LAN failure")
	}
	if !a.imm.isStopped() {
		t.Fatal("a failed imm source must be stopped")
	}
	if a.cloudBridge == original {
		t.Fatal("the webrtc fallback ran on a bridge that had already run")
	}

	// Within the cool-down the next start goes straight to webrtc. (Made
	// before the first stream stops: the helper writes the server's map
	// without its lock, and a stop removes the stream on another goroutine.)
	cs2, _ := newLANStream(t, server, "hd")
	cs.stopStream()
	b := fakeIMMAttempts(t, cs2, nil, nil)
	if ok, reason := cs2.immEligible(); ok || reason != "cooling down" {
		t.Fatalf("immEligible() = %v, %q; want false, \"cooling down\"", ok, reason)
	}
	begin(cs2)
	if b.immBuilt.Load() != 0 || b.cloudStarted.Load() != 1 {
		t.Fatalf("imm built %d, cloud started %d; want 0 and 1", b.immBuilt.Load(), b.cloudStarted.Load())
	}
	cs2.stopStream()

	// And once it is over, imm is back.
	server.immCooldowns["testdev"] = time.Now().Add(-time.Second)
	if ok, reason := cs2.immEligible(); !ok {
		t.Fatalf("immEligible() after the cool-down = false, %q", reason)
	}
}

func TestIneligibleStreamsSkipIMM(t *testing.T) {
	cases := []struct {
		reason     string
		resolution string
		configure  func(*RTSPServer)
	}{
		{"talkback on", "hd", func(s *RTSPServer) { s.Talkback = true }},
		{"sd stream", "sd", func(*RTSPServer) {}},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			server := newIMMServer(CloudTransportIMM)
			tc.configure(server)
			cs, _ := newLANStream(t, server, tc.resolution)
			a := fakeIMMAttempts(t, cs, nil, nil)
			if ok, reason := cs.immEligible(); ok || reason != tc.reason {
				t.Fatalf("immEligible() = %v, %q; want false, %q", ok, reason, tc.reason)
			}
			begin(cs)
			if a.immBuilt.Load() != 0 || a.cloudStarted.Load() != 1 {
				t.Fatalf("imm built %d, cloud started %d; want 0 and 1", a.immBuilt.Load(), a.cloudStarted.Load())
			}
			if server.immCoolingDown("testdev") {
				t.Fatal("not an imm failure: no cool-down")
			}
			cs.stopStream()
		})
	}
}

// The LAN comes first when it may; imm comes before webrtc when the LAN fails.
func TestTransportOrder(t *testing.T) {
	server := newIMMServer(CloudTransportIMM)
	server.LANTargets["testdev"] = LANTarget{LocalKey: "lk", Password: "pw", IP: "192.0.2.10", Protocol: "3.5"}

	cs, _ := newLANStream(t, server, "hd")
	a := fakeIMMAttempts(t, cs, nil, nil)
	begin(cs)
	if a.lanBuilt.Load() != 1 || a.immBuilt.Load() != 0 || a.cloudStarted.Load() != 0 {
		t.Fatalf("lan %d, imm %d, cloud %d; want the LAN alone", a.lanBuilt.Load(), a.immBuilt.Load(), a.cloudStarted.Load())
	}

	cs2, _ := newLANStream(t, server, "hd") // before the stop: see TestIMMFailureFallsBackToWebRTC
	cs.stopStream()
	b := fakeIMMAttempts(t, cs2, func(context.Context) error {
		return fmt.Errorf("%w: %w", errNoFirstFrame, context.DeadlineExceeded)
	}, nil)
	begin(cs2)
	if b.lanBuilt.Load() != 1 || b.immBuilt.Load() != 1 || b.cloudStarted.Load() != 0 {
		t.Fatalf("lan %d, imm %d, cloud %d; want the LAN, then imm", b.lanBuilt.Load(), b.immBuilt.Load(), b.cloudStarted.Load())
	}
	if _, _, src := streamState(cs2); src != b.imm {
		t.Fatal("the stream is not on the imm source")
	}
	cs2.stopStream()
}

func TestIMMDeathRestartsOnWebRTC(t *testing.T) {
	server := newIMMServer(CloudTransportIMM)
	cs, clientConn := newLANStream(t, server, "hd")
	a := fakeIMMAttempts(t, cs, nil, nil)
	begin(cs)

	a.imm.die(fmt.Errorf("lan: Test Cam stopped streaming: %w", lan.ErrEnded))

	_ = clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := clientConn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("client connection not closed after the imm session died: %v", err)
	}
	if active, connecting, src := streamState(cs); active || connecting || src != nil {
		t.Fatalf("active=%v connecting=%v source=%v after the imm session died", active, connecting, src)
	}
	if !a.imm.isStopped() || !server.immCoolingDown("testdev") {
		t.Fatal("an imm death must stop the source and start the cool-down")
	}

	// The restart takes webrtc, and a late report from the dead source is ignored.
	begin(cs)
	if a.cloudStarted.Load() != 1 {
		t.Fatalf("cloud started %d times, want once", a.cloudStarted.Load())
	}
	a.imm.die(errors.New("late report"))
	if active, _, _ := streamState(cs); !active {
		t.Fatal("a stale imm source's error stopped the stream that replaced it")
	}
	cs.stopStream()
}

func TestStopCancelsIMMAttempt(t *testing.T) {
	server := newIMMServer(CloudTransportIMM)
	cs, _ := newLANStream(t, server, "hd")
	started := make(chan struct{})
	a := fakeIMMAttempts(t, cs, nil, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	done := make(chan struct{})
	go func() { begin(cs); close(done) }()
	<-started
	cs.stopStream()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not cancel the imm attempt")
	}
	if a.cloudStarted.Load() != 0 || server.immCoolingDown("testdev") {
		t.Fatal("a stopped attempt must neither fall back nor cool down")
	}
}

func TestParseCloudTransport(t *testing.T) {
	for in, want := range map[string]string{"": CloudTransportWebRTC, "webrtc": CloudTransportWebRTC, "imm": CloudTransportIMM} {
		if got, err := ParseCloudTransport(in); err != nil || got != want {
			t.Errorf("ParseCloudTransport(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseCloudTransport("lan"); err == nil {
		t.Error("an unknown transport must be refused")
	}
}

// The log names the failed step, whatever wrapped the error on the way.
func TestIMMStepNames(t *testing.T) {
	noFrame := func(stall error) error {
		return fmt.Errorf("%w: %w (%w)", errNoFirstFrame, context.DeadlineExceeded, stall)
	}
	cases := map[string]error{
		"cloud setup":                     fmt.Errorf("%w: rtc.config.get: boom", errIMMCloud),
		"offer":                           fmt.Errorf("%w: mqtt closed", lan.ErrOfferNotSent),
		"refused":                         lan.ErrRefused,
		"no answer":                       fmt.Errorf("%w: %v", lan.ErrNoAnswer, context.DeadlineExceeded),
		"answer unusable":                 fmt.Errorf("%w: no ICE credentials", lan.ErrBadAnswer),
		"answer without usable candidate": fmt.Errorf("%w: deadline", lan.ErrNoCandidate),
		"ICE timeout":                     fmt.Errorf("%w: lan: no answer to the ICE checks", lan.ErrICE),
		"login not sent":                  fmt.Errorf("%w: kcp", lan.ErrLogin),
		"login refused or one-way path":   noFrame(errIMMSilent),
		"no media after login":            noFrame(errIMMNoMedia),
		"no video":                        noFrame(errIMMNoVideo),
		"no SPS/IDR":                      noFrame(errIMMNoKeyframe),
		"start":                           errors.New("something else"),
	}
	for want, err := range cases {
		if got := immStep(err); got != want {
			t.Errorf("immStep(%v) = %q, want %q", err, got, want)
		}
	}
}
