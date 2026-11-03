package rtsp

import (
	"context"
	"strconv"
	"time"

	"avent-webrtc-bridge/pkg/core"
)

// LAN-first selection, rebuilt from Leonard Pitzu's fork (pkg/rtsp/server.go
// tryLAN and connectCloud, commit 05b9372) with a first-frame deadline, a
// cancellable attempt and a per-camera cool-down.

const (
	// lanFirstFrameDeadline bounds a LAN attempt before the cloud takes over.
	lanFirstFrameDeadline = 4 * time.Second
	// lanCooldownPeriod keeps a camera whose LAN path failed on the cloud.
	lanCooldownPeriod = 5 * time.Minute
	// lanMinProtocol is the Tuya LAN protocol pkg/lan speaks. A monitor on 3.3
	// ignores its frames, so trying would only wait out the deadline.
	lanMinProtocol = 3.5
)

func (s *RTSPServer) lanCoolingDown(deviceID string) bool {
	s.cooldownMutex.Lock()
	defer s.cooldownMutex.Unlock()
	return time.Now().Before(s.lanCooldowns[deviceID])
}

func (s *RTSPServer) startLANCooldown(deviceID string) {
	s.cooldownMutex.Lock()
	defer s.cooldownMutex.Unlock()
	s.lanCooldowns[deviceID] = time.Now().Add(lanCooldownPeriod)
}

// lanEligible says whether this stream may try the LAN, and if not, why.
func (cs *CameraStream) lanEligible() (bool, string) {
	s := cs.server
	if s == nil || !s.LanStreaming {
		return false, "disabled"
	}
	id := cs.camera.DeviceID
	target := s.LANTargets[id]
	protocol, err := strconv.ParseFloat(target.Protocol, 64)
	switch {
	case s.Talkback:
		return false, "talkback on" // the LAN path carries no return audio
	case cs.resolution == "sd":
		return false, "sd stream"
	case target.LocalKey == "" || target.Password == "":
		return false, "no LAN credentials"
	case target.IP == "":
		return false, "no address"
	case target.Protocol == "":
		return false, "protocol unknown"
	case err != nil || !(protocol >= lanMinProtocol): // written so NaN fails too
		return false, "protocol " + target.Protocol
	case s.lanCoolingDown(id):
		return false, "cooling down"
	}
	return true, ""
}

// newLANSource is the default lanFactory. Callers hold cs.mutex.
func (cs *CameraStream) newLANSource(forwarder *RTPForwarder) lanStreamer {
	s := cs.server
	return NewLANSource(cs.camera.DeviceID, cs.camera.DeviceName, s.UID, s.LANTargets[cs.camera.DeviceID], forwarder)
}

// tryLAN streams over the LAN when the stream may. It reports done when the
// cloud must not start: the LAN took the stream, or the stream was stopped
// meanwhile. cs.mutex is released while the attempt waits for the first
// frame, so a Stop cancels the attempt instead of waiting for it.
func (cs *CameraStream) tryLAN(gen uint64) (done bool) {
	if ok, reason := cs.lanEligible(); !ok {
		core.Logger.Info().Msgf("Not using LAN signalling for %s: %s", cs.camera.DeviceName, reason)
		return false
	}

	cs.mutex.Lock()
	if cs.superseded(gen) {
		cs.mutex.Unlock()
		return true
	}
	// A cloud fallback needs a bridge that has not run yet.
	if cs.bridgeStarted {
		cs.replaceBridge()
	}
	// The stream's forwarder is the one RTSP clients attach to at SETUP.
	source := cs.lanFactory(cs.forwarder)
	// Set before Start, so a death between Start and the commit below reaches
	// lanDied, whose teardown cancels the attempt.
	cs.lan = source
	source.SetOnError(func(err error) { cs.lanDied(source, err) })
	stopped, err := cs.runLANAttempt(source)
	defer cs.mutex.Unlock()
	if stopped {
		return true
	}
	if err != nil {
		cs.lan = nil
		source.Stop()
		cs.forwarder.ResetMedia() // the cloud is a new source on the same clients
		cs.server.startLANCooldown(cs.camera.DeviceID)
		core.Logger.Warn().Msgf("Streaming with LAN signalling failed for %s: %v, trying cloud signalling", cs.camera.DeviceName, err)
		return false
	}
	cs.connecting.Store(false)
	cs.active.Store(true)
	if len(cs.clients) == 0 { // the last client left during the attempt
		cs.scheduleShutdown()
	}
	core.Logger.Info().Msgf("Streaming %s with LAN signalling", cs.camera.DeviceName)
	return true
}

// runLANAttempt starts src under the first-frame deadline without cs.mutex,
// so a Stop cancels the attempt instead of waiting for it. Called with
// cs.mutex held; returns with it held. stopped means the stream stopped
// meanwhile: src is stopped, and a restart may own attemptCancel and
// promoteTimer now, so the caller must leave the stream alone.
func (cs *CameraStream) runLANAttempt(src lanStreamer) (stopped bool, err error) {
	// Only stopStreamInternal cancels this while src starts.
	stoppedCtx, stop := context.WithCancel(context.Background())
	defer stop()
	cs.attemptCancel = stop
	cs.mutex.Unlock()

	ctx, cancel := context.WithTimeout(stoppedCtx, lanFirstFrameDeadline)
	err = src.Start(ctx)
	cancel()

	cs.mutex.Lock()
	if stoppedCtx.Err() != nil {
		src.Stop()
		return true, err
	}
	cs.attemptCancel = nil
	return false, err
}

// lanDied hands a LAN stream that died to the cloud: the camera cools down and
// the clients are closed, so they reconnect onto a cloud stream. A source the
// stream has already let go of is ignored.
func (cs *CameraStream) lanDied(source lanStreamer, err error) {
	cs.mutex.Lock()
	if cs.lan != source {
		cs.mutex.Unlock()
		return
	}
	cs.server.startLANCooldown(cs.camera.DeviceID)
	core.Logger.Warn().Msgf("Stream with LAN signalling for %s died: %v; using cloud signalling for %v", cs.camera.DeviceName, err, lanCooldownPeriod)
	clients := cs.teardownLocked()
	cs.mutex.Unlock()
	closeClients(clients)
}
