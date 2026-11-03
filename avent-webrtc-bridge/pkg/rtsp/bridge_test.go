package rtsp

import (
	"fmt"
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
