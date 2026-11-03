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

func TestStreamBridgesTakeTheServersSettings(t *testing.T) {
	server, cs := newTestCameraStream(t)
	if !server.SpeakerOff {
		t.Fatal("a new server must ask for the speaker off by default")
	}
	if !cs.webrtcBridge.SpeakerOff {
		t.Fatal("the stream's first bridge did not take SpeakerOff from the server")
	}
	server.SpeakerOff = false
	cs.mutex.Lock()
	cs.replaceBridge()
	cs.mutex.Unlock()
	if cs.webrtcBridge.SpeakerOff {
		t.Fatal("a replacement bridge did not take SpeakerOff from the server")
	}
}
