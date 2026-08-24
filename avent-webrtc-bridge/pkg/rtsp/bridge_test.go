package rtsp

import (
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
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
