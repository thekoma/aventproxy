package rtsp

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"
)

func newTestCameraStream(t *testing.T) (*RTSPServer, *CameraStream) {
	t.Helper()
	server := NewRTSPServer(0, nil)
	camera := &storage.CameraInfo{
		DeviceID:   "testdev",
		DeviceName: "Test Cam",
	}
	user := &storage.UserSession{UserKey: "user1"}
	stream := NewCameraStream(camera, "hd", user, nil, server)
	stream.attachBridgeErrorHandler()
	server.streams[stream.streamId] = stream
	return server, stream
}

func waitStreamRemoved(t *testing.T, server *RTSPServer, streamId string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		server.mutex.RLock()
		_, exists := server.streams[streamId]
		server.mutex.RUnlock()
		if !exists {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stream %s still in server map after timeout", streamId)
}

func TestHandleBridgeErrorClosesClientsAndClearsActive(t *testing.T) {
	server, stream := newTestCameraStream(t)
	stream.active = true

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	client := &RTSPClient{
		conn:    serverConn,
		session: "sess1",
		stream:  stream,
	}
	stream.clients[client.session] = client

	stream.handleBridgeError(errors.New("WebRTC connection failed/closed"))

	stream.mutex.RLock()
	active := stream.active
	connecting := stream.connecting
	stream.mutex.RUnlock()
	if active || connecting {
		t.Fatalf("expected inactive stream after error, active=%v connecting=%v", active, connecting)
	}

	waitStreamRemoved(t, server, stream.streamId)

	_ = clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1)
	_, err := clientConn.Read(buf)
	if err != io.EOF && !errors.Is(err, net.ErrClosed) && err.Error() != "io: read/write on closed pipe" {
		t.Fatalf("expected closed client conn (EOF), got %v", err)
	}
}

func TestHandleBridgeErrorReentrancy(t *testing.T) {
	server, stream := newTestCameraStream(t)
	stream.active = true

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	client := &RTSPClient{
		conn:    serverConn,
		session: "sess1",
		stream:  stream,
	}
	stream.clients[client.session] = client

	stream.handleBridgeError(errors.New("first"))
	// Second call must be a no-op (already torn down / handlingError guard).
	stream.handleBridgeError(errors.New("second"))

	stream.mutex.RLock()
	active := stream.active
	handling := stream.handlingError
	stream.mutex.RUnlock()
	if active {
		t.Fatal("expected inactive after errors")
	}
	if handling {
		t.Fatal("handlingError should be cleared after handleBridgeError returns")
	}

	waitStreamRemoved(t, server, stream.streamId)
}

func TestRemoveStreamSkipsReplacementInstance(t *testing.T) {
	server, oldStream := newTestCameraStream(t)
	oldStream.active = true

	// Simulate teardown scheduling async remove, then a fast reconnect
	// registering a new CameraStream under the same streamId.
	oldStream.stopStreamInternal()

	replacement := NewCameraStream(oldStream.camera, oldStream.resolution, oldStream.user, nil, server)
	replacement.active = true
	server.mutex.Lock()
	server.streams[replacement.streamId] = replacement
	server.mutex.Unlock()

	// Stale cleanup from the old stream must not delete the replacement.
	server.removeStream(oldStream)

	server.mutex.RLock()
	got := server.streams[replacement.streamId]
	server.mutex.RUnlock()
	if got != replacement {
		t.Fatalf("stale removeStream deleted replacement stream")
	}

	// Cleanup of the live instance still works.
	server.removeStream(replacement)
	server.mutex.RLock()
	_, exists := server.streams[replacement.streamId]
	server.mutex.RUnlock()
	if exists {
		t.Fatal("expected replacement stream to be removed by its own cleanup")
	}
}

func TestAddClientRestartsAfterBridgeError(t *testing.T) {
	_, stream := newTestCameraStream(t)
	stream.active = true

	started := make(chan struct{}, 1)
	stream.startStreamOverride = func() {
		started <- struct{}{}
	}

	stream.handleBridgeError(errors.New("WebRTC connection failed/closed"))

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	client := &RTSPClient{
		conn:    serverConn,
		session: "sess2",
		stream:  stream,
	}
	stream.AddClient(client)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("AddClient did not restart stream after bridge error")
	}

	stream.mutex.RLock()
	connecting := stream.connecting
	stream.mutex.RUnlock()
	if !connecting {
		t.Fatal("expected connecting=true after AddClient on inactive stream")
	}
}

func TestReplaceBridgeReattachesErrorHandler(t *testing.T) {
	// After a teardown the bridge is unusable (Stop cancels its context) and its
	// OnError has been cleared. A stream reused by a fast reconnect must get a
	// fresh bridge with the handler attached, otherwise the next WebRTC failure
	// is never noticed and the stream stays active with dead video (issue #68).
	_, stream := newTestCameraStream(t)
	stream.active = true

	first := stream.webrtcBridge
	if first == nil || first.OnError == nil {
		t.Fatal("expected a bridge with an error handler on a new stream")
	}

	stream.handleBridgeError(errors.New("WebRTC connection failed/closed"))

	if first.OnError != nil {
		t.Error("teardown should clear OnError on the failed bridge")
	}

	stream.mutex.Lock()
	stream.replaceBridge()
	stream.mutex.Unlock()

	if stream.webrtcBridge == first {
		t.Fatal("expected a replacement bridge, got the stopped one")
	}
	if stream.webrtcBridge.OnError == nil {
		t.Fatal("replacement bridge must have the error handler attached")
	}
}

func TestStartStreamReplacesAnAlreadyStartedBridge(t *testing.T) {
	// Guards the branch condition in startStream: a bridge that has run once is
	// never restarted in place.
	_, stream := newTestCameraStream(t)

	stream.mutex.Lock()
	stream.bridgeStarted = true
	first := stream.webrtcBridge
	stream.replaceBridge()
	stream.mutex.Unlock()

	if stream.webrtcBridge == first {
		t.Fatal("a started bridge must be replaced, not reused")
	}

	// A brand new stream has not started its bridge yet, so nothing is thrown away.
	_, fresh := newTestCameraStream(t)
	if fresh.bridgeStarted {
		t.Fatal("a new stream must not be marked as already started")
	}
}

func TestTalkbackOffByDefaultOnTheBridge(t *testing.T) {
	// The audio direction in the offer is what makes the camera set DPS 253 and
	// stop a playing lullaby (issue #72), so a bridge must not ask for it unless
	// the server was told to.
	_, stream := newTestCameraStream(t)
	if stream.webrtcBridge.Talkback {
		t.Error("a bridge built from a default server must not request talkback")
	}

	server, _ := newTestCameraStream(t)
	server.Talkback = true
	replacement := NewCameraStream(&storage.CameraInfo{DeviceID: "d", DeviceName: "n"}, "hd", nil, nil, server)
	if !replacement.webrtcBridge.Talkback {
		t.Error("a bridge built from a talkback server must request it")
	}

	replacement.mutex.Lock()
	replacement.replaceBridge()
	replacement.mutex.Unlock()
	if !replacement.webrtcBridge.Talkback {
		t.Error("a replacement bridge must keep the server's talkback setting")
	}
}

func TestSDPAdvertisesBackchannelOnlyWithTalkback(t *testing.T) {
	// Clients set up whatever the description offers, and a client setting up the
	// return channel is what coincides with the camera interrupting a lullaby
	// (issue #72), so the section must be absent unless talkback is on.
	camera := &storage.CameraInfo{DeviceID: "dev1", DeviceName: "Erik"}

	off := NewRTSPServer(0, nil)
	sdp := off.generateSDP(camera, "rtsp://host:38554/Erik")
	if strings.Contains(sdp, "/backchannel") {
		t.Error("talkback off: the SDP must not advertise a return channel")
	}
	if !strings.Contains(sdp, "a=recvonly") || !strings.Contains(sdp, "m=audio") {
		t.Error("talkback off must still offer the camera's own audio to the client")
	}

	on := NewRTSPServer(0, nil)
	on.Talkback = true
	sdp = on.generateSDP(camera, "rtsp://host:38554/Erik")
	if !strings.Contains(sdp, "/backchannel") {
		t.Error("talkback on: the SDP must advertise the return channel")
	}
	if !strings.Contains(sdp, "a=sendonly") {
		t.Error("talkback on: the return channel must be marked sendonly")
	}
}
