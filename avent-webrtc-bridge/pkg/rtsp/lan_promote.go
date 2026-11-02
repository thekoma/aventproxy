package rtsp

import (
	"errors"
	"time"

	"avent-webrtc-bridge/pkg/core"
)

// Back to the LAN after a fallback. A consumer that never disconnects
// (Frigate, Scrypted, an always-on go2rtc) would otherwise stay on the cloud
// for good after one LAN failure, and a freshly booted monitor ignores local
// offers until it has reached Tuya, so every power cut ends there.

// errLANAvailable is why a cloud stream is torn down once the LAN works again.
var errLANAvailable = errors.New("LAN available again")

// armPromotion probes the LAN when the camera's cool-down ends, if the
// cool-down is all that keeps this stream on the cloud. Callers hold cs.mutex.
func (cs *CameraStream) armPromotion() {
	if ok, reason := cs.lanEligible(); !ok && reason != "cooling down" {
		return
	}
	s := cs.server
	s.cooldownMutex.Lock()
	delay := time.Until(s.lanCooldowns[cs.camera.DeviceID])
	s.cooldownMutex.Unlock()

	if cs.promoteTimer != nil {
		cs.promoteTimer.Stop()
	}
	core.Logger.Info().Msgf("Trying the LAN again for %s in %v", cs.camera.DeviceName, delay.Round(time.Second))
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		// Taken before timer is read: armPromotion's caller holds it while timer is set.
		cs.mutex.Lock()
		if cs.promoteTimer != timer { // the stream stopped, or re-armed, since it fired
			cs.mutex.Unlock()
			return
		}
		cs.promoteTimer = nil
		cs.promote()
	})
	cs.promoteTimer = timer
}

// promote runs a throwaway LAN source to see whether the LAN works again,
// without the lock while it waits. When it does, the cloud stream is torn
// down and its clients closed, so they reconnect onto the LAN; when it does
// not, the cool-down starts over. Called with cs.mutex held; releases it.
func (cs *CameraStream) promote() {
	if ok, _ := cs.lanEligible(); !ok { // the cool-down was extended meanwhile
		cs.armPromotion()
		cs.mutex.Unlock()
		return
	}
	// A forwarder with no clients: the cloud stream's viewers never see the probe.
	probe := cs.lanFactory(NewRTPForwarder())
	stopped, err := cs.runLANAttempt(probe)
	if stopped {
		cs.mutex.Unlock()
		return
	}
	probe.Stop() // before the clients reconnect, so the monitor is free for them
	if err != nil {
		cs.server.startLANCooldown(cs.camera.DeviceID)
		core.Logger.Info().Msgf("LAN still unavailable for %s: %v", cs.camera.DeviceName, err)
		cs.armPromotion()
		cs.mutex.Unlock()
		return
	}
	core.Logger.Info().Msgf("%v for %s, moving viewers", errLANAvailable, cs.camera.DeviceName)
	clients := cs.teardownLocked()
	cs.mutex.Unlock()
	closeClients(clients)
}
