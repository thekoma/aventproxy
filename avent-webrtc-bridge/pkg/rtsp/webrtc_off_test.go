package rtsp

import (
	"context"
	"errors"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"
	"avent-webrtc-bridge/pkg/tuya"
)

func TestWebRTCOffServesEveryPathLocally(t *testing.T) {
	h := newStallHarness(t, func(ctx context.Context, cs *CameraStream) error {
		t.Error("WebRTC off started a cloud attempt")
		return errors.New("no cloud")
	})
	h.server.NoWebRTC = true
	h.server.LanStreaming = true
	h.server.LANTargets = map[string]LANTarget{"cam1": {LocalKey: "lk", Password: "pw", IP: "192.0.2.10", Protocol: "3.5"}}
	created := h.server.newStream
	h.server.newStream = func(cs *CameraStream) {
		created(cs)
		cs.lanFactory = func(*RTPForwarder) lanStreamer { return &fakeLAN{} }
	}

	c := dialRTSP(t, h.port, "/cam1")
	if code := c.describe(2 * time.Second); code != 200 {
		t.Fatalf("DESCRIBE /cam1 = %d, want 200", code)
	}
	deadline := time.Now().Add(2 * time.Second)
	var cs *CameraStream
	for cs == nil && time.Now().Before(deadline) {
		h.server.mutex.RLock()
		cs = h.server.streams["cam1-hd-local"]
		h.server.mutex.RUnlock()
		time.Sleep(10 * time.Millisecond)
	}
	if cs == nil {
		t.Fatal("/cam1 was not served as the local path")
	}
	// /cam1/local is the same path, so it shares /cam1's stream.
	if code := dialRTSP(t, h.port, "/cam1/local").describe(2 * time.Second); code != 200 {
		t.Fatalf("DESCRIBE /cam1/local = %d, want 200", code)
	}
	h.server.mutex.RLock()
	n, same := len(h.server.streams), h.server.streams["cam1-hd-local"] == cs
	h.server.mutex.RUnlock()
	if n != 1 || !same {
		t.Fatalf("after /cam1 and /cam1/local: %d streams, the same one %v; want one shared stream", n, same)
	}
	// The LAN path serves HD only, so the SD stream has no path at all.
	if code := dialRTSP(t, h.port, "/cam1/sd").describe(2 * time.Second); code != 404 {
		t.Fatalf("DESCRIBE /cam1/sd = %d, want 404", code)
	}
	// Without a LAN path there is nothing to serve.
	if code := dialRTSP(t, h.port, "/cam2").describe(2 * time.Second); code != 404 {
		t.Fatalf("DESCRIBE /cam2 = %d, want 404", code)
	}
}

func TestWebRTCOffMakesNoMQTTManager(t *testing.T) {
	t.Chdir(t.TempDir())
	sm, err := storage.NewStorageManager()
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []bool{false, true} {
		s := NewRTSPServer(freePort(t), sm)
		s.BindAddress = "127.0.0.1"
		s.MobileClient = &tuya.MobileSDKClient{}
		s.NoWebRTC = off
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		made := s.mqttManager != nil
		_ = s.Stop()
		if made == off {
			t.Errorf("NoWebRTC %v: MQTT manager made %v", off, made)
		}
	}
}
