package rtsp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"
)

// Issues #97 and #89: an RTSP client must get its answers while its camera's
// stream is still starting, whatever that start waits for.

// stallHarness is a real RTSP server on loopback with two cameras, /cam1 and
// /cam2, whose streams start through a fake cloud attempt (LAN streaming is
// off), and counts the streams it creates per device id.
type stallHarness struct {
	server *RTSPServer
	port   int

	mu      sync.Mutex
	created map[string]int
}

// newStallHarness starts the server. start is every cloud attempt of every
// stream; it runs where the real one does, with the stream's lock held.
func newStallHarness(t *testing.T, start func(cs *CameraStream) error) *stallHarness {
	t.Helper()
	t.Chdir(t.TempDir())
	sm, err := storage.NewStorageManager()
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if err := sm.SaveUser("eu", "viewer@example.com", nil); err != nil {
		t.Fatalf("save user: %v", err)
	}
	users, err := sm.ListUsers()
	if err != nil || len(users) != 1 {
		t.Fatalf("users = %v, %v", users, err)
	}
	key := users[0].UserKey
	if err := sm.UpdateCamerasForUser(key, []storage.CameraInfo{
		{UserKey: key, DeviceID: "cam1", DeviceName: "Stall Cam One", RTSPPath: "/cam1"},
		{UserKey: key, DeviceID: "cam2", DeviceName: "Stall Cam Two", RTSPPath: "/cam2"},
	}); err != nil {
		t.Fatalf("save cameras: %v", err)
	}

	h := &stallHarness{port: freePort(t), created: map[string]int{}}
	h.server = NewRTSPServer(h.port, sm)
	h.server.BindAddress = "127.0.0.1"
	h.server.newStream = func(cs *CameraStream) {
		h.mu.Lock()
		h.created[cs.camera.DeviceID]++
		h.mu.Unlock()
		cs.cloudStart = func() error { return start(cs) }
	}
	if err := h.server.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = h.server.Stop() })
	return h
}

func (h *stallHarness) streamsCreated(deviceID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.created[deviceID]
}

// stream returns the stream the server holds for a camera's HD path.
func (h *stallHarness) stream(deviceID string) *CameraStream {
	h.server.mutex.RLock()
	defer h.server.mutex.RUnlock()
	return h.server.streams[deviceID+"-hd"]
}

// shortRetry makes the two cloud attempts of a start follow each other at once.
func shortRetry(t *testing.T) {
	old := cloudRetryDelay
	cloudRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { cloudRetryDelay = old })
}

// rtspTestClient is one RTSP control connection to the harness.
type rtspTestClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	url  string
	cseq int
}

func dialRTSP(t *testing.T, port int, path string) *rtspTestClient {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &rtspTestClient{t: t, conn: conn, r: bufio.NewReader(conn), url: fmt.Sprintf("rtsp://127.0.0.1:%d%s", port, path)}
}

// do sends one request and returns the status code and headers of its answer,
// failing the test if none comes within wait.
func (c *rtspTestClient) do(method, url string, headers map[string]string, wait time.Duration) (int, map[string]string) {
	c.t.Helper()
	c.cseq++
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\nCSeq: %d\r\n", method, url, c.cseq)
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	sent := time.Now()
	if _, err := c.conn.Write([]byte(b.String())); err != nil {
		c.t.Fatalf("%s: write: %v", method, err)
	}

	_ = c.conn.SetReadDeadline(sent.Add(wait))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()
	status, err := c.r.ReadString('\n')
	if err != nil {
		c.t.Fatalf("%s %s: no answer within %v: %v", method, url, wait, err)
	}
	got := map[string]string{}
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("%s: header: %v", method, err)
		}
		if line == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			got[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	length, _ := strconv.Atoi(got["Content-Length"])
	if _, err := io.ReadFull(c.r, make([]byte, length)); err != nil {
		c.t.Fatalf("%s: body: %v", method, err)
	}
	fields := strings.Fields(status)
	if len(fields) < 2 {
		c.t.Fatalf("%s: bad status line %q", method, status)
	}
	code, _ := strconv.Atoi(fields[1])
	return code, got
}

func (c *rtspTestClient) describe(wait time.Duration) int {
	c.t.Helper()
	code, _ := c.do("DESCRIBE", c.url, map[string]string{"Accept": "application/sdp"}, wait)
	return code
}

// play runs DESCRIBE, SETUP of the video track over TCP and PLAY, each of
// which must be answered 200 within wait.
func (c *rtspTestClient) play(wait time.Duration) {
	c.t.Helper()
	if code := c.describe(wait); code != 200 {
		c.t.Fatalf("DESCRIBE = %d, want 200", code)
	}
	code, headers := c.do("SETUP", c.url+"/video", map[string]string{"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1"}, wait)
	if code != 200 {
		c.t.Fatalf("SETUP = %d, want 200", code)
	}
	session, _, _ := strings.Cut(headers["Session"], ";")
	if code, _ := c.do("PLAY", c.url, map[string]string{"Session": session}, wait); code != 200 {
		c.t.Fatalf("PLAY = %d, want 200", code)
	}
}

// expectClosedWithin checks that the server closed the connection.
func (c *rtspTestClient) expectClosedWithin(wait time.Duration) {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 512)
	for {
		_, err := c.r.Read(buf)
		if err == nil {
			continue // media or a late answer; keep reading until the close
		}
		if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
			return
		}
		c.t.Fatalf("connection still open after %v: %v", wait, err)
	}
}

// Issue #97: right after the bridge starts, Home Assistant opens the same
// camera twice ~0.4 s apart (the stream and a snapshot). Both clients must
// share one stream and one camera session, both must get their DESCRIBE
// answered while that session is still being set up, and a client of another
// camera must not wait for it either.
func TestTwoClientsOnAStartingCamera(t *testing.T) {
	shortRetry(t)
	release := make(chan struct{})
	var cam1Attempts atomic.Int32
	h := newStallHarness(t, func(cs *CameraStream) error {
		if cs.camera.DeviceID != "cam1" {
			return nil
		}
		cam1Attempts.Add(1)
		<-release // the camera has not answered yet
		return errors.New("test over")
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) }) // runs before the server stops

	first := dialRTSP(t, h.port, "/cam1")
	if code := first.describe(time.Second); code != 200 {
		t.Fatalf("first DESCRIBE = %d, want 200", code)
	}
	time.Sleep(400 * time.Millisecond)

	second := dialRTSP(t, h.port, "/cam1")
	if code := second.describe(time.Second); code != 200 {
		t.Fatalf("second DESCRIBE = %d, want 200", code)
	}
	other := dialRTSP(t, h.port, "/cam2")
	if code := other.describe(time.Second); code != 200 {
		t.Fatalf("other camera's DESCRIBE = %d, want 200", code)
	}

	if n := h.streamsCreated("cam1"); n != 1 {
		t.Fatalf("%d streams created for one camera, want 1", n)
	}
	if n := cam1Attempts.Load(); n != 1 {
		t.Fatalf("%d camera sessions started for one stream, want 1", n)
	}

	// Let the start fail its retry before the test restores the retry delay.
	once.Do(func() { close(release) })
	waitUntil(t, func() bool { return cam1Attempts.Load() == 2 }, "the start never retried")
}

// Issue #89: a start that fails must close every client that came for it,
// those that joined while it was failing too, and the next client must get a
// fresh start rather than a stream that is already dead.
func TestClientsOfAFailedStartReconnectToAFreshOne(t *testing.T) {
	shortRetry(t)
	fail := make(chan struct{})
	var attempts atomic.Int32
	h := newStallHarness(t, func(cs *CameraStream) error {
		switch attempts.Add(1) {
		case 1:
			<-fail // the camera never answers, until the test says so
			return errors.New("no answer")
		case 2:
			return errors.New("no answer either")
		default:
			return nil
		}
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(fail) }) })

	first := dialRTSP(t, h.port, "/cam1")
	first.play(time.Second)
	second := dialRTSP(t, h.port, "/cam1") // joins while the start hangs
	second.play(time.Second)

	once.Do(func() { close(fail) })
	first.expectClosedWithin(2 * time.Second)
	second.expectClosedWithin(2 * time.Second)

	third := dialRTSP(t, h.port, "/cam1")
	third.play(time.Second)
	waitUntil(t, func() bool {
		cs := h.stream("cam1")
		return cs != nil && cs.active.Load()
	}, "the client after the failure never got a working stream")
	if n := attempts.Load(); n != 3 {
		t.Fatalf("%d cloud attempts, want 2 for the failed start and 1 fresh one", n)
	}
}

// A client can go through its whole RTSP setup while its stream's lock is
// held, before it has joined the stream. A teardown in that window drops it
// from the forwarder but cannot close a client the stream does not know yet:
// the client must be closed when it comes to join, not left playing a stream
// that will never send it anything.
func TestClientJoiningAfterATeardownIsClosed(t *testing.T) {
	var attempts atomic.Int32
	h := newStallHarness(t, func(cs *CameraStream) error {
		attempts.Add(1)
		return nil
	})
	first := dialRTSP(t, h.port, "/cam1")
	first.play(time.Second)
	waitUntil(t, func() bool {
		cs := h.stream("cam1")
		return cs != nil && cs.active.Load()
	}, "the stream never started")
	cs := h.stream("cam1")

	cs.mutex.Lock() // a long locked section, e.g. a teardown stopping a bridge
	locked := true
	defer func() {
		if locked {
			cs.mutex.Unlock()
		}
	}()
	second := dialRTSP(t, h.port, "/cam1")
	second.play(time.Second)
	clients := cs.teardownLocked()
	locked = false
	cs.mutex.Unlock()
	closeClients(clients)

	first.expectClosedWithin(2 * time.Second)
	second.expectClosedWithin(2 * time.Second)
	if n := attempts.Load(); n != 1 {
		t.Fatalf("%d cloud attempts; the late client must not restart the stream it never joined", n)
	}
}

// shortVideoWatch makes a cloud session that carries no video fail after d.
func shortVideoWatch(t *testing.T, d time.Duration) {
	old := cloudVideoTimeout
	cloudVideoTimeout = d
	t.Cleanup(func() { cloudVideoTimeout = old })
}

// silentStream is a stream with one client whose cloud attempts succeed,
// carrying video only if video is set.
func silentStream(t *testing.T, video *atomic.Bool) (*CameraStream, net.Conn) {
	t.Helper()
	_, cs := newTestCameraStream(t)
	cs.camera.DeviceName = fmt.Sprintf("Silent Cam %d", time.Now().UnixNano())
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})
	cs.clients["s1"] = &RTSPClient{conn: serverConn, session: "s1", stream: cs}
	cs.cloudStart = func() error {
		if video.Load() {
			cs.webrtcBridge.videoPackets.Add(1)
		}
		return nil
	}
	t.Cleanup(cs.stopStream)
	return cs, clientConn
}

// Issue #89: a cloud session can come up (ICE connected, or the HEVC
// datachannel confirmed its codec) and then carry no video at all. Its
// clients would wait for good, and every later one would join the dead
// stream: the session must count as failed, its clients closed, so the next
// one starts afresh.
func TestCloudSessionWithoutVideoCountsAsFailed(t *testing.T) {
	shortVideoWatch(t, 100*time.Millisecond)
	cs, clientConn := silentStream(t, new(atomic.Bool))

	begin(cs)
	if active, _, _ := streamState(cs); !active {
		t.Fatal("the cloud attempt did not bring the stream up")
	}
	expectClosed(t, clientConn)
	if active, connecting, _ := streamState(cs); active || connecting {
		t.Fatalf("active=%v connecting=%v, want the silent session torn down", active, connecting)
	}
	for _, line := range testLog.lines(cs.camera.DeviceName) {
		if strings.Contains(line, `"level":"warn"`) && strings.Contains(line, "no video") {
			return
		}
	}
	t.Fatalf("no warning naming the missing video in %q", testLog.lines(cs.camera.DeviceName))
}

func TestCloudSessionWithVideoIsKept(t *testing.T) {
	shortVideoWatch(t, 50*time.Millisecond)
	video := new(atomic.Bool)
	video.Store(true)
	cs, clientConn := silentStream(t, video)

	begin(cs)
	time.Sleep(200 * time.Millisecond)
	if active, _, _ := streamState(cs); !active {
		t.Fatal("a session carrying video was torn down")
	}
	expectOpen(t, clientConn)
}

// The check belongs to the session it was armed for: a restart in between
// gets its own.
func TestVideoCheckOfAStoppedSessionLeavesTheRestartAlone(t *testing.T) {
	shortVideoWatch(t, 100*time.Millisecond)
	video := new(atomic.Bool)
	cs, clientConn := silentStream(t, video)

	begin(cs) // silent
	cs.stopStream()
	video.Store(true)
	begin(cs) // carries video
	time.Sleep(300 * time.Millisecond)
	if active, _, _ := streamState(cs); !active {
		t.Fatal("the first session's check tore down the restarted one")
	}
	expectOpen(t, clientConn)
}
