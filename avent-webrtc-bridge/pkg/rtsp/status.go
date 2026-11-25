package rtsp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/lan"
)

// The bridge's status for Home Assistant: per camera, which path its stream
// takes now (local, imm, webrtc or idle), since when, and the last time it
// left the LAN path and why. Without it Home Assistant cannot tell whether a
// monitor streams over the LAN. Served read-only as JSON on StatusPort, on the
// same addresses as RTSP: open like RTSP itself without a password, and only
// for StatusToken when the streams have one. It carries no keys, passwords or
// addresses.

const (
	PathLocal  = "local"  // LAN signalling, media over the LAN
	PathIMM    = "imm"    // imm over cloud signalling
	PathWebRTC = "webrtc" // the webrtc cloud session
	PathIdle   = "idle"
)

// CameraStatus is one camera in the status document.
type CameraStatus struct {
	CameraID       string     `json:"camera_id"`
	Path           string     `json:"path"`
	Since          *time.Time `json:"since,omitempty"`
	LastFallback   string     `json:"last_fallback,omitempty"`
	LastFallbackAt *time.Time `json:"last_fallback_at,omitempty"`
}

// Status is the document /status serves.
type Status struct {
	Cameras []CameraStatus `json:"cameras"`
}

type activeStream struct {
	path  string
	since time.Time
}

type cameraBoard struct {
	active         map[*CameraStream]activeStream
	idleSince      time.Time
	lastFallback   string
	lastFallbackAt time.Time
}

// statusBoard records what the streams do. Streams call it while holding
// their own mutex, so it never takes a stream's mutex itself.
type statusBoard struct {
	mu   sync.Mutex
	cams map[string]*cameraBoard
}

func (b *statusBoard) camera(deviceID string) *cameraBoard {
	if b.cams == nil {
		b.cams = map[string]*cameraBoard{}
	}
	c, ok := b.cams[deviceID]
	if !ok {
		c = &cameraBoard{active: map[*CameraStream]activeStream{}}
		b.cams[deviceID] = c
	}
	return c
}

func (b *statusBoard) streamActive(cs *CameraStream, path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.camera(cs.camera.DeviceID).active[cs] = activeStream{path: path, since: time.Now()}
}

func (b *statusBoard) streamStopped(cs *CameraStream) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.camera(cs.camera.DeviceID)
	if _, ok := c.active[cs]; !ok {
		return
	}
	delete(c.active, cs)
	if len(c.active) == 0 {
		c.idleSince = time.Now()
	}
}

func (b *statusBoard) fallback(deviceID, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.camera(deviceID)
	c.lastFallback = reason
	c.lastFallbackAt = time.Now()
}

// snapshot reports every camera in deviceIDs. A camera can have two streams
// at once (hd and sd, or one waiting out its shutdown delay): it reports one a
// client watches before one nobody watches; among those alike, a local stream
// (the one that holds a LAN session), else the newest; with none active, idle.
func (b *statusBoard) snapshot(deviceIDs []string) []CameraStatus {
	type candidate struct {
		cs     *CameraStream
		stream activeStream
	}
	type pick struct {
		candidates []candidate
		status     CameraStatus
	}
	b.mu.Lock()
	picks := make([]pick, 0, len(deviceIDs))
	for _, id := range deviceIDs {
		p := pick{status: CameraStatus{CameraID: id, Path: PathIdle}}
		if c, ok := b.cams[id]; ok {
			for cs, a := range c.active {
				p.candidates = append(p.candidates, candidate{cs, a})
			}
			if len(c.active) == 0 && !c.idleSince.IsZero() {
				since := c.idleSince
				p.status.Since = &since
			}
			if c.lastFallback != "" {
				at := c.lastFallbackAt
				p.status.LastFallback, p.status.LastFallbackAt = c.lastFallback, &at
			}
		}
		picks = append(picks, p)
	}
	b.mu.Unlock()

	// Client counts come from the forwarders' own locks, outside b.mu and
	// without the stream lock, which a start holds for seconds.
	out := make([]CameraStatus, 0, len(picks))
	for _, p := range picks {
		var best activeStream
		var bestID string
		found, bestWatched := false, false
		for _, c := range p.candidates {
			watched := c.cs.forwarder != nil && c.cs.forwarder.GetClientCount() > 0
			local, bestLocal := c.stream.path == PathLocal, best.path == PathLocal
			alike := watched == bestWatched && local == bestLocal
			// Among alike streams the newest; two that started at the same
			// instant by stream id, so the map's order never decides.
			better := !found ||
				(watched && !bestWatched) ||
				(watched == bestWatched && local && !bestLocal) ||
				(alike && c.stream.since.After(best.since)) ||
				(alike && c.stream.since.Equal(best.since) && c.cs.streamId < bestID)
			if better {
				found, best, bestWatched, bestID = true, c.stream, watched, c.cs.streamId
			}
		}
		if found {
			since := best.since
			p.status.Path, p.status.Since = best.path, &since
		}
		out = append(out, p.status)
	}
	return out
}

// fallbackCause names what went wrong in a failed LAN start or a LAN stream
// that died, without the error's own text: that can carry the camera's
// address ("dial tcp4 <address>:6668"), which /status, and the sensor's
// history in Home Assistant, must not.
func fallbackCause(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, errNoFirstFrame): // the attempt ran out of time waiting for video
		return "no video"
	case errors.Is(err, lan.ErrMediaIdle):
		return "no media"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "unreachable"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET):
		return "connection closed"
	}
	return "error"
}

// noteFallback records why a stream left, or never took, the path it tries first.
func (cs *CameraStream) noteFallback(reason string) {
	if cs.server != nil {
		cs.server.status.fallback(cs.camera.DeviceID, reason)
	}
}

// statusDocument is what /status answers.
func (s *RTSPServer) statusDocument() Status {
	var ids []string
	if s.storageManager != nil {
		if cameras, err := s.storageManager.GetAllCameras(); err == nil {
			for _, c := range cameras {
				ids = append(ids, c.DeviceID)
			}
		}
	}
	sort.Strings(ids)
	return Status{Cameras: s.status.snapshot(ids)}
}

func (s *RTSPServer) statusHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// With a password on the streams, the status is for the integration
		// only: it names each camera's device id and when someone watches it.
		if s.StatusToken != "" && !bearerTokenOK(r, s.StatusToken) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.statusDocument())
	})
	return mux
}

// bearerTokenOK reports whether the request carries "Authorization: Bearer
// <token>", compared in constant time.
func bearerTokenOK(r *http.Request, token string) bool {
	scheme, got, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	return ok && strings.EqualFold(scheme, "Bearer") &&
		subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(token)) == 1
}

// startStatus serves /status on StatusPort at every RTSP bind host. A host it
// cannot bind is logged and skipped: the status is a diagnostic, never a
// reason for the bridge not to stream.
func (s *RTSPServer) startStatus(hosts []string) {
	if s.StatusPort <= 0 {
		return
	}
	handler := s.statusHandler()
	for _, host := range hosts {
		addr := net.JoinHostPort(host, strconv.Itoa(s.StatusPort))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			core.Logger.Warn().Msgf("Status: cannot listen on %s: %v", addr, err)
			continue
		}
		srv := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       5 * time.Second,
			WriteTimeout:      5 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		s.statusServers = append(s.statusServers, srv)
		core.Logger.Info().Msgf("Status: serving /status on %s", ln.Addr())
		go func() { _ = srv.Serve(ln) }()
	}
}

func (s *RTSPServer) stopStatus() {
	for _, srv := range s.statusServers {
		_ = srv.Close()
	}
	s.statusServers = nil
}
