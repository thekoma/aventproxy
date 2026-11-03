package rtsp

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"

	"avent-webrtc-bridge/pkg/storage"
)

func TestHandlePeerConnectionStateChangeUnblocksSDH264(t *testing.T) {
	wb := &WebRTCBridge{
		resolution: "sd",
		isHEVC:     false,
	}

	wait := wb.waiter.WaitChan()
	wb.handlePeerConnectionStateChange(pion.PeerConnectionStateConnected)

	select {
	case err := <-wait:
		if err != nil {
			t.Fatalf("expected successful connection, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SD H264 connection did not unblock the bridge waiter")
	}
}

// A Start that timed out waiting for the answer leaves a PeerConnection that
// never connected; stopping the bridge must close it all the same.
func TestStopClosesANeverConnectedBridge(t *testing.T) {
	wb := NewWebRTCBridge(&storage.CameraInfo{DeviceName: "Nursery"}, "hd", nil, nil, nil)
	pc, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	wb.peerConnection = pc // what Start leaves behind on ErrWaitTimeout

	wb.Stop()
	if state := pc.ConnectionState(); state != pion.PeerConnectionStateClosed {
		t.Fatalf("PeerConnection %v after Stop, want closed", state)
	}
	if wb.ctx.Err() == nil {
		t.Fatal("Stop left the bridge context running")
	}
}

// Issue #72: whether audio keeps flowing once the monitor is asked to keep its
// speaker closed is read from the session's packet counts, logged at Stop.
func TestStopLogsTheSessionsPacketCounts(t *testing.T) {
	name := fmt.Sprintf("Count Cam %d", time.Now().UnixNano())
	wb := NewWebRTCBridge(&storage.CameraInfo{DeviceName: name}, "hd", nil, nil, nil)
	pc, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	wb.peerConnection = pc
	wb.forwardVideo(&rtp.Packet{Payload: []byte{0x65, 0x88}})
	wb.forwardVideo(&rtp.Packet{Payload: []byte{0x41, 0x9a}})
	wb.forwardAudio(&rtp.Packet{Payload: make([]byte, 160)})

	wb.Stop()
	if lines := testLog.lines("Cloud session for " + name + " carried 2 video and 1 audio packets"); len(lines) != 1 {
		t.Fatalf("want one packet-count line for %s, got %q", name, lines)
	}
}

// controlRecorder stands in for the MQTT camera client's 312 controls.
type controlRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *controlRecorder) SendResolution(v int) error { return r.record("resolution", v) }
func (r *controlRecorder) SendSpeaker(v int) error    { return r.record("speaker", v) }

func (r *controlRecorder) record(what string, v int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf("%s %d", what, v))
	return nil
}

func (r *controlRecorder) sent() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.calls, ", ")
}

// connect drives the bridge's Connected callback and reports whether it
// released Start.
func connect(wb *WebRTCBridge) (released bool) {
	wait := wb.waiter.WaitChan()
	wb.handlePeerConnectionStateChange(pion.PeerConnectionStateConnected)
	select {
	case err := <-wait:
		return err == nil
	case <-time.After(100 * time.Millisecond):
		return false
	}
}

// Issue #72: a webrtc-mode session opens the monitor's speaker, which pauses
// a playing lullaby. Once connected, a session without talkback asks for the
// speaker to stay closed, with the 312 control that SendSpeaker sends.
func TestConnectedSessionControls(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resolution string
		hevc       bool
		talkback   bool
		speakerOff bool
		want       string
		released   bool
	}{
		{"HD without talkback", "hd", false, false, true, "resolution 0, speaker 0", true},
		{"talkback keeps the speaker", "hd", false, true, true, "resolution 0", true},
		{"SD asks only for the speaker", "sd", false, false, true, "speaker 0", true},
		// HEVC waits for its data channel before Start returns, and asks for no resolution.
		{"HEVC", "hd", true, false, true, "speaker 0", false},
		{"switched off", "hd", false, false, false, "resolution 0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &controlRecorder{}
			wb := &WebRTCBridge{
				camera:     &storage.CameraInfo{DeviceName: "Nursery"},
				resolution: tc.resolution,
				isHEVC:     tc.hevc,
				Talkback:   tc.talkback,
				SpeakerOff: tc.speakerOff,
				controls:   rec,
			}
			if released := connect(wb); released != tc.released {
				t.Errorf("Start released = %v, want %v", released, tc.released)
			}
			if got := rec.sent(); got != tc.want {
				t.Errorf("sent %q, want %q", got, tc.want)
			}
		})
	}
}

// A bridge whose camera client never came up (Start failed early) must not
// panic when pion reports a state change.
func TestConnectedWithoutCameraClient(t *testing.T) {
	wb := &WebRTCBridge{camera: &storage.CameraInfo{DeviceName: "Nursery"}, resolution: "hd", SpeakerOff: true}
	if !connect(wb) {
		t.Fatal("Start not released")
	}
}

// Speaker-off stays off unless asked for: until a tester confirms it, only the
// Dev card turns it on (issue #72), and every other card behaves as before.
func TestStreamBridgesTakeTheServersSettings(t *testing.T) {
	server, cs := newTestCameraStream(t)
	if server.SpeakerOff || cs.webrtcBridge.SpeakerOff {
		t.Fatal("speaker-off must be off by default")
	}
	server.SpeakerOff = true
	cs.mutex.Lock()
	cs.replaceBridge()
	cs.mutex.Unlock()
	if !cs.webrtcBridge.SpeakerOff {
		t.Fatal("a replacement bridge did not take SpeakerOff from the server")
	}
}

// The webrtc_audio experiment knob (issue #72): "none" leaves the audio
// section out of the offer to the camera, to learn whether the monitor opens
// its speaker only when audio is negotiated. Talkback needs audio, so it wins.
func TestOfferMedias(t *testing.T) {
	for _, tc := range []struct {
		talkback bool
		audio    string
		want     string
	}{
		{false, WebRTCAudioRecvonly, "audio recvonly, video recvonly"},
		{false, WebRTCAudioNone, "video recvonly"},
		{true, WebRTCAudioRecvonly, "audio sendrecv, video recvonly"},
		{true, WebRTCAudioNone, "audio sendrecv, video recvonly"},
		{false, "", "audio recvonly, video recvonly"}, // a bridge built without the setting
	} {
		var got []string
		for _, m := range offerMedias(tc.talkback, tc.audio) {
			got = append(got, m.Kind+" "+m.Direction)
		}
		if s := strings.Join(got, ", "); s != tc.want {
			t.Errorf("talkback=%v audio=%q: offer %q, want %q", tc.talkback, tc.audio, s, tc.want)
		}
	}
}

// Whatever the camera is offered, RTSP clients keep the same description, so
// the experiment never changes what a consumer has to cope with.
func TestRTSPDescriptionIgnoresWebRTCAudio(t *testing.T) {
	server, cs := newTestCameraStream(t)
	before := server.generateSDP(cs.camera, "rtsp://localhost/cam")
	server.WebRTCAudio = WebRTCAudioNone
	after := server.generateSDP(cs.camera, "rtsp://localhost/cam")
	strip := func(sdp string) string { return sdp[strings.Index(sdp, "s="):] } // o= carries the time
	if strip(before) != strip(after) || !strings.Contains(after, "m=audio") {
		t.Fatalf("the RTSP description changed with webrtc_audio=none:\n%s\n---\n%s", before, after)
	}
}

func TestParseWebRTCAudio(t *testing.T) {
	for in, want := range map[string]string{"": WebRTCAudioRecvonly, "recvonly": WebRTCAudioRecvonly, "none": WebRTCAudioNone} {
		if got, err := ParseWebRTCAudio(in); err != nil || got != want {
			t.Errorf("ParseWebRTCAudio(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseWebRTCAudio("sendrecv"); err == nil {
		t.Error("an unknown mode must be refused")
	}
}

func TestStreamBridgesTakeWebRTCAudio(t *testing.T) {
	server, cs := newTestCameraStream(t)
	if server.WebRTCAudio != WebRTCAudioRecvonly || cs.webrtcBridge.WebRTCAudio != WebRTCAudioRecvonly {
		t.Fatalf("defaults: server %q, bridge %q; want recvonly", server.WebRTCAudio, cs.webrtcBridge.WebRTCAudio)
	}
	server.WebRTCAudio = WebRTCAudioNone
	cs.mutex.Lock()
	cs.replaceBridge()
	cs.mutex.Unlock()
	if cs.webrtcBridge.WebRTCAudio != WebRTCAudioNone {
		t.Fatal("a replacement bridge did not take WebRTCAudio from the server")
	}
}
