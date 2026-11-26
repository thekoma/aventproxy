package rtsp

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"
)

func TestSplitLocal(t *testing.T) {
	for _, tc := range []struct {
		url, path, res string
		local          bool
	}{
		{"rtsp://h:1/Nursery/local", "/Nursery", "hd", true},
		{"rtsp://h:1/Nursery/local/hd", "/Nursery", "hd", true},
		{"rtsp://h:1/Nursery/local/sd", "/Nursery", "sd", true},
		{"rtsp://h:1/Nursery", "/Nursery", "hd", false},
		{"rtsp://h:1/Nursery/hd", "/Nursery", "hd", false},
		{"rtsp://h:1/local", "/local", "hd", false}, // a camera named "local"
		{"rtsp://h:1/local/local", "/local", "hd", true},
		{"rtsp://h:1//local", "//local", "hd", false}, // no camera before it
	} {
		path, res := extractCameraPath(tc.url)
		path, local := splitLocal(path)
		if path != tc.path || res != tc.res || local != tc.local {
			t.Errorf("%s: got %q %q %v, want %q %q %v", tc.url, path, res, local, tc.path, tc.res, tc.local)
		}
	}
}

// newLocalStream is newLANStream for the camera's local path.
func newLocalStream(t *testing.T, server *RTSPServer) (*CameraStream, func() bool) {
	t.Helper()
	cs, clientConn := newPathStream(t, server, "hd", true)
	closed := func() bool {
		_ = clientConn.SetReadDeadline(time.Now().Add(time.Second))
		_, err := clientConn.Read(make([]byte, 1))
		return errors.Is(err, io.EOF)
	}
	return cs, closed
}

// The README and the PR text promise that an automatic stream kept off a held
// LAN session waits 5 minutes before it looks again.
func TestLANBusyRetryIsFiveMinutes(t *testing.T) {
	if lanBusyRetry != 5*time.Minute {
		t.Fatalf("lanBusyRetry = %s, want 5m", lanBusyRetry)
	}
}

func TestLocalStreamStreamsOverTheLAN(t *testing.T) {
	server := newLANServer()
	cs, _ := newLocalStream(t, server)
	a := fakeAttempts(t, cs, nil)

	begin(cs)

	if active, _, lan := streamState(cs); !active || lan == nil {
		t.Fatalf("active=%v lan=%v, want an active LAN stream", active, lan)
	}
	if n := a.cloudStarted.Load(); n != 0 {
		t.Fatalf("cloud started %d times, want none", n)
	}
	if server.lanHolders.heldBy("testdev") != cs {
		t.Fatal("the local stream does not hold the camera's LAN session")
	}
}

func TestLocalStreamFailureHasNoCloudAndNoCooldown(t *testing.T) {
	server := newLANServer()
	cs, closed := newLocalStream(t, server)
	a := fakeAttempts(t, cs, func(context.Context) error { return errors.New("no answer") })

	begin(cs)

	if n := a.cloudStarted.Load(); n != 0 {
		t.Fatalf("cloud started %d times, want none", n)
	}
	if !closed() {
		t.Fatal("the client was not closed after the local stream failed")
	}
	if server.lanCoolingDown("testdev") {
		t.Fatal("a failed local stream started the camera's cool-down")
	}
	if server.lanHolders.heldBy("testdev") != nil {
		t.Fatal("the failed local stream kept the LAN session")
	}
}

func TestLocalStreamIgnoresTheCooldown(t *testing.T) {
	server := newLANServer()
	server.startLANCooldown("testdev")
	cs, _ := newLocalStream(t, server)
	fakeAttempts(t, cs, nil)

	begin(cs)

	if active, _, _ := streamState(cs); !active {
		t.Fatal("the local stream did not start during the cool-down")
	}
}

func TestLocalStreamThatDiesStartsNoCooldown(t *testing.T) {
	server := newLANServer()
	cs, closed := newLocalStream(t, server)
	a := fakeAttempts(t, cs, nil)
	begin(cs)

	a.lan.die(errors.New("gone"))

	if !closed() {
		t.Fatal("the client was not closed after the local stream died")
	}
	if server.lanCoolingDown("testdev") {
		t.Fatal("a dead local stream started the camera's cool-down")
	}
	if a.cloudStarted.Load() != 0 {
		t.Fatal("a dead local stream went to the cloud")
	}
}

func TestLocalStreamWithSessionTalkback(t *testing.T) {
	server := newLANServer()
	server.Talkback = true // session mode: the automatic path stays off the LAN
	cs, _ := newLocalStream(t, server)
	fakeAttempts(t, cs, nil)
	if cs.talk != nil {
		t.Fatal("session talkback made a talker")
	}

	begin(cs)

	if active, _, lan := streamState(cs); !active || lan == nil {
		t.Fatal("the local stream did not stream over the LAN with session talkback on")
	}
}

func TestOneLANSessionPerCamera(t *testing.T) {
	server := newLANServer()
	auto, _ := newLANStream(t, server, "hd")
	autoA := fakeAttempts(t, auto, nil)
	begin(auto) // holds the LAN, with its client watching

	local, closed := newLocalStream(t, server)
	localA := fakeAttempts(t, local, nil)
	begin(local)

	if n := localA.lanBuilt.Load(); n != 0 {
		t.Fatalf("the local stream built %d LAN sources while the automatic one held the session", n)
	}
	if !closed() {
		t.Fatal("the local stream's client was not closed")
	}
	if active, _, _ := streamState(auto); !active {
		t.Fatal("the watched automatic stream was stopped")
	}

	// Once the automatic stream lets go, the local path can take the LAN.
	auto.Stop()
	if server.lanHolders.heldBy("testdev") != nil {
		t.Fatal("a stopped stream kept the LAN session")
	}
	if !autoA.lan.isStopped() {
		t.Fatal("the automatic stream's LAN source was not stopped")
	}
}

func TestAutomaticStreamTakesTheCloudWhileTheLocalPathHoldsTheLAN(t *testing.T) {
	server := newLANServer()
	local, _ := newLocalStream(t, server)
	fakeAttempts(t, local, nil)
	begin(local)

	auto, _ := newLANStream(t, server, "hd")
	a := fakeAttempts(t, auto, nil)
	begin(auto)

	if n := a.lanBuilt.Load(); n != 0 {
		t.Fatalf("built %d LAN sources, want none", n)
	}
	if n := a.cloudStarted.Load(); n != 1 {
		t.Fatalf("cloud started %d times, want 1", n)
	}
	if server.lanCoolingDown("testdev") {
		t.Fatal("a held LAN session counted as a LAN failure")
	}
	// Its move back to the LAN waits: the timer is armed, not fired at once.
	time.Sleep(50 * time.Millisecond)
	auto.mutex.Lock()
	timer := auto.promoteTimer
	pending := timer != nil && timer.Stop()
	auto.mutex.Unlock()
	if !pending {
		t.Fatal("the promotion probe did not wait while the local path holds the LAN")
	}
}

func TestPromotionProbeLeavesAHeldSessionAlone(t *testing.T) {
	server := newLANServer()
	local, _ := newLocalStream(t, server)
	fakeAttempts(t, local, nil)
	begin(local)

	auto, _ := newLANStream(t, server, "hd")
	a := fakeAttempts(t, auto, nil)
	auto.mutex.Lock()
	auto.promote() // releases the lock
	if n := a.lanBuilt.Load(); n != 0 {
		t.Fatalf("the probe built %d LAN sources on a held session", n)
	}
	if server.lanCoolingDown("testdev") {
		t.Fatal("a held LAN session counted as a failed probe")
	}
}

func TestStartStopsTheUnwatchedHolder(t *testing.T) {
	server := newLANServer()
	auto, _ := newLANStream(t, server, "hd")
	autoA := fakeAttempts(t, auto, nil)
	begin(auto)
	auto.mutex.Lock()
	delete(auto.clients, "s1") // in its 30 s shutdown delay
	auto.mutex.Unlock()

	local, _ := newLocalStream(t, server)
	fakeAttempts(t, local, nil)
	begin(local)

	if active, _, _ := streamState(local); !active {
		t.Fatal("the local stream did not take the LAN from an unwatched stream")
	}
	if active, _, _ := streamState(auto); active || !autoA.lan.isStopped() {
		t.Fatal("the unwatched automatic stream was not stopped")
	}
}

func TestAutomaticStartStopsAnUnwatchedLocalStream(t *testing.T) {
	server := newLANServer()
	local, _ := newLocalStream(t, server)
	localA := fakeAttempts(t, local, nil)
	begin(local)
	local.mutex.Lock()
	delete(local.clients, "s1") // in its 30 s shutdown delay
	local.mutex.Unlock()

	auto, _ := newLANStream(t, server, "hd")
	a := fakeAttempts(t, auto, nil)
	begin(auto)

	if a.lanBuilt.Load() != 1 || a.cloudStarted.Load() != 0 {
		t.Fatalf("LAN built %d, cloud started %d: want the LAN", a.lanBuilt.Load(), a.cloudStarted.Load())
	}
	if !localA.lan.isStopped() {
		t.Fatal("the unwatched local stream was not stopped")
	}
}

func TestLocalPathOverRTSP(t *testing.T) {
	h := newStallHarness(t, func(ctx context.Context, cs *CameraStream) error {
		t.Error("the local path started a cloud attempt")
		return errors.New("no cloud")
	})
	h.server.LanStreaming = true
	h.server.Talkback = true
	h.server.TalkbackMode = TalkbackOnDemand
	h.server.LANTargets = map[string]LANTarget{"cam1": {LocalKey: "lk", Password: "pw", IP: "192.0.2.10", Protocol: "3.5"}}
	created := h.server.newStream
	h.server.newStream = func(cs *CameraStream) {
		created(cs)
		cs.lanFactory = func(*RTPForwarder) lanStreamer { return &fakeLAN{} }
	}

	c := dialRTSP(t, h.port, "/cam1/local")
	code, _ := c.do("DESCRIBE", c.url, map[string]string{"Accept": "application/sdp"}, 2*time.Second)
	if code != 200 {
		t.Fatalf("DESCRIBE /cam1/local = %d, want 200", code)
	}
	deadline := time.Now().Add(2 * time.Second)
	var cs *CameraStream
	for cs == nil && time.Now().Before(deadline) {
		h.server.mutex.RLock()
		cs = h.server.streams["cam1-hd-local"]
		h.server.mutex.RUnlock()
		time.Sleep(10 * time.Millisecond)
	}
	if cs == nil || !cs.localOnly {
		t.Fatal("no local-path stream under cam1-hd-local")
	}
	if cs.talk != nil {
		t.Fatal("the local path has a talker, which would open a cloud session")
	}

	// The camera without a LAN path has no local one: 404 at once.
	if code := dialRTSP(t, h.port, "/cam2/local").describe(2 * time.Second); code != 404 {
		t.Fatalf("DESCRIBE /cam2/local = %d, want 404", code)
	}
}

func TestLocalPathOffersNoReturnChannel(t *testing.T) {
	// /cam1 with session talkback takes the cloud: one that comes up leaves
	// no start behind that sleeps out the retry delay into the next test.
	h := newStallHarness(t, func(context.Context, *CameraStream) error { return nil })
	h.server.LanStreaming = true
	h.server.Talkback = true
	h.server.LANTargets = map[string]LANTarget{"cam1": {LocalKey: "lk", Password: "pw", IP: "192.0.2.10", Protocol: "3.5"}}
	created := h.server.newStream
	h.server.newStream = func(cs *CameraStream) {
		created(cs)
		cs.lanFactory = func(*RTPForwarder) lanStreamer { return &fakeLAN{} }
	}
	for path, want := range map[string]bool{"/cam1/local": false, "/cam1": true} {
		c := dialRTSP(t, h.port, path)
		sdp := describeBody(t, c)
		if got := strings.Contains(sdp, "/backchannel"); got != want {
			t.Errorf("%s: return channel offered %v, want %v", path, got, want)
		}
	}
}

// describeBody sends DESCRIBE and returns the description.
func describeBody(t *testing.T, c *rtspTestClient) string {
	t.Helper()
	if _, err := c.conn.Write([]byte("DESCRIBE " + c.url + " RTSP/1.0\r\nCSeq: 1\r\nAccept: application/sdp\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	length := 0
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			t.Fatalf("DESCRIBE: %v", err)
		}
		if line == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			length, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(c.r, body); err != nil {
		t.Fatalf("DESCRIBE body: %v", err)
	}
	return string(body)
}

func TestLocalStreamHasNoTalkerAndNoReturnAudio(t *testing.T) {
	server := newLANServer()
	server.Talkback = true
	server.TalkbackMode = TalkbackOnDemand
	camera := &storage.CameraInfo{DeviceID: "testdev", DeviceName: "Test Cam"}
	for _, local := range []bool{false, true} {
		cs, err := server.getOrCreatePathStream(camera, "hd", &storage.UserSession{UserKey: "u"}, local)
		if err != nil {
			t.Fatal(err)
		}
		cs.forwarder.mutex.RLock()
		sink := cs.forwarder.backchannelSink
		cs.forwarder.mutex.RUnlock()
		if got := cs.talk != nil || sink != nil; got == local {
			t.Errorf("local=%v: talker %v, return audio sink %v", local, cs.talk != nil, sink != nil)
		}
	}
}

func TestLocalPathRefusesAReturnChannel(t *testing.T) {
	h := newStallHarness(t, func(context.Context, *CameraStream) error { return errors.New("no cloud") })
	h.server.LanStreaming = true
	h.server.Talkback = true
	h.server.TalkbackMode = TalkbackOnDemand
	h.server.LANTargets = map[string]LANTarget{"cam1": {LocalKey: "lk", Password: "pw", IP: "192.0.2.10", Protocol: "3.5"}}
	created := h.server.newStream
	h.server.newStream = func(cs *CameraStream) {
		created(cs)
		cs.lanFactory = func(*RTPForwarder) lanStreamer { return &fakeLAN{} }
	}

	c := dialRTSP(t, h.port, "/cam1/local")
	if code := c.describe(2 * time.Second); code != 200 {
		t.Fatalf("DESCRIBE = %d, want 200", code)
	}
	code, _ := c.do("SETUP", c.url+"/backchannel", map[string]string{"Transport": "RTP/AVP/TCP;unicast;interleaved=4-5"}, 2*time.Second)
	if code != 404 {
		t.Fatalf("SETUP /cam1/local/backchannel = %d, want 404", code)
	}
	if code, _ := c.do("SETUP", c.url+"/video", map[string]string{"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1"}, 2*time.Second); code != 200 {
		t.Fatalf("SETUP /cam1/local/video = %d, want 200", code)
	}
}

func TestLANHoldIsPerSource(t *testing.T) {
	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	fakeAttempts(t, cs, nil)
	cs.mutex.Lock()
	first := cs.newHeldLAN(NewRTPForwarder())
	second := cs.newHeldLAN(NewRTPForwarder())
	cs.mutex.Unlock()
	if first == nil || second != nil {
		t.Fatalf("first %v, second %v: want only the first, even from the same stream", first != nil, second != nil)
	}
	first.Stop()
	first.Stop() // a second stop of the same source gives nothing back twice
	cs.mutex.Lock()
	third := cs.newHeldLAN(NewRTPForwarder())
	cs.mutex.Unlock()
	if third == nil {
		t.Fatal("a stopped source kept the LAN session")
	}
	first.Stop()
	if server.lanHolders.heldBy("testdev") != cs {
		t.Fatal("a stale stop of an old source freed the session the new source holds")
	}
	third.Stop()
}

// gatedLAN is a promotion probe whose Start, once its context ends, returns
// only when gate is closed: an attempt that takes a while to end.
type gatedLAN struct {
	fakeLAN
	entered chan struct{}
	gate    chan struct{}
}

func (g *gatedLAN) Start(ctx context.Context) error {
	close(g.entered)
	<-ctx.Done()
	<-g.gate
	return ctx.Err()
}

// startProbe runs auto's promotion probe on probe in the background, after a
// cloud start, and returns once the probe is in its attempt; the channel
// closes when promote has returned. With unwatched, auto's client has left.
func startProbe(t *testing.T, server *RTSPServer, auto *CameraStream, probe *gatedLAN, unwatched bool) <-chan struct{} {
	t.Helper()
	server.startLANCooldown("testdev") // on the cloud first
	fakeAttempts(t, auto, nil)
	begin(auto)
	if active, _, lan := streamState(auto); !active || lan != nil {
		t.Fatalf("want an active cloud stream, active=%v lan=%v", active, lan)
	}
	server.lanWorked("testdev")
	auto.mutex.Lock()
	if auto.promoteTimer != nil {
		auto.promoteTimer.Stop()
		auto.promoteTimer = nil
	}
	auto.lanFactory = func(*RTPForwarder) lanStreamer { return probe }
	if unwatched {
		delete(auto.clients, "s1") // in its shutdown delay
	}
	done := make(chan struct{})
	go func() { auto.promote(); close(done) }() // releases the lock
	<-probe.entered
	return done
}

func TestStaleProbeStopKeepsARestartedStreamsHold(t *testing.T) {
	server := newLANServer()
	auto, _ := newLANStream(t, server, "hd")
	probe := &gatedLAN{entered: make(chan struct{}), gate: make(chan struct{})}
	done := startProbe(t, server, auto, probe, false)

	auto.stopStream() // stops the probe at once, and frees its session
	if !probe.isStopped() || server.lanHolders.heldBy("testdev") != nil {
		t.Fatalf("after the stop: probe stopped %v, session held by %p", probe.isStopped(), server.lanHolders.heldBy("testdev"))
	}
	restarted := &fakeLAN{}
	auto.lanFactory = func(*RTPForwarder) lanStreamer { return restarted }
	begin(auto) // a client restarts the same stream object
	if active, _, lan := streamState(auto); !active || lan == nil {
		t.Fatalf("restart: active=%v lan=%v, want the LAN", active, lan)
	}
	close(probe.gate) // the probe's attempt ends now, and stops its source again
	<-done

	if server.lanHolders.heldBy("testdev") != auto {
		t.Fatal("the probe's late stop freed the session the restarted stream streams on")
	}
	local, _ := newLocalStream(t, server)
	la := fakeAttempts(t, local, nil)
	begin(local)
	if n := la.lanBuilt.Load(); n != 0 {
		t.Fatalf("the local path built %d LAN sources next to the running one", n)
	}
	auto.stopStream()
}

func TestLocalStartTakesTheLANFromAnUnwatchedHoldersProbe(t *testing.T) {
	server := newLANServer()
	auto, _ := newLANStream(t, server, "hd")
	probe := &gatedLAN{entered: make(chan struct{}), gate: make(chan struct{})}
	done := startProbe(t, server, auto, probe, true)
	defer func() { close(probe.gate); <-done }()

	local, _ := newLocalStream(t, server)
	la := fakeAttempts(t, local, nil)
	begin(local)

	if la.lanBuilt.Load() != 1 || server.lanHolders.heldBy("testdev") != local {
		t.Fatalf("local built %d LAN sources, held by the local stream %v: the unwatched holder's probe kept the session",
			la.lanBuilt.Load(), server.lanHolders.heldBy("testdev") == local)
	}
	local.stopStream()
}

func TestRefusedProbeLooksAgainLater(t *testing.T) {
	server := newLANServer()
	local, _ := newLocalStream(t, server)
	fakeAttempts(t, local, nil)
	begin(local)

	auto, _ := newLANStream(t, server, "hd")
	fakeAttempts(t, auto, nil)
	auto.mutex.Lock()
	auto.promote() // refused: the local path holds the session
	auto.mutex.Lock()
	defer auto.mutex.Unlock()
	if auto.promoteTimer == nil || !auto.promoteTimer.Stop() {
		t.Fatal("a probe refused for a held session did not arm the next one")
	}
	auto.promoteTimer = nil
}

func TestLocalStreamLeftDuringItsAttemptSchedulesShutdown(t *testing.T) {
	server := newLANServer()
	cs, _ := newLocalStream(t, server)
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
	if !cs.active.Load() || cs.lan == nil {
		t.Fatal("expected the local stream on the LAN")
	}
	if cs.shutdownTimer == nil {
		t.Fatal("a local stream left without clients during the attempt must schedule its shutdown")
	}
}

// heldStopLAN records, at its Stop, whether its session was still held.
type heldStopLAN struct {
	fakeLAN
	server       *RTSPServer
	heldDuringIt bool
}

func (h *heldStopLAN) Stop() {
	h.heldDuringIt = h.server.lanHolders.heldBy("testdev") != nil
	h.fakeLAN.Stop()
}

func TestLANSessionIsFreedOnlyAfterItsSourceStopped(t *testing.T) {
	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	src := &heldStopLAN{server: server}
	cs.lanFactory = func(*RTPForwarder) lanStreamer { return src }
	cs.mutex.Lock()
	held := cs.newHeldLAN(NewRTPForwarder())
	cs.mutex.Unlock()

	held.Stop()

	if !src.heldDuringIt {
		t.Fatal("the session was given back before the source had closed it")
	}
	if server.lanHolders.heldBy("testdev") != nil {
		t.Fatal("a stopped source kept the session")
	}
}

func TestStartLeavesAHolderBeingHandedAClientAlone(t *testing.T) {
	server := newLANServer()
	auto, _ := newLANStream(t, server, "hd")
	autoA := fakeAttempts(t, auto, nil)
	begin(auto)
	auto.mutex.Lock()
	delete(auto.clients, "s1")
	auto.mutex.Unlock()
	auto.pending.Add(1) // a client has been handed the stream and attaches next
	defer auto.pending.Add(-1)

	local, _ := newLocalStream(t, server)
	la := fakeAttempts(t, local, nil)
	begin(local)

	if active, _, _ := streamState(auto); !active || autoA.lan.isStopped() {
		t.Fatal("a stream a client is attaching to was stopped for the other path")
	}
	if n := la.lanBuilt.Load(); n != 0 {
		t.Fatalf("the local path built %d LAN sources next to it", n)
	}
	auto.stopStream()
}

// The 404 pre-check is the local path's own: over RTSP, /cam1/local answers
// during the automatic path's cool-down and with session talkback, which keep
// only the automatic path off the LAN.
func TestLocalPathAnswersDuringACooldownAndWithSessionTalkback(t *testing.T) {
	for _, mode := range []string{"cooldown", "session-talkback"} {
		t.Run(mode, func(t *testing.T) {
			h := newStallHarness(t, func(ctx context.Context, cs *CameraStream) error { return errors.New("no cloud") })
			h.server.LanStreaming = true
			h.server.LANTargets = map[string]LANTarget{"cam1": {LocalKey: "lk", Password: "pw", IP: "192.0.2.10", Protocol: "3.5"}}
			if mode == "cooldown" {
				h.server.startLANCooldown("cam1")
			} else {
				h.server.Talkback = true
				h.server.TalkbackMode = TalkbackSession
			}
			created := h.server.newStream
			h.server.newStream = func(cs *CameraStream) {
				created(cs)
				cs.lanFactory = func(*RTPForwarder) lanStreamer { return &fakeLAN{} }
			}
			if code := dialRTSP(t, h.port, "/cam1/local").describe(2 * time.Second); code != 200 {
				t.Fatalf("%s: DESCRIBE /cam1/local = %d, want 200", mode, code)
			}
		})
	}
}
