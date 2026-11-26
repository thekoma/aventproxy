package rtsp

import (
	"errors"
	"strings"
	"sync"

	"avent-webrtc-bridge/pkg/core"
)

// The local path: /<camera>/local streams over the LAN only. It never falls
// back to the cloud, so a viewer on it either gets the local stream or none,
// and a failure there leaves the camera's cool-down alone: the automatic path
// (/<camera>) keeps its own LAN and cloud choice.
//
// One LAN source at a time holds a camera's LAN session, whatever its path.
// The bridge then has one session per monitor and the integration its own:
// two, as on the automatic path alone. A monitor takes three (measured on an
// SCD923 and an SCD951, 24-11-2026), and the third is left free.

// localSuffix is the path segment that asks for the local path.
const localSuffix = "/local"

// errLANBusy is why a LAN attempt does not start: another stream of the same
// camera holds its LAN session.
var errLANBusy = errors.New("the camera's LAN session is in use by another stream")

// splitLocal takes a trailing /local off an RTSP path. A camera whose own
// path is /local keeps it: only a segment after the camera's counts.
func splitLocal(path string) (string, bool) {
	rest, ok := strings.CutSuffix(path, localSuffix)
	if !ok || rest == "" || rest == "/" {
		return path, false
	}
	return rest, true
}

// lanHolders gives one LAN source at a time a camera's LAN session. It is
// keyed by source, not by stream: a stream that stops and restarts while its
// old source is still ending (a promotion probe returns only once its attempt
// does) must not count that source's session as its own.
type lanHolders struct {
	mu      sync.Mutex
	holders map[string]*heldSource // device id -> the source holding its LAN session
}

func (r *lanHolders) acquire(deviceID string, h *heldSource) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.holders[deviceID]; ok {
		return false
	}
	if r.holders == nil {
		r.holders = map[string]*heldSource{}
	}
	r.holders[deviceID] = h
	return true
}

func (r *lanHolders) release(deviceID string, h *heldSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.holders[deviceID] == h {
		delete(r.holders, deviceID)
	}
}

// heldBy returns the stream whose source holds the camera's LAN session, or nil.
func (r *lanHolders) heldBy(deviceID string) *CameraStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h := r.holders[deviceID]; h != nil {
		return h.owner
	}
	return nil
}

// heldSource is a LAN source that gives the camera's LAN session back when it
// stops. Every path that ends a source stops it (a failed or stopped attempt,
// a teardown, a promotion probe), so none can keep the session.
type heldSource struct {
	lanStreamer
	owner   *CameraStream
	release func()
	once    sync.Once
}

// Stop gives the session back only once the source has closed it.
func (h *heldSource) Stop() {
	h.lanStreamer.Stop()
	h.once.Do(h.release)
}

// newHeldLAN builds a LAN source on forwarder if the camera's LAN session is
// free, and nil if a source holds it, this stream's own included. Callers
// hold cs.mutex.
func (cs *CameraStream) newHeldLAN(forwarder *RTPForwarder) lanStreamer {
	s, id := cs.server, cs.camera.DeviceID
	h := &heldSource{owner: cs}
	if !s.lanHolders.acquire(id, h) {
		return nil
	}
	h.release = func() { s.lanHolders.release(id, h) }
	h.lanStreamer = cs.lanFactory(forwarder)
	return h
}

// lanHeldByOther reports whether another stream holds the camera's LAN session.
func (cs *CameraStream) lanHeldByOther() bool {
	holder := cs.server.lanHolders.heldBy(cs.camera.DeviceID)
	return holder != nil && holder != cs
}

// freeLAN stops the stream holding the camera's LAN session if nobody watches
// it: a stream waits out its 30 s shutdown delay still connected to the
// camera, and would keep a viewer who just switched paths off the LAN.
// Called without cs.mutex.
func (cs *CameraStream) freeLAN() {
	holder := cs.server.lanHolders.heldBy(cs.camera.DeviceID)
	if holder == nil || holder == cs {
		return
	}
	holder.mutex.Lock()
	defer holder.mutex.Unlock()
	if len(holder.clients) == 0 && holder.pending.Load() == 0 {
		core.Logger.Info().Msgf("Stopping the unwatched stream of %s that holds its LAN session", cs.camera.DeviceName)
		holder.stopStreamInternal()
	}
}

// startLocal runs a local-path stream: one LAN attempt, and when it fails the
// clients are closed, so the request ends instead of waiting for media that
// will not come.
func (cs *CameraStream) startLocal(gen uint64) {
	if ok, _ := cs.lanEligible(); ok {
		cs.freeLAN()
	}
	cs.mutex.Lock()
	if cs.superseded(gen) {
		cs.mutex.Unlock()
		return
	}
	fail := func(reason string) {
		core.Logger.Warn().Msgf("Local stream of %s did not start: %s; the local path has no cloud fallback", cs.camera.DeviceName, reason)
		clients := cs.teardownLocked()
		cs.mutex.Unlock()
		closeClients(clients)
	}
	if ok, reason := cs.lanEligible(); !ok {
		fail(reason)
		return
	}
	source := cs.newHeldLAN(cs.forwarder)
	if source == nil {
		fail(errLANBusy.Error())
		return
	}
	cs.lan = source
	source.SetOnError(func(err error) { cs.lanDied(source, err) })
	stopped, err := cs.runLANAttempt(source)
	if stopped {
		cs.mutex.Unlock()
		return
	}
	if err != nil {
		cs.lan = nil
		source.Stop()
		fail(err.Error())
		return
	}
	cs.markActive()
	if len(cs.clients) == 0 { // the last client left during the attempt
		cs.scheduleShutdown()
	}
	cs.mutex.Unlock()
	core.Logger.Info().Msgf("Streaming %s with LAN signalling (local path)", cs.camera.DeviceName)
}
