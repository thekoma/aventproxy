package rtsp

import (
	"bufio"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"avent-webrtc-bridge/pkg/core"
)

// RTSP Digest authentication (RFC 2617, MD5, without qop). With a password
// configured (BridgeConfig.RTSPPassword) every request needs it: without one,
// anything that can reach the port, the host's own processes included, gets
// the video. Basic is refused, so the password never crosses the wire.
// Clients that pull the stream (ffmpeg in Home Assistant's stream worker and
// snapshots, go2rtc) send their first request without credentials, get the
// 401 and retry with them.

// RTSPUser is the user name the integration puts in the stream URL.
const RTSPUser = "homeassistant"

const digestRealm = "avent-webrtc-bridge"

// digestAuth checks requests against one user and password. The nonce is
// stateless and belongs to one connection: an HMAC of the connection's
// session id under a key made at start. A request answered on one
// connection cannot be replayed on another, and a client keeps its nonce for
// as long as its connection lives (go2rtc never takes a new one).
type digestAuth struct {
	user, password string
	key            []byte
}

func newDigestAuth(user, password string) *digestAuth {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(fmt.Sprintf("rtsp: no randomness for the digest nonce key: %v", err))
	}
	return &digestAuth{user: user, password: password, key: key}
}

// nonce is the nonce of the connection with this session id.
func (a *digestAuth) nonce(session string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(session))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// challenge is the WWW-Authenticate value of a 401 on that connection.
func (a *digestAuth) challenge(session string) string {
	return fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5`, digestRealm, a.nonce(session))
}

// check reports whether the request, on the connection with this session id,
// carries valid credentials.
func (a *digestAuth) check(session, method, requestURI, header string) bool {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(header), " ")
	if !strings.EqualFold(scheme, "Digest") {
		return false
	}
	p := parseDigestParams(rest)
	if alg := p["algorithm"]; alg != "" && !strings.EqualFold(alg, "MD5") {
		return false
	}
	// The challenge offers no qop, so a client answers without one.
	if p["qop"] != "" || p["realm"] != digestRealm || !sameRequestURI(p["uri"], requestURI) {
		return false
	}
	if !hmac.Equal([]byte(p["nonce"]), []byte(a.nonce(session))) {
		return false
	}
	ha1 := md5hex(a.user + ":" + digestRealm + ":" + a.password)
	ha2 := md5hex(method + ":" + p["uri"])
	want := md5hex(ha1 + ":" + p["nonce"] + ":" + ha2)
	userOK := subtle.ConstantTimeCompare([]byte(p["username"]), []byte(a.user)) == 1
	respOK := subtle.ConstantTimeCompare([]byte(strings.ToLower(p["response"])), []byte(want)) == 1
	return userOK && respOK
}

// sameRequestURI accepts the digest's uri when it names the request's URI,
// absolute or as its path: clients differ in which they put there.
func sameRequestURI(digestURI, requestURI string) bool {
	if digestURI == "" {
		return false
	}
	if digestURI == requestURI {
		return true
	}
	d, err1 := url.Parse(digestURI)
	r, err2 := url.Parse(requestURI)
	if err1 != nil || err2 != nil {
		return false
	}
	return d.EscapedPath() == r.EscapedPath() && (d.Host == "" || d.Host == r.Host)
}

// parseDigestParams reads the comma-separated key=value list of a Digest
// header; values may be quoted, and a quoted value may hold commas.
func parseDigestParams(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,")
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				break
			}
			val, s = s[1:end+1], s[end+2:]
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			val, s = strings.TrimSpace(s[:end]), s[end:]
		}
		out[key] = val
	}
	return out
}

// maxUnauthorized is how many requests a connection may send without valid
// credentials before its first authorized one; then it is closed. A client
// asks once without, then with them; one more is slack.
const maxUnauthorized = 3

// authDeadline bounds the time to the first authorized request; without a
// password the first request has requestReadTimeout, as before. A variable
// for the tests.
var authDeadline = 10 * time.Second

// errNoCredentials: a connection sent maxUnauthorized requests without valid
// credentials, which firstAuthorizedRequest has logged.
var errNoCredentials = errors.New("no valid credentials")

// header reads a request header regardless of case.
func (r *RTSPRequest) header(name string) string {
	if v, ok := r.Headers[name]; ok {
		return v
	}
	for k, v := range r.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// authorize reports whether the request may be served. Without a password
// every request may; otherwise a request without valid credentials gets a 401
// with a fresh challenge.
func (s *RTSPServer) authorize(conn net.Conn, session string, request *RTSPRequest) bool {
	if s.auth == nil {
		return true
	}
	if s.auth.check(session, request.Method, request.URL, request.header("Authorization")) {
		return true
	}
	headers := map[string]string{
		"CSeq":             strconv.Itoa(request.CSeq),
		"WWW-Authenticate": s.auth.challenge(session),
	}
	sendRTSPResponse(conn, 401, "Unauthorized", headers, "")
	return false
}

// firstAuthorizedRequest reads requests until one is authorized, answering
// the others with a challenge, within authDeadline and maxUnauthorized. The
// caller has set the read deadline for the first request.
func (s *RTSPServer) firstAuthorizedRequest(conn net.Conn, session string, reader *bufio.Reader) (*RTSPRequest, error) {
	if s.auth != nil {
		if err := conn.SetReadDeadline(time.Now().Add(authDeadline)); err != nil {
			return nil, err
		}
	}
	for refused := 0; ; refused++ {
		request, err := s.parseRTSPRequestFromReader(reader)
		if err != nil {
			return nil, err
		}
		if s.authorize(conn, session, request) {
			return request, nil
		}
		if refused+1 >= maxUnauthorized {
			core.Logger.Warn().Msgf("RTSP: closing a connection after %d requests without valid credentials", maxUnauthorized)
			return nil, errNoCredentials
		}
	}
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
