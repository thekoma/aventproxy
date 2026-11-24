package rtsp

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/tuya"
)

// Issue #162: the cloud attempt runs without the stream's lock, as the LAN and
// imm attempts do, so a stop ends it rather than waiting for it. Held, the
// lock made a stop, the inactivity sweep and every leaving client wait out the
// MQTT back-off and the answer timeout, twice: minutes with the broker down,
// longer than Home Assistant gives the add-on to stop.

// within runs f and fails the test if it has not returned after d.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s still waiting after %v", what, d)
	}
}

// The add-on's stop: a camera that never answers must not hold it.
func TestServerStopDoesNotWaitForACloudAttempt(t *testing.T) {
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	h := newStallHarness(t, func(ctx context.Context, _ *CameraStream) error {
		signal(started)
		<-ctx.Done() // the camera never answers
		signal(cancelled)
		return ctx.Err()
	})
	client := dialRTSP(t, h.port, "/cam1")
	client.play(time.Second)
	receive(t, started, "the stream never started its cloud attempt")

	within(t, time.Second, "the inactivity sweep", h.server.cleanupInactiveStreams)
	within(t, time.Second, "the server's Stop", func() { _ = h.server.Stop() })
	receive(t, cancelled, "the cloud attempt never saw the stop")
	client.expectClosedWithin(2 * time.Second)
}

// The real cloud attempt hands its stop to the MQTT connect, which then
// sleeps no more of its back-off.
func TestStopEndsTheBrokerWaitOfACloudAttempt(t *testing.T) {
	server, cs := newTestCameraStream(t)
	m := NewMQTTManager(nil)
	connecting := make(chan struct{}, 1)
	gaveUp := make(chan struct{}, 1)
	m.connectFn = func(ctx context.Context, _ string) (*tuya.MQTTClient, error) {
		signal(connecting)
		<-ctx.Done() // the broker is unreachable
		signal(gaveUp)
		return nil, ctx.Err()
	}
	server.mqttManager = m
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		begin(cs)
	}()
	receive(t, connecting, "the cloud attempt never asked for the broker")

	within(t, time.Second, "the stream's Stop", cs.Stop)
	receive(t, gaveUp, "the MQTT connect never saw the stop")
	receive(t, finished, "the start never returned after its stop")
	within(t, time.Second, "the MQTT manager's Stop", m.Stop)
	if active, connecting, _ := streamState(cs); active || connecting {
		t.Fatalf("active=%v connecting=%v after the stop", active, connecting)
	}
}

// A stopped attempt can outlive its stop, in a call that does not watch its
// context. A client restarting the stream meanwhile must neither wait for it
// nor have its own session undone when it returns, even with a success.
func TestRestartWhileAStoppedCloudAttemptLingers(t *testing.T) {
	_, cs := newTestCameraStream(t)
	t.Cleanup(cs.stopStream)
	release := make(chan struct{})
	first := make(chan *WebRTCBridge, 1)
	var calls atomic.Int32
	cs.cloudStart = func(_ context.Context, bridge *WebRTCBridge) error {
		if calls.Add(1) == 1 {
			first <- bridge
			<-release // an HTTP request, say
		}
		return nil
	}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		begin(cs)
	}()
	var stale *WebRTCBridge
	select {
	case stale = <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("the first start never reached its cloud attempt")
	}

	within(t, time.Second, "the stop", cs.stopStream)
	if stale.errorHandler() != nil {
		t.Fatal("the stopped stream still listens to its abandoned bridge")
	}
	within(t, time.Second, "the restart", func() { begin(cs) })
	cs.mutex.RLock()
	current := cs.webrtcBridge
	cs.mutex.RUnlock()
	if current == stale || !cs.active.Load() {
		t.Fatalf("restart: active=%v, same bridge as the stopped attempt=%v; want a new session up",
			cs.active.Load(), current == stale)
	}

	close(release)
	receive(t, firstDone, "the stopped attempt never returned")
	cs.mutex.RLock()
	still := cs.webrtcBridge
	cs.mutex.RUnlock()
	if !cs.active.Load() || still != current {
		t.Fatalf("after the stopped attempt returned: active=%v, bridge replaced=%v; want the restart untouched",
			cs.active.Load(), still != current)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d cloud attempts, want one per start", n)
	}
}
