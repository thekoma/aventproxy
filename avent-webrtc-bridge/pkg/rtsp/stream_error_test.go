package rtsp

import (
	"errors"
	"io"
	"net"
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
