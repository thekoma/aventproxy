package rtsp

import (
	"net"
	"testing"

	"github.com/pion/rtp"
)

// A retry, or a restart of a stream a client reconnected to, replaces the
// WebRTC bridge. Clients set up on the old one must keep receiving media.
func TestClientsSurviveBridgeReplacement(t *testing.T) {
	_, cs := newTestCameraStream(t)
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	if err := cs.webrtcBridge.rtpForwarder.AddTCPClient("s1", server, 0, 2, 4); err != nil {
		t.Fatal(err)
	}

	cs.mutex.Lock()
	cs.replaceBridge()
	cs.mutex.Unlock()

	wait := collect(t, client, 1)
	cs.webrtcBridge.rtpForwarder.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{Marker: true}, Payload: []byte{0x65, 0x88}})
	wait()
}

// SETUP and PLAY run on the client's goroutine while startStream may be
// swapping the bridge under the stream's lock (run with -race).
func TestPlayDuringBridgeReplacement(t *testing.T) {
	server, client, peer := newProtocolClient(t)
	cs := client.stream
	client.transportMode = TransportTCP

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			cs.mutex.Lock()
			cs.replaceBridge()
			cs.mutex.Unlock()
		}
	}()
	for range 50 {
		go server.handlePlay(client, &RTSPRequest{Method: "PLAY", Headers: map[string]string{"Session": "s1"}})
		if status := peer.readResponse(t); status != "RTSP/1.0 200 OK" {
			t.Fatalf("PLAY: %q", status)
		}
	}
	<-done
}
