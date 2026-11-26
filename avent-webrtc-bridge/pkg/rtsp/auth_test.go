package rtsp

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"
)

const testRTSPPassword = "a-password-for-the-test-0123456789"

// digestHeader answers a challenge the way ffmpeg and go2rtc do (no qop), or
// with qop=auth when cnonce is set, which the bridge does not offer.
func digestHeader(user, password, method, uri, nonce, cnonce string) string {
	ha1 := md5hex(user + ":" + digestRealm + ":" + password)
	ha2 := md5hex(method + ":" + uri)
	if cnonce == "" {
		resp := md5hex(ha1 + ":" + nonce + ":" + ha2)
		return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`, user, digestRealm, nonce, uri, resp)
	}
	resp := md5hex(ha1 + ":" + nonce + ":00000001:" + cnonce + ":auth:" + ha2)
	return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=auth, nc=00000001, cnonce="%s", response="%s", algorithm=MD5`,
		user, digestRealm, nonce, uri, cnonce, resp)
}

func TestDigestAcceptsTheRightAnswer(t *testing.T) {
	a := newDigestAuth(RTSPUser, testRTSPPassword)
	uri := "rtsp://bridge:38554/Cam"
	h := digestHeader(RTSPUser, testRTSPPassword, "DESCRIBE", uri, a.nonce("s1"), "")
	if !a.check("s1", "DESCRIBE", uri, h) {
		t.Fatal("the right answer was refused")
	}
	// A path-only uri for an absolute request URI, as some clients send.
	h = digestHeader(RTSPUser, testRTSPPassword, "OPTIONS", "/Cam", a.nonce("s1"), "")
	if !a.check("s1", "OPTIONS", uri, h) {
		t.Fatal("a path-only uri was refused")
	}
	if !strings.Contains(a.challenge("s1"), `nonce="`+a.nonce("s1")+`"`) || strings.Contains(a.challenge("s1"), "qop") {
		t.Fatalf("challenge %q", a.challenge("s1"))
	}
}

func TestDigestRefusesEverythingElse(t *testing.T) {
	a := newDigestAuth(RTSPUser, testRTSPPassword)
	other := newDigestAuth(RTSPUser, testRTSPPassword)
	uri := "rtsp://bridge:38554/Cam"
	n := a.nonce("s1")
	for name, h := range map[string]string{
		"none":           "",
		"basic":          "Basic " + base64.StdEncoding.EncodeToString([]byte(RTSPUser+":"+testRTSPPassword)),
		"wrong password": digestHeader(RTSPUser, "guess", "DESCRIBE", uri, n, ""),
		"wrong user":     digestHeader("admin", testRTSPPassword, "DESCRIBE", uri, n, ""),
		"other method":   digestHeader(RTSPUser, testRTSPPassword, "OPTIONS", uri, n, ""),
		"other uri":      digestHeader(RTSPUser, testRTSPPassword, "DESCRIBE", "rtsp://bridge:38554/Other", n, ""),
		"made-up nonce":  digestHeader(RTSPUser, testRTSPPassword, "DESCRIBE", uri, "00112233445566778899aabbccddeeff", ""),
		"foreign nonce":  digestHeader(RTSPUser, testRTSPPassword, "DESCRIBE", uri, other.nonce("s1"), ""),
		// A request answered on one connection does not open another.
		"other connection": digestHeader(RTSPUser, testRTSPPassword, "DESCRIBE", uri, a.nonce("s2"), ""),
		"qop not offered":  digestHeader(RTSPUser, testRTSPPassword, "DESCRIBE", uri, n, "") + `, qop=auth, nc=00000001, cnonce="0a4f113b"`,
	} {
		if a.check("s1", "DESCRIBE", uri, h) {
			t.Errorf("%s: accepted", name)
		}
	}
}

// rtspExchange writes one request on c and reads the response head.
func rtspExchange(t *testing.T, c net.Conn, r *bufio.Reader, req string) (int, map[string]string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	// An empty write on a net.Pipe blocks; "" only reads the next response.
	if req != "" {
		if _, err := io.WriteString(c, req); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	var code int
	fmt.Sscanf(line, "RTSP/1.0 %d", &code)
	headers := map[string]string{}
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			return code, headers
		}
		k, v, _ := strings.Cut(l, ":")
		headers[k] = strings.TrimSpace(v)
	}
}

func authServer() *RTSPServer {
	s := NewRTSPServer(0, nil)
	s.RTSPPassword = testRTSPPassword
	s.auth = newDigestAuth(RTSPUser, testRTSPPassword)
	return s
}

// Without the password a connection gets challenges, never a camera lookup
// or a stream (the server has no storage: a lookup would panic), and is
// closed after maxUnauthorized requests.
func TestUnauthenticatedConnectionStartsNothing(t *testing.T) {
	s := authServer()
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() { s.handleConnection(server); close(done) }()
	r := bufio.NewReader(client)
	for i := 1; i <= maxUnauthorized; i++ {
		code, h := rtspExchange(t, client, r, fmt.Sprintf("OPTIONS rtsp://bridge:38554/Cam RTSP/1.0\r\nCSeq: %d\r\n\r\n", i))
		if code != 401 || !strings.HasPrefix(h["WWW-Authenticate"], "Digest ") || h["CSeq"] != fmt.Sprint(i) {
			t.Fatalf("request %d: %d %v", i, code, h)
		}
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the connection stayed open after the refused requests")
	}
	if len(s.streams) != 0 || len(s.clients) != 0 {
		t.Fatalf("an unauthenticated connection made %d streams, %d clients", len(s.streams), len(s.clients))
	}
}

// One connection end to end: the nonce of its first 401 opens it and serves
// its later requests too (the first request and the later ones are checked
// on two paths), and a later request without credentials gets a 401.
func TestTheConnectionsNonceServesItsLaterRequests(t *testing.T) {
	s, _ := newTestServer(t, "127.0.0.1")
	s.RTSPPassword = testRTSPPassword
	s.auth = newDigestAuth(RTSPUser, testRTSPPassword)
	if err := s.storageManager.SaveUser("eu", "lab@example.com", nil); err != nil {
		t.Fatal(err)
	}
	users, _ := s.storageManager.ListUsers()
	key := users[0].UserKey
	if err := s.storageManager.UpdateCamerasForUser(key, []storage.CameraInfo{{UserKey: key, DeviceID: "d", DeviceName: "Cam", RTSPPath: "/Cam"}}); err != nil {
		t.Fatal(err)
	}
	s.newStream = func(cs *CameraStream) {
		cs.cloudStart = func(context.Context, *WebRTCBridge) error { return nil }
	}
	server, client := net.Pipe()
	defer client.Close()
	go s.handleConnection(server)
	r := bufio.NewReader(client)
	uri := "rtsp://bridge:38554/Cam"
	options := func(cseq int, auth string) int {
		req := fmt.Sprintf("OPTIONS %s RTSP/1.0\r\nCSeq: %d\r\n", uri, cseq)
		if auth != "" {
			req += "Authorization: " + auth + "\r\n"
		}
		code, _ := rtspExchange(t, client, r, req+"\r\n")
		return code
	}
	_, h := rtspExchange(t, client, r, "OPTIONS "+uri+" RTSP/1.0\r\nCSeq: 1\r\n\r\n")
	nonce := parseDigestParams(strings.TrimPrefix(h["WWW-Authenticate"], "Digest "))["nonce"]
	auth := digestHeader(RTSPUser, testRTSPPassword, "OPTIONS", uri, nonce, "")
	for i, want := range []int{200, 200, 200} {
		if code := options(i+2, auth); code != want {
			t.Fatalf("authorized OPTIONS %d: %d, want %d", i+1, code, want)
		}
	}
	if code := options(9, ""); code != 401 {
		t.Fatalf("a later OPTIONS without credentials: %d, want 401", code)
	}
}

// With the right answer the request goes on to the path check: an empty path
// is a 400, not a 401.
func TestAuthenticatedRequestIsServed(t *testing.T) {
	s := authServer()
	server, client := net.Pipe()
	go s.handleConnection(server)
	defer client.Close()
	r := bufio.NewReader(client)
	uri := "rtsp://bridge:38554/"
	code, h := rtspExchange(t, client, r, "OPTIONS "+uri+" RTSP/1.0\r\nCSeq: 1\r\n\r\n")
	if code != 401 {
		t.Fatalf("first request %d", code)
	}
	nonce := parseDigestParams(strings.TrimPrefix(h["WWW-Authenticate"], "Digest "))["nonce"]
	auth := digestHeader(RTSPUser, testRTSPPassword, "OPTIONS", uri, nonce, "")
	code, _ = rtspExchange(t, client, r, "OPTIONS "+uri+" RTSP/1.0\r\nCSeq: 2\r\nAuthorization: "+auth+"\r\n\r\n")
	if code != 400 {
		t.Fatalf("authorized request answered %d, want 400 for the empty path", code)
	}
}

// A connection that sends nothing is closed after authDeadline, not the
// longer requestReadTimeout.
func TestSilentConnectionClosedAtTheAuthDeadline(t *testing.T) {
	old := authDeadline
	authDeadline = 100 * time.Millisecond
	defer func() { authDeadline = old }()
	s := authServer()
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() { s.handleConnection(server); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a silent connection stayed open past the auth deadline")
	}
}

// Without a password the server answers as before, unauthenticated.
func TestNoPasswordNoAuthentication(t *testing.T) {
	s := NewRTSPServer(0, nil)
	if !s.authorize(nil, "s1", &RTSPRequest{Method: "OPTIONS", URL: "rtsp://x/Cam"}) {
		t.Fatal("a server without a password refused a request")
	}
}

// On a connection past its first request, every later request needs the
// password too: one without gets a 401 and is not served, and the
// connection stays open for the retry.
func TestLaterRequestsNeedThePasswordToo(t *testing.T) {
	s := authServer()
	server, peer := net.Pipe()
	defer peer.Close()
	stream := NewCameraStream(&storage.CameraInfo{DeviceID: "d", DeviceName: "Cam"}, "hd", nil, nil, s)
	client := &RTSPClient{conn: server, reader: bufio.NewReader(server), session: "s1", stream: stream}
	done := make(chan struct{})
	go func() { s.handleRTSPProtocol(client); close(done) }()
	r := bufio.NewReader(peer)
	for i := 1; i <= 2; i++ {
		code, h := rtspExchange(t, peer, r, fmt.Sprintf("DESCRIBE rtsp://bridge:38554/Cam RTSP/1.0\r\nCSeq: %d\r\n\r\n", i))
		if code != 401 || h["CSeq"] != fmt.Sprint(i) || !strings.HasPrefix(h["WWW-Authenticate"], "Digest ") {
			t.Fatalf("request %d without credentials: %d %v", i, code, h)
		}
	}
	// With the connection's nonce the next request is served: OPTIONS gets
	// its 200.
	nonce := s.auth.nonce("s1")
	auth := digestHeader(RTSPUser, testRTSPPassword, "OPTIONS", "rtsp://bridge:38554/Cam", nonce, "")
	code, _ := rtspExchange(t, peer, r, "OPTIONS rtsp://bridge:38554/Cam RTSP/1.0\r\nCSeq: 3\r\nAuthorization: "+auth+"\r\n\r\n")
	if code != 200 {
		t.Fatalf("authorized OPTIONS: %d", code)
	}
	peer.Close()
	<-done
}

// With a token, /status answers only a request that carries it; without one
// it is open, as before.
func TestStatusNeedsTheTokenWhenSet(t *testing.T) {
	const token = "a-token-for-the-test-0123456789"
	s := NewRTSPServer(0, nil)
	s.StatusToken = token
	h := s.statusHandler()
	for name, header := range map[string]string{
		"none": "", "wrong": "Bearer nope", "basic": "Basic " + token, "no scheme": token,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with the token: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	NewRTSPServer(0, nil).statusHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("without a token configured: %d", rec.Code)
	}
}

// Start makes the digest checker from RTSPPassword, and none without it.
func TestStartUsesThePassword(t *testing.T) {
	for _, password := range []string{"", testRTSPPassword} {
		s, _ := newTestServer(t, "127.0.0.1")
		s.RTSPPassword = password
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		got := s.auth != nil
		s.Stop()
		if got != (password != "") {
			t.Errorf("password %q: auth %v", password, got)
		}
	}
}
