package rtsp

import (
	"errors"
	"fmt"
	"time"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/lan"
)

// The cloud transports, the Dev card's cloud_transport option.
const (
	// CloudTransportWebRTC is the webrtc-mode session every card has used:
	// the default, and what imm falls back to.
	CloudTransportWebRTC = "webrtc"
	// CloudTransportIMM tries an imm session over cloud signalling first.
	CloudTransportIMM = "imm"
)

// ParseCloudTransport checks a cloud_transport setting; empty means webrtc.
func ParseCloudTransport(v string) (string, error) {
	switch v {
	case "", CloudTransportWebRTC:
		return CloudTransportWebRTC, nil
	case CloudTransportIMM:
		return CloudTransportIMM, nil
	}
	return "", fmt.Errorf("cloud transport %q: want %s or %s", v, CloudTransportWebRTC, CloudTransportIMM)
}

// immCooldownPeriod keeps a camera whose imm session failed on webrtc.
const immCooldownPeriod = lanCooldownPeriod

func (s *RTSPServer) immCoolingDown(deviceID string) bool {
	s.cooldownMutex.Lock()
	defer s.cooldownMutex.Unlock()
	return time.Now().Before(s.immCooldowns[deviceID])
}

func (s *RTSPServer) startIMMCooldown(deviceID string) {
	s.cooldownMutex.Lock()
	defer s.cooldownMutex.Unlock()
	if s.immCooldowns == nil {
		s.immCooldowns = make(map[string]time.Time)
	}
	s.immCooldowns[deviceID] = time.Now().Add(immCooldownPeriod)
}

// immEligible says whether this stream may try imm over cloud signalling. An
// empty reason means the option is off, which is not worth a log line.
func (cs *CameraStream) immEligible() (bool, string) {
	s := cs.server
	if s == nil || s.CloudTransport != CloudTransportIMM {
		return false, ""
	}
	switch {
	case s.Talkback:
		return false, "talkback on" // the imm path carries no return audio
	case cs.resolution == "sd":
		return false, "sd stream" // the start sequence asks for the main stream
	case s.immCoolingDown(cs.camera.DeviceID):
		return false, "cooling down"
	}
	return true, ""
}

// tryIMM streams with imm over cloud signalling when the stream may. Like
// tryLAN, it reports done when the webrtc session must not start, and holds no
// lock while the attempt waits.
func (cs *CameraStream) tryIMM(gen uint64) (done bool) {
	ok, reason := cs.immEligible()
	if !ok {
		if reason != "" {
			core.Logger.Info().Msgf("Not using imm over cloud signalling for %s: %s", cs.camera.DeviceName, reason)
		}
		return false
	}

	cs.mutex.Lock()
	if cs.superseded(gen) {
		cs.mutex.Unlock()
		return true
	}
	if cs.bridgeStarted {
		cs.replaceBridge()
	}
	factory := cs.immFactory
	if factory == nil {
		factory = cs.newIMMSource
	}
	source := factory(cs.forwarder)
	cs.lan = source // the stream's non-webrtc source; Stop and teardown end it
	source.SetOnError(func(err error) { cs.immDied(source, err) })
	stopped, err := cs.runAttempt(source, immAttemptDeadline)
	defer cs.mutex.Unlock()
	if stopped {
		return true
	}
	if err != nil {
		cs.lan = nil
		source.Stop()
		cs.forwarder.ResetMedia()
		cs.server.startIMMCooldown(cs.camera.DeviceID)
		core.Logger.Warn().Msgf("imm over cloud signalling failed for %s at %s: %v; using webrtc cloud signalling, imm skipped for %v",
			cs.camera.DeviceName, immStep(err), err, immCooldownPeriod)
		return false
	}
	cs.markActive()
	if len(cs.clients) == 0 { // the last client left during the attempt
		cs.scheduleShutdown()
	}
	core.Logger.Info().Msgf("Streaming %s with imm over cloud signalling", cs.camera.DeviceName)
	return true
}

// immDied hands an imm stream that died to the webrtc session, as lanDied
// does for the LAN.
func (cs *CameraStream) immDied(source lanStreamer, err error) {
	cs.mutex.Lock()
	if cs.lan != source {
		cs.mutex.Unlock()
		return
	}
	cs.server.startIMMCooldown(cs.camera.DeviceID)
	core.Logger.Warn().Msgf("Stream with imm over cloud signalling for %s died: %v; using webrtc cloud signalling for %v",
		cs.camera.DeviceName, err, immCooldownPeriod)
	clients := cs.teardownLocked()
	cs.mutex.Unlock()
	closeClients(clients)
}

// immStep names the step an imm attempt failed at, for the one log line the
// maintainer reads first.
func immStep(err error) string {
	steps := []struct {
		err  error
		name string
	}{
		{errIMMCloud, "cloud setup"},
		{lan.ErrOfferNotSent, "offer"},
		{lan.ErrRefused, "refused"},
		{lan.ErrNoAnswer, "no answer"},
		{lan.ErrBadAnswer, "answer unusable"},
		{lan.ErrNoCandidate, "answer without usable candidate"},
		{lan.ErrICE, "ICE timeout"},
		{lan.ErrLogin, "login not sent"},
		{errIMMSilent, "login refused or one-way path"},
		{errIMMNoMedia, "no media after login"},
		{errIMMNoVideo, "no video"},
		{errIMMNoKeyframe, "no SPS/IDR"},
		{lan.ErrMediaIdle, "no media after login"},
		{lan.ErrEnded, "ended by the monitor"},
	}
	for _, s := range steps {
		if errors.Is(err, s.err) {
			return s.name
		}
	}
	if errors.Is(err, errNoFirstFrame) {
		return "no SPS/IDR"
	}
	return "start"
}
