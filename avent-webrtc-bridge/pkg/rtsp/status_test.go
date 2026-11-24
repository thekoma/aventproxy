package rtsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/lan"
	"avent-webrtc-bridge/pkg/storage"
)

func TestStatusFollowsTheStream(t *testing.T) {
	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	fakeAttempts(t, cs, nil)
	ids := []string{"testdev", "other"}

	if got := server.status.snapshot(ids); got[0].Path != PathIdle || got[0].Since != nil || got[1].Path != PathIdle {
		t.Fatalf("before any stream: %+v", got)
	}
	begin(cs)
	got := server.status.snapshot(ids)
	if got[0].Path != PathLocal || got[0].Since == nil || got[0].LastFallback != "" {
		t.Fatalf("LAN stream running: %+v", got[0])
	}
	if got[1].Path != PathIdle {
		t.Fatalf("a camera without a stream: %+v", got[1])
	}

	cs.stopStream()
	if got = server.status.snapshot(ids); got[0].Path != PathIdle || got[0].Since == nil {
		t.Fatalf("after stop: %+v", got[0])
	}
}

func TestStatusNamesTheFallback(t *testing.T) {
	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	fakeAttempts(t, cs, func(context.Context) error {
		return fmt.Errorf("%w: %w (lan: start ended: context canceled (read tcp 192.0.2.50:41744->192.0.2.10:6668: use of closed network connection))",
			errNoFirstFrame, context.DeadlineExceeded)
	})
	begin(cs)
	got := server.status.snapshot([]string{"testdev"})[0]
	if got.Path != PathWebRTC {
		t.Fatalf("path = %q, want webrtc", got.Path)
	}
	if got.LastFallback != "local stream did not start (no video), cloud signalling" || got.LastFallbackAt == nil {
		t.Fatalf("fallback = %q at %v", got.LastFallback, got.LastFallbackAt)
	}
	cs.stopStream()
}

func TestStatusNamesALANStreamThatDied(t *testing.T) {
	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	a := fakeAttempts(t, cs, nil)
	begin(cs)
	a.lan.die(fmt.Errorf("lan: Test Cam: %w", lan.ErrMediaIdle))
	got := server.status.snapshot([]string{"testdev"})[0]
	if got.Path != PathIdle || got.LastFallback != "local stream died (no media), cloud signalling" {
		t.Fatalf("after the LAN stream died: %+v", got)
	}
}

// Why a camera never tried the LAN shows too, except for the reasons that are
// the design (LAN streaming off, the sd stream; talkback in session mode and
// the cool-down are in TestStatusKeepsTheCauseThroughTheCooldown).
func TestStatusNamesWhyTheLANWasNotTried(t *testing.T) {
	server := newLANServer()
	server.LANTargets["testdev"] = LANTarget{LocalKey: "lk", Password: "pw", IP: "192.0.2.10"}
	cs, _ := newLANStream(t, server, "hd")
	fakeAttempts(t, cs, nil)
	begin(cs)
	got := server.status.snapshot([]string{"testdev"})[0]
	if got.Path != PathWebRTC || got.LastFallback != "local stream not tried (protocol unknown), cloud signalling" {
		t.Fatalf("no published protocol: %+v", got)
	}
	cs.stopStream()

	for _, setup := range []func(){
		func() { server.LanStreaming = false },
		func() { server.LanStreaming = true },
	} {
		server = newLANServer()
		setup()
		resolution := "hd"
		if server.LanStreaming {
			resolution = "sd"
		}
		cs, _ = newLANStream(t, server, resolution)
		fakeAttempts(t, cs, nil)
		begin(cs)
		if got := server.status.snapshot([]string{"testdev"})[0]; got.LastFallback != "" {
			t.Fatalf("lan_streaming %v, %s stream: fallback %q, want none", server.LanStreaming, resolution, got.LastFallback)
		}
		cs.stopStream()
	}
}

func TestStatusNamesTheIMMFallback(t *testing.T) {
	server := newIMMServer(CloudTransportIMM)
	cs, _ := newLANStream(t, server, "hd")
	a := fakeIMMAttempts(t, cs, nil, func(context.Context) error { return lan.ErrNoAnswer })
	begin(cs)
	got := server.status.snapshot([]string{"testdev"})[0]
	if a.cloudStarted.Load() != 1 || got.Path != PathWebRTC || got.LastFallback != "imm did not start (no answer), webrtc cloud signalling" {
		t.Fatalf("imm without an answer: cloud %d, %+v", a.cloudStarted.Load(), got)
	}
	cs.stopStream()
}

// With two streams of one camera, the one a client watches is reported, and
// among unwatched ones the local one. Each case is checked with the streams
// registered in both orders and either one the older.
func TestStatusPrefersTheWatchedStream(t *testing.T) {
	camera := &storage.CameraInfo{DeviceID: "testdev", DeviceName: "Test Cam"}
	user := &storage.UserSession{UserKey: "user1"}
	for _, localFirst := range []bool{true, false} {
		for _, localOlder := range []bool{true, false} {
			server := NewRTSPServer(0, nil)
			local := NewCameraStream(camera, "hd", user, nil, server)
			cloud := NewCameraStream(camera, "sd", user, nil, server)
			older, newer := time.Now().Add(-time.Minute), time.Now()
			if !localOlder {
				older, newer = newer, older
			}
			board := server.status.camera("testdev")
			if localFirst {
				board.active[local] = activeStream{path: PathLocal, since: older}
				board.active[cloud] = activeStream{path: PathWebRTC, since: newer}
			} else {
				board.active[cloud] = activeStream{path: PathWebRTC, since: newer}
				board.active[local] = activeStream{path: PathLocal, since: older}
			}
			if got := server.status.snapshot([]string{"testdev"})[0]; got.Path != PathLocal {
				t.Fatalf("nobody watching (local older %v): path = %q, want local", localOlder, got.Path)
			}
			clientConn, serverConn := net.Pipe()
			if err := cloud.forwarder.AddTCPClient("c1", serverConn, 0, 1, 255); err != nil {
				t.Fatal(err)
			}
			if got := server.status.snapshot([]string{"testdev"})[0]; got.Path != PathWebRTC {
				t.Fatalf("sd watched (local older %v): path = %q, want webrtc", localOlder, got.Path)
			}
			cloud.forwarder.RemoveClient("c1")
			clientConn.Close()
			serverConn.Close()
		}
	}
}

func TestStatusEndpoint(t *testing.T) {
	server, _ := newTestServer(t, "127.0.0.1")
	if err := server.storageManager.UpdateCamerasForUser("u", []storage.CameraInfo{{DeviceID: "cam1", UserKey: "u"}}); err != nil {
		t.Fatal(err)
	}
	h := server.statusHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var doc Status
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || rec.Code != 200 {
		t.Fatalf("GET /status: %d %v %s", rec.Code, err, rec.Body)
	}
	if len(doc.Cameras) != 1 || doc.Cameras[0].CameraID != "cam1" || doc.Cameras[0].Path != PathIdle {
		t.Fatalf("document %+v", doc)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/status", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /status = %d", method, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /config = %d, want 404", rec.Code)
	}
}

// The server serves /status on StatusPort at its bind address and stops it
// with itself; without a StatusPort nothing listens.
func TestStatusServedNextToRTSP(t *testing.T) {
	server, _ := newTestServer(t, "127.0.0.1")
	server.StatusPort = freePort(t)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(server.StatusPort)) + "/status"
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"cameras"`) {
		t.Fatalf("GET /status: %d %s", resp.StatusCode, body)
	}
	if err := server.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(url); err == nil {
		t.Fatal("/status still answers after Stop")
	}
}

// A failure's reason names the kind of failure, never the address the
// error's text carries.
func TestFallbackCauseCarriesNoAddress(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp4", Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 7), Port: 6668}, Err: syscall.ECONNREFUSED}
	for err, want := range map[error]string{
		fmt.Errorf("lan: dial 192.0.2.7: %w", dial):                                                        "connection refused",
		fmt.Errorf("%w: %w (lan: dial 192.0.2.7: i/o timeout)", errNoFirstFrame, context.DeadlineExceeded): "no video",
		fmt.Errorf("lan: negotiation with 192.0.2.7: %w", context.DeadlineExceeded):                        "timed out",
		fmt.Errorf("lan: 192.0.2.7: %w", lan.ErrMediaIdle):                                                 "no media",
		fmt.Errorf("lan: neg response: read tcp 192.0.2.7:6668: %w", io.EOF):                               "connection closed",
		fmt.Errorf("lan: dial 192.0.2.7: %w", syscall.EHOSTUNREACH):                                        "unreachable",
		fmt.Errorf("lan: dial 192.0.2.7: %w", syscall.ENETUNREACH):                                         "unreachable",
		errors.New("lan: negotiation HMAC mismatch, wrong localKey at 192.0.2.7"):                          "error",
	} {
		got := fallbackCause(err)
		if got != want {
			t.Errorf("fallbackCause(%v) = %q, want %q", err, got, want)
		}
		if strings.Contains(got, "192.0.2") {
			t.Errorf("address in %q", got)
		}
	}
}

// A start during the cool-down that a failure began, or with talkback in
// session mode, keeps the real cause in the status.
func TestStatusKeepsTheCauseThroughTheCooldown(t *testing.T) {
	server := newLANServer()
	cs, _ := newLANStream(t, server, "hd")
	fakeAttempts(t, cs, func(context.Context) error {
		return fmt.Errorf("%w: %w", errNoFirstFrame, context.DeadlineExceeded)
	})
	begin(cs)
	cs.stopStream()
	waitRemoved(t, server, cs)
	want := "local stream did not start (no video), cloud signalling"
	if got := server.status.snapshot([]string{"testdev"})[0].LastFallback; got != want {
		t.Fatalf("after the failure: %q", got)
	}

	next, _ := newLANStream(t, server, "hd")
	fakeAttempts(t, next, nil)
	begin(next) // cooling down: the LAN is skipped
	if got := server.status.snapshot([]string{"testdev"})[0]; got.Path != PathWebRTC || got.LastFallback != want {
		t.Fatalf("second start in the cool-down: %+v, want the first cause kept", got)
	}
	next.stopStream()
	waitRemoved(t, server, next)

	server.Talkback = true
	server.TalkbackMode = TalkbackSession
	server.lanWorked("testdev")
	third, _ := newLANStream(t, server, "hd")
	fakeAttempts(t, third, nil)
	begin(third)
	if got := server.status.snapshot([]string{"testdev"})[0].LastFallback; got != want {
		t.Fatalf("talkback in session mode: %q, want the first cause kept", got)
	}
	third.stopStream()
}

func TestStatusNamesAFailedPromotionProbe(t *testing.T) {
	probed := make(chan struct{}, 1)
	cs, _, _ := cloudStreamCoolingDown(t, 100*time.Millisecond, func(context.Context) error {
		signal(probed)
		return fmt.Errorf("lan: dial 192.0.2.10: %w", syscall.ECONNREFUSED)
	})
	receive(t, probed, "no LAN probe once the cool-down ended")
	waitUntil(t, func() bool { return promoteTimer(cs) != nil }, "no new promotion after the failed probe")
	got := cs.server.status.snapshot([]string{"testdev"})[0].LastFallback
	if got != "local stream check failed (connection refused), staying on cloud signalling" {
		t.Fatalf("fallback %q", got)
	}
}

func TestStatusNamesAnIMMStreamThatDied(t *testing.T) {
	server := newIMMServer(CloudTransportIMM)
	cs, _ := newLANStream(t, server, "hd")
	a := fakeIMMAttempts(t, cs, nil, nil)
	begin(cs)
	if got := server.status.snapshot([]string{"testdev"})[0]; got.Path != PathIMM {
		t.Fatalf("imm stream running: %+v", got)
	}
	a.imm.die(fmt.Errorf("lan: Test Cam stopped streaming: %w", lan.ErrEnded))
	got := server.status.snapshot([]string{"testdev"})[0]
	if got.Path != PathIdle || got.LastFallback != "imm died (ended by the monitor), webrtc cloud signalling" {
		t.Fatalf("after the imm stream died: %+v", got)
	}
}

// The status server closes idle keep-alive connections and slow requests.
func TestStatusServerHasTimeouts(t *testing.T) {
	server, _ := newTestServer(t, "127.0.0.1")
	server.StatusPort = freePort(t)
	server.startStatus([]string{"127.0.0.1"})
	defer server.stopStatus()
	if len(server.statusServers) != 1 {
		t.Fatalf("%d status servers", len(server.statusServers))
	}
	srv := server.statusServers[0]
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("timeouts %v %v %v %v", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
}

// waitRemoved waits until a stopped stream's removal from the server's map
// has run (stopStreamInternal does it in a goroutine), so the next stream the
// test registers does not race it.
func waitRemoved(t *testing.T, server *RTSPServer, cs *CameraStream) {
	t.Helper()
	waitUntil(t, func() bool {
		server.mutex.RLock()
		defer server.mutex.RUnlock()
		_, ok := server.streams[cs.streamId]
		return !ok
	}, "the stopped stream is still in the server's map")
}

// Two alike streams that started at the same instant resolve by stream id,
// whatever the map's order.
func TestStatusTieBreaksByStreamID(t *testing.T) {
	camera := &storage.CameraInfo{DeviceID: "testdev", DeviceName: "Test Cam"}
	user := &storage.UserSession{UserKey: "user1"}
	at := time.Now()
	for i := 0; i < 20; i++ {
		server := NewRTSPServer(0, nil)
		hd := NewCameraStream(camera, "hd", user, nil, server)
		sd := NewCameraStream(camera, "sd", user, nil, server)
		board := server.status.camera("testdev")
		board.active[sd] = activeStream{path: PathWebRTC, since: at}
		board.active[hd] = activeStream{path: PathIMM, since: at}
		if got := server.status.snapshot([]string{"testdev"})[0]; got.Path != PathIMM {
			t.Fatalf("run %d: path %q, want the hd stream's (the smaller id)", i, got.Path)
		}
	}
}

// Among alike streams the newest wins, even when the older has the smaller
// stream id; and the id only decides between alike streams: a local one wins
// over a WebRTC one that started at the same instant.
func TestStatusNewestThenClassBeforeID(t *testing.T) {
	camera := &storage.CameraInfo{DeviceID: "testdev", DeviceName: "Test Cam"}
	user := &storage.UserSession{UserKey: "user1"}
	now := time.Now()
	for i := 0; i < 20; i++ {
		server := NewRTSPServer(0, nil)
		hd := NewCameraStream(camera, "hd", user, nil, server)
		sd := NewCameraStream(camera, "sd", user, nil, server)
		board := server.status.camera("testdev")
		board.active[hd] = activeStream{path: PathIMM, since: now.Add(-time.Minute)}
		board.active[sd] = activeStream{path: PathWebRTC, since: now}
		if got := server.status.snapshot([]string{"testdev"})[0]; got.Path != PathWebRTC {
			t.Fatalf("run %d, alike streams: path %q, want the newer (webrtc)", i, got.Path)
		}

		server = NewRTSPServer(0, nil)
		hd = NewCameraStream(camera, "hd", user, nil, server)
		sd = NewCameraStream(camera, "sd", user, nil, server)
		board = server.status.camera("testdev")
		board.active[hd] = activeStream{path: PathWebRTC, since: now}
		board.active[sd] = activeStream{path: PathLocal, since: now}
		if got := server.status.snapshot([]string{"testdev"})[0]; got.Path != PathLocal {
			t.Fatalf("run %d, same instant, different classes: path %q, want local", i, got.Path)
		}
	}
}
