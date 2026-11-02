package lan

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
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

// Session is an authenticated LAN connection to one monitor.
type Session struct {
	DeviceID string
	Key      []byte // session key, derived during the handshake

	conn    net.Conn
	seq     atomic.Uint32
	readBuf []byte
	chunk   []byte // reused for every socket read

	writeMu sync.Mutex
}

// Dial opens a connection and completes the 3.5 session-key negotiation.
// ctx bounds both the dial and the negotiation; without a deadline the
// negotiation is capped at ioTimeout.
func Dial(ctx context.Context, ip, deviceID, localKey string) (*Session, error) {
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
	s := &Session{DeviceID: deviceID, conn: conn, Key: []byte(localKey)}
	if err := s.negotiate([]byte(localKey)); err != nil {
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
func (s *Session) negotiate(localKey []byte) error {
	localNonce := make([]byte, nonceLen)
	if _, err := rand.Read(localNonce); err != nil {
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
	// Frames from the monitor carry a retcode; the nonce sits behind it.
	payload = StripRetcode(payload)
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

	key, err := sessionKey(localKey, localNonce, remoteNonce)
	if err != nil {
		return err
	}
	s.Key = key
	return nil
}

// sessionKey mirrors the SDK: encrypt the XOR of both nonces under the
// localKey, in GCM with the local nonce as IV, and keep the ciphertext.
func sessionKey(localKey, localNonce, remoteNonce []byte) ([]byte, error) {
	mixed := make([]byte, nonceLen)
	for i := range mixed {
		mixed[i] = localNonce[i] ^ remoteNonce[i]
	}
	block, err := aes.NewCipher(localKey)
	if err != nil {
		return nil, err
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

// Send encrypts and writes a frame under the session key.
//
// iv must be an ASCII UUID fragment for command 32; see Pack6699.
func (s *Session) Send(cmd uint32, iv, payload []byte) error {
	return s.send(s.Key, cmd, iv, payload)
}

func (s *Session) sendWithKey(key []byte, cmd uint32, payload []byte) error {
	iv := make([]byte, gcmIVLen)
	if _, err := rand.Read(iv); err != nil {
		return err
	}
	return s.send(key, cmd, iv, payload)
}

func (s *Session) send(key []byte, cmd uint32, iv, payload []byte) error {
	frame, err := Pack6699(key, iv, s.seq.Add(1), cmd, payload)
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
			hdr, plain, err := Unpack6699(key, frame)
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

// Heartbeat pings the monitor so it does not drop an idle session.
func (s *Session) Heartbeat() error {
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
