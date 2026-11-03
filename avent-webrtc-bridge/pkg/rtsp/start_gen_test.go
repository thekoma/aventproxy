package rtsp

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// A start that waits out the retry delay can wake into a stream that was
// stopped and restarted meanwhile. It must leave the newer start alone: here
// the newer start is on the LAN, and the stale one used to bring the cloud up
// beside it, two sources feeding one forwarder.
func TestStaleStartLeavesTheNewerOneAlone(t *testing.T) {
	old := cloudRetryDelay
	cloudRetryDelay = 100 * time.Millisecond
	defer func() { cloudRetryDelay = old }()

	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	t.Cleanup(cs.stopStream)

	var lanCalls atomic.Int32
	releaseLAN := make(chan struct{})
	a := fakeAttempts(t, cs, func(ctx context.Context) error {
		if lanCalls.Add(1) == 1 {
			return errors.New("first start: LAN down")
		}
		<-releaseLAN // the newer start holds its LAN attempt open
		return nil
	})
	firstCloudFailed := make(chan struct{})
	var cloudCalls atomic.Int32
	cs.cloudStart = func() error {
		a.cloudStarted.Add(1)
		if cloudCalls.Add(1) == 1 {
			close(firstCloudFailed)
			return errors.New("first start: cloud down")
		}
		return nil
	}

	go begin(cs) // the first start fails both paths' first try and sleeps
	<-firstCloudFailed
	time.Sleep(10 * time.Millisecond)
	cs.stopStream() // its clients leave, the stream stops...
	server.cooldownMutex.Lock()
	delete(server.lanCooldowns, "testdev")
	server.cooldownMutex.Unlock()
	go begin(cs) // ...and a client restarts it, on the LAN this time

	time.Sleep(3 * cloudRetryDelay) // the first start wakes meanwhile
	close(releaseLAN)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if active, _, lan := streamState(cs); active && lan != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the newer start never took the LAN")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := a.cloudStarted.Load(); n != 1 {
		t.Fatalf("cloud started %d times; the stale start must not try again beside the LAN", n)
	}
}

// The cleanup a stop schedules runs later, on its own goroutine. A stream a
// client was just handed, or one a client restarted meanwhile, must stay in
// the map: dropped, it would keep streaming where nothing finds or stops it.
func TestRemoveStreamKeepsAStreamInUse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(*RTSPServer, *CameraStream)
		after  func(*CameraStream)
	}{
		{"handed to a client just before the stop", func(s *RTSPServer, cs *CameraStream) {
			if got, _ := s.getOrCreateStream(cs.camera, cs.resolution, cs.user); got != cs {
				t.Fatal("getOrCreateStream did not reuse the live stream")
			}
		}, func(*CameraStream) {}},
		{"restarted after the stop", func(*RTSPServer, *CameraStream) {}, func(cs *CameraStream) {
			cs.mutex.Lock()
			cs.claimStart()
			cs.mutex.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, cs := newTestCameraStream(t)
			cs.active.Store(true)
			tc.before(server, cs)
			cs.mutex.Lock()
			cs.stopStreamInternal()
			cs.mutex.Unlock()
			tc.after(cs)

			server.removeStream(cs) // what the scheduled cleanup does
			server.mutex.RLock()
			got := server.streams[cs.streamId]
			server.mutex.RUnlock()
			if got != cs {
				t.Fatal("the stream in use was dropped from the server map")
			}
		})
	}
}

// Stop and the inactivity sweep read the client map and lastActivity, which
// clients connecting and leaving write under the stream's lock (run with -race).
func TestStopAndSweepWhileClientsComeAndGo(t *testing.T) {
	server, cs := newTestCameraStream(t)
	cs.startStreamOverride = func() {}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 100 {
			id := fmt.Sprint(i)
			cs.AddClient(&RTSPClient{session: id, stream: cs})
			cs.RemoveClient(id)
		}
	}()
	for range 100 {
		cs.Stop()
		server.cleanupInactiveStreams()
	}
	<-done
}
