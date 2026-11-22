package lan

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Port is the Tuya LAN control channel.
	Port = 6668

	// HeartbeatInterval keeps the socket alive: the monitor closes an idle
	// connection after roughly 30-45 seconds.
	HeartbeatInterval = 8 * time.Second

	ioTimeout = 10 * time.Second
	nonceLen  = 16
)

// dialPort is a variable so tests can point Dial at a local listener.
var dialPort = Port

// nonceSource supplies the negotiation's nonce; tests pin it.
var nonceSource io.Reader = rand.Reader

// Version is a Tuya LAN protocol version a session speaks.
type Version int

const (
	// V33 has no key negotiation, so a connection proves nothing; Dial asks
	// for the data points first and keeps the session only when the answer
	// decrypts under the monitor's key.
	V33 Version = 33
	V34 Version = 34
	V35 Version = 35
)

func (v Version) String() string {
	return fmt.Sprintf("%d.%d", int(v)/10, int(v)%10)
}

// ParseVersion reads a protocol version as the integration publishes it, such
// as "3.4". Anything this package does not speak is an error.
func ParseVersion(s string) (Version, error) {
	switch strings.TrimSpace(s) {
	case "3.3":
		return V33, nil
	case "3.4":
		return V34, nil
	case "3.5":
		return V35, nil
	}
	return 0, fmt.Errorf("lan: unsupported protocol version %q", s)
}

// Session is an authenticated LAN connection to one monitor.
type Session struct {
	DeviceID string
	Key      []byte // session key, derived during the handshake
	// Version is the protocol the session speaks; zero is 3.5.
	Version Version

	conn    net.Conn
	seq     atomic.Uint32
	readBuf []byte
	chunk   []byte // reused for every socket read

	writeMu sync.Mutex
}

// Dial opens a connection and completes the session-key negotiation for
// version, 3.5 when it is zero; on 3.3, which has none, it proves the monitor
// instead (prove33). ctx bounds both the dial and the handshake; without a
// deadline the handshake is capped at ioTimeout.
func Dial(ctx context.Context, ip, deviceID, localKey string, version Version) (*Session, error) {
	if version == 0 {
		version = V35
	}
	if version != V33 && version != V34 && version != V35 {
		return nil, fmt.Errorf("lan: unsupported protocol version %s", version)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(dialPort)))
	if err != nil {
		return nil, fmt.Errorf("lan: dial %s: %w", ip, err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(ioTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, err
	}
	// Cancellation without a deadline must also unblock the negotiation reads.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	s := &Session{DeviceID: deviceID, conn: conn, Key: []byte(localKey), Version: version}
	handshake := s.negotiate
	if version == V33 {
		handshake = s.prove33
	}
	if err := handshake([]byte(localKey)); err != nil {
		stop()
		conn.Close()
		// A cancelled ctx closed the conn under the read: say so, rather than
		// leave only the closed-connection error it caused.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("lan: negotiation with %s: %w (%v)", ip, ctxErr, err)
		}
		return nil, err
	}
	if !stop() {
		return nil, ctx.Err() // ctx ended and closed the conn just as we finished
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

// negotiate performs SESS_KEY_NEG start/response/finish and installs the key.
// 3.4 and 3.5 run the same exchange; they differ in framing and in how the key
// is derived from the two nonces.
func (s *Session) negotiate(localKey []byte) error {
	localNonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(nonceSource, localNonce); err != nil {
		return err
	}

	if err := s.sendWithKey(localKey, CmdSessKeyNegStart, localNonce); err != nil {
		return fmt.Errorf("lan: neg start: %w", err)
	}

	h, payload, err := s.recvWithKey(localKey)
	if err != nil {
		return fmt.Errorf("lan: neg response: %w", err)
	}
	if h.Cmd != CmdSessKeyNegResp {
		return fmt.Errorf("lan: expected cmd %d, got %d", CmdSessKeyNegResp, h.Cmd)
	}
	if s.Version != V34 {
		// 6699 frames from the monitor carry a retcode inside the encryption;
		// the nonce sits behind it. 3.4 keeps the retcode outside, and
		// Unpack55AA34 has already taken it off.
		payload = StripRetcode(payload)
	}
	if len(payload) < nonceLen+sha256.Size {
		return errors.New("lan: negotiation response too short")
	}
	remoteNonce := payload[:nonceLen]

	want := hmacSHA256(localKey, localNonce)
	if !hmac.Equal(want, payload[nonceLen:nonceLen+sha256.Size]) {
		return errors.New("lan: negotiation HMAC mismatch, wrong localKey")
	}

	if err := s.sendWithKey(localKey, CmdSessKeyNegFinish, hmacSHA256(localKey, remoteNonce)); err != nil {
		return fmt.Errorf("lan: neg finish: %w", err)
	}

	key, err := sessionKey(s.Version, localKey, localNonce, remoteNonce)
	if err != nil {
		return err
	}
	s.Key = key
	return nil
}

// prove33 stands in for the negotiation 3.3 lacks. Anyone can complete a 3.3
// connection, so before anything else the session asks for the data points
// (command 10, as tinytuya does) and accepts only an answer that decrypts
// under localKey into JSON and names no other device. Two unreadable answers,
// or a foreign devId, mean another device at that address, which must never
// get the offer.
//
// A monitor with a 22-character id may answer command 10 with the text "json
// obj data unvalid" instead of JSON (tinytuya's "device22", localtuya's "type
// 0d"): an SCD973-family camera did, 11 ms after the query, 02-11-2026. That
// text decrypted under localKey, which no other device's key yields, so it
// proves the monitor as well as JSON would.
func (s *Session) prove33(localKey []byte) error {
	query, err := json.Marshal(map[string]string{
		"gwId": s.DeviceID, "devId": s.DeviceID, "uid": s.DeviceID,
		"t": strconv.FormatInt(time.Now().Unix(), 10),
	})
	if err != nil {
		return err
	}
	if err := s.send(localKey, CmdDPQuery33, nil, query); err != nil {
		return fmt.Errorf("lan: 3.3 status query: %w", err)
	}
	unreadable := 0
	for {
		_, payload, err := s.recvWithKey(localKey)
		if errors.Is(err, errBadCRC) {
			payload = []byte{0} // damaged: no proof either way, but counted
		} else if err != nil {
			return fmt.Errorf("lan: 3.3 status answer: %w", err)
		}
		if len(payload) == 0 {
			continue // an acknowledgement: no proof either way
		}
		if bytes.Contains(payload, []byte("data unvalid")) {
			return nil
		}
		var doc map[string]any
		if json.Unmarshal(bytes.TrimSpace(payload), &doc) != nil {
			if unreadable++; unreadable >= 2 {
				return errors.New("lan: 3.3 answers do not decrypt under the localKey: another device, or a stale key")
			}
			continue
		}
		if id, ok := doc["devId"].(string); ok && s.DeviceID != "" && id != s.DeviceID {
			return errors.New("lan: the 3.3 answer names another device")
		}
		return nil
	}
}

// sessionKey mirrors the SDK and tinytuya: encrypt the XOR of both nonces
// under the localKey and keep 16 bytes of the result. 3.4 uses one AES-ECB
// block; 3.5 uses GCM with the local nonce as IV.
func sessionKey(v Version, localKey, localNonce, remoteNonce []byte) ([]byte, error) {
	mixed := make([]byte, nonceLen)
	for i := range mixed {
		mixed[i] = localNonce[i] ^ remoteNonce[i]
	}
	block, err := aes.NewCipher(localKey)
	if err != nil {
		return nil, err
	}
	if v == V34 {
		out := make([]byte, nonceLen)
		block.Encrypt(out, mixed)
		return out, nil
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, gcmIVLen)
	if err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, localNonce[:gcmIVLen], mixed, nil)
	return sealed[:nonceLen], nil
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// Send encrypts and writes a frame under the session key, which on 3.3 is the
// localKey.
//
// iv is used by 3.5 only and must be an ASCII UUID fragment for command 32;
// see Pack6699. 3.3 and 3.4 frames carry no IV.
func (s *Session) Send(cmd uint32, iv, payload []byte) error {
	return s.send(s.Key, cmd, iv, payload)
}

func (s *Session) sendWithKey(key []byte, cmd uint32, payload []byte) error {
	var iv []byte
	if s.Version != V33 && s.Version != V34 {
		iv = make([]byte, gcmIVLen)
		if _, err := rand.Read(iv); err != nil {
			return err
		}
	}
	return s.send(key, cmd, iv, payload)
}

func (s *Session) send(key []byte, cmd uint32, iv, payload []byte) error {
	var frame []byte
	var err error
	switch s.Version {
	case V33:
		frame, err = Pack55AA33(key, s.seq.Add(1), cmd, payload, !noVersionHeader33[cmd])
	case V34:
		body := append(append([]byte{}, versionHeader34For(cmd)...), payload...)
		frame, err = Pack55AA34(key, s.seq.Add(1), cmd, body)
	default:
		frame, err = Pack6699(key, iv, s.seq.Add(1), cmd, payload)
	}
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(ioTimeout)); err != nil {
		return err
	}
	_, err = s.conn.Write(frame)
	return err
}

// Recv reads the next frame, decrypted under the session key.
func (s *Session) Recv(timeout time.Duration) (Header, []byte, error) {
	if err := s.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return Header{}, nil, err
	}
	return s.recvWithKey(s.Key)
}

func (s *Session) recvWithKey(key []byte) (Header, []byte, error) {
	for {
		// Serve a complete frame out of the buffer before reading more.
		if h, err := ParseHeader(s.readBuf); err == nil && len(s.readBuf) >= h.Total {
			frame := s.readBuf[:h.Total]
			s.readBuf = s.readBuf[h.Total:]
			unpack := Unpack6699
			switch s.Version {
			case V33:
				unpack = Unpack55AA33
			case V34:
				unpack = Unpack55AA34
			}
			hdr, plain, err := unpack(key, frame)
			if err != nil {
				return hdr, nil, err
			}
			return hdr, plain, nil
		} else if errors.Is(err, ErrBadPrefix) || errors.Is(err, ErrBadLength) {
			// Resynchronise on the next plausible prefix rather than giving up.
			// A bad length must resync too: waiting for that frame to complete
			// would grow the buffer without bound.
			if idx := nextPrefix(s.readBuf); idx > 0 {
				s.readBuf = s.readBuf[idx:]
				continue
			}
			s.readBuf = nil
		}

		if s.chunk == nil {
			s.chunk = make([]byte, maxFrameLen)
		}
		n, err := s.conn.Read(s.chunk)
		if n > 0 {
			s.readBuf = append(s.readBuf, s.chunk[:n]...)
		}
		if err != nil {
			return Header{}, nil, err
		}
	}
}

func nextPrefix(buf []byte) int {
	for i := 1; i+4 <= len(buf); i++ {
		switch binary.BigEndian.Uint32(buf[i : i+4]) {
		case Prefix6699, Prefix55AA:
			return i
		}
	}
	return -1
}

// Heartbeat pings the monitor so it does not drop an idle session. On 3.3 the
// body is "{}", as tinytuya sends it.
func (s *Session) Heartbeat() error {
	if s.Version == V33 {
		return s.sendWithKey(s.Key, CmdHeartbeat, []byte("{}"))
	}
	return s.sendWithKey(s.Key, CmdHeartbeat, nil)
}

// Drain discards whatever the monitor sends until the connection fails, so
// replies and pushes nobody reads cannot fill the socket buffer. It returns
// io.EOF when the monitor closes the connection.
func (s *Session) Drain() error {
	if err := s.conn.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, s.conn); err != nil {
		return err
	}
	return io.EOF
}

// Close shuts the connection down.
func (s *Session) Close() error {
	if s.conn == nil {
		return nil
	}
	return s.conn.Close()
}
