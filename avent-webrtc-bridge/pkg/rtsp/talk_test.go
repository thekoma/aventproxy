package rtsp

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"

	"github.com/pion/rtp"
)

// fakeTalk is a talk session the test drives.
type fakeTalk struct {
	startErr  error
	release   chan struct{} // Start waits on it when set
	ready     atomic.Bool
	forwarded atomic.Int32
	stopped   atomic.Bool
	onError   func(error)
}

func (f *fakeTalk) Start() error {
	if f.release != nil {
		<-f.release
	}
	return f.startErr
}
func (f *fakeTalk) Stop()                                     { f.stopped.Store(true) }
func (f *fakeTalk) BackchannelReady() bool                    { return f.ready.Load() }
func (f *fakeTalk) ForwardBackchannelAudioPacket(*rtp.Packet) { f.forwarded.Add(1) }

// talkOpener hands out the sessions a test prepared, in order, and records them.
type talkOpener struct {
	mu       sync.Mutex
	next     func() *fakeTalk
	sessions []*fakeTalk
}

func (o *talkOpener) open(onError func(error)) (talkSession, error) {
	f := o.next()
	f.onError = onError
	o.mu.Lock()
	o.sessions = append(o.sessions, f)
	o.mu.Unlock()
	return f, nil
}

func (o *talkOpener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.sessions)
}

func (o *talkOpener) session(i int) *fakeTalk {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sessions[i]
}

func newTestTalker(o *talkOpener) *talker {
	t := newTalker("test camera", o.open)
	t.idleAfter = 80 * time.Millisecond
	t.retryWait = 200 * time.Millisecond
	return t
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sessionUp(tk *talker) bool {
	tk.mu.Lock()
	defer tk.mu.Unlock()
	return tk.session != nil
}

func TestTalkerOpensOneSessionAndForwardsOnceItCanCarryAudio(t *testing.T) {
	o := &talkOpener{next: func() *fakeTalk { return &fakeTalk{} }}
	tk := newTestTalker(o)
	defer tk.end("test over")

	for range 5 {
		tk.packet(&rtp.Packet{})
	}
	eventually(t, "the talk session", func() bool { return sessionUp(tk) })
	tk.packet(&rtp.Packet{}) // up, but its backchannel track is not there yet
	s := o.session(0)
	if got := s.forwarded.Load(); got != 0 {
		t.Fatalf("forwarded %d packets before the session could carry them", got)
	}

	s.ready.Store(true)
	tk.packet(&rtp.Packet{})
	tk.packet(&rtp.Packet{})
	if got := s.forwarded.Load(); got != 2 {
		t.Fatalf("forwarded %d packets once ready, want 2", got)
	}
	if o.count() != 1 {
		t.Fatalf("opened %d sessions for one talk, want 1", o.count())
	}
}

func TestTalkerEndsTheSessionWhenTheAudioStops(t *testing.T) {
	o := &talkOpener{next: func() *fakeTalk { f := &fakeTalk{}; f.ready.Store(true); return f }}
	tk := newTestTalker(o)
	defer tk.end("test over")

	tk.packet(&rtp.Packet{})
	eventually(t, "the talk session", func() bool { return sessionUp(tk) })
	// Packets keep it open past the idle time.
	for range 6 {
		time.Sleep(30 * time.Millisecond)
		tk.packet(&rtp.Packet{})
	}
	if o.session(0).stopped.Load() {
		t.Fatal("the session ended while audio was still arriving")
	}

	eventually(t, "the session to end after the audio stopped", func() bool { return o.session(0).stopped.Load() })
	if sessionUp(tk) {
		t.Fatal("the talker still holds the ended session")
	}

	tk.packet(&rtp.Packet{}) // a new talk opens a new session
	eventually(t, "a second session", func() bool { return o.count() == 2 })
}

func TestTalkerWaitsBeforeRetryingAFailedSession(t *testing.T) {
	failing := true
	var mu sync.Mutex
	o := &talkOpener{next: func() *fakeTalk {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return &fakeTalk{startErr: errors.New("camera said no")}
		}
		return &fakeTalk{}
	}}
	tk := newTestTalker(o)
	tk.idleAfter = time.Second // the talk stays on through the test
	defer tk.end("test over")

	tk.packet(&rtp.Packet{})
	eventually(t, "the failed session to be stopped", func() bool { return o.count() == 1 && o.session(0).stopped.Load() })
	for range 5 {
		tk.packet(&rtp.Packet{})
	}
	time.Sleep(20 * time.Millisecond)
	if o.count() != 1 {
		t.Fatalf("opened %d sessions within the retry wait, want 1", o.count())
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	time.Sleep(tk.retryWait)
	tk.packet(&rtp.Packet{})
	eventually(t, "a retry after the wait", func() bool { return sessionUp(tk) })
	if o.count() != 2 {
		t.Fatalf("opened %d sessions, want 2", o.count())
	}
}

func TestTalkerStopsASessionThatCameUpAfterTheTalkEnded(t *testing.T) {
	release := make(chan struct{})
	o := &talkOpener{next: func() *fakeTalk { return &fakeTalk{release: release} }}
	tk := newTestTalker(o)

	tk.packet(&rtp.Packet{})
	eventually(t, "the session to be opened", func() bool { return o.count() == 1 })
	tk.end("the stream stopped") // while Start still waits
	close(release)

	eventually(t, "the late session to be stopped", func() bool { return o.session(0).stopped.Load() })
	if sessionUp(tk) {
		t.Fatal("a session that came up after its talk ended was kept")
	}
}

func TestTalkerEndsTheTalkWhenItsSessionFails(t *testing.T) {
	o := &talkOpener{next: func() *fakeTalk { f := &fakeTalk{}; f.ready.Store(true); return f }}
	tk := newTestTalker(o)
	tk.idleAfter = time.Second
	defer tk.end("test over")

	tk.packet(&rtp.Packet{})
	eventually(t, "the talk session", func() bool { return sessionUp(tk) })
	o.session(0).onError(errors.New("peer connection failed"))

	if !o.session(0).stopped.Load() || sessionUp(tk) {
		t.Fatal("a failed session must be stopped and dropped")
	}
	tk.packet(&rtp.Packet{})
	time.Sleep(20 * time.Millisecond)
	if o.count() != 1 {
		t.Fatalf("opened %d sessions right after a failure, want 1", o.count())
	}
}

func TestParseTalkbackMode(t *testing.T) {
	for in, want := range map[string]string{"": TalkbackSession, "session": TalkbackSession, "on_demand": TalkbackOnDemand} {
		if got, err := ParseTalkbackMode(in); err != nil || got != want {
			t.Errorf("ParseTalkbackMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseTalkbackMode("always"); err == nil {
		t.Error("an unknown mode must be refused")
	}
}

// On demand, the camera session stays one-way and the backchannel feeds the
// talker; in session mode nothing changes.
func TestTalkOnDemandKeepsTheCameraSessionOneWay(t *testing.T) {
	camera := &storage.CameraInfo{DeviceID: "d", DeviceName: "n"}

	server, _ := newTestCameraStream(t)
	server.Talkback = true
	server.TalkbackMode = TalkbackOnDemand
	stream := NewCameraStream(camera, "hd", nil, nil, server)
	if stream.webrtcBridge.Talkback {
		t.Error("on demand, the camera session must not ask for talkback")
	}
	if stream.talk == nil {
		t.Fatal("on demand, the stream needs a talker")
	}
	o := &talkOpener{next: func() *fakeTalk { return &fakeTalk{} }}
	stream.talk.open = o.open
	defer stream.talk.end("test over")

	stream.mutex.Lock()
	stream.replaceBridge() // a replacement must not take the backchannel back
	stream.mutex.Unlock()
	stream.forwarder.ForwardBackchannel(&rtp.Packet{})
	eventually(t, "the talker to open a session", func() bool { return o.count() == 1 })

	server.TalkbackMode = TalkbackSession
	session := NewCameraStream(camera, "hd", nil, nil, server)
	if !session.webrtcBridge.Talkback || session.talk != nil {
		t.Error("in session mode the camera session asks for talkback and no talker exists")
	}
}

// Talk on demand needs neither imm nor the LAN to carry return audio, so it
// leaves both to the stream.
func TestTalkOnDemandLeavesIMMAndTheLANEligible(t *testing.T) {
	onDemand := func(s *RTSPServer) {
		s.Talkback = true
		s.TalkbackMode = TalkbackOnDemand
	}

	immServer := newIMMServer(CloudTransportIMM)
	onDemand(immServer)
	cs, _ := newLANStream(t, immServer, "hd")
	if ok, reason := cs.immEligible(); !ok {
		t.Errorf("immEligible() = false, %q; want true with talk on demand", reason)
	}

	lanServer := newLANServer()
	onDemand(lanServer)
	cs, _ = newLANStream(t, lanServer, "hd")
	if ok, reason := cs.lanEligible(); !ok {
		t.Errorf("lanEligible() = false, %q; want true with talk on demand", reason)
	}
}
