package lan

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// startTimeout caps the handshake when the caller's context does not.
	startTimeout = 20 * time.Second
	// mediaIdleTimeout is how long every media conversation may stay silent
	// before the stream counts as dead; audio alone arrives about 50 times a
	// second.
	mediaIdleTimeout = 3 * time.Second
)

// DefaultStartupGap is the longest pause between two messages of the startup
// sequence when ClientConfig.StartupGap is zero: the 120 ms this package slept
// after every message, which came from Leonard Pitzu's fork without a reason
// given. The monitor answers each setup message on the control conversation
// (never the login), so the next one goes out as soon as that answer is in.
// On an SCD951 (protocol 3.4, 22-11-2026) the first two waited the whole
// 120 ms and the others 20-40 ms. A fixed 20 ms pause brought sessions up in
// 0.2-0.4 s but stays unproven: once in five starts an SCD951 then sent no
// video, which also happened once with 400 ms waits (2026-11-04).
const DefaultStartupGap = 120 * time.Millisecond

// ErrMediaIdle is a stream on which no conversation delivered a readable media
// message for mediaIdleTimeout: after the login on a start, or at any time on a
// live stream.
var ErrMediaIdle = errors.New("no media")

// ClientConfig names the monitor and where its media goes. Any callback may
// be nil, in which case that stream is discarded. Callbacks run on the
// client's own goroutines: media on the conversation's reader, OnClosed on
// whichever loop failed.
type ClientConfig struct {
	DeviceID string
	LocalKey string
	Password string
	UID      string
	// IP is the monitor's address. The integration discovers it; the bridge
	// never does.
	IP string
	// Version is the protocol the monitor speaks on 6668, as the integration's
	// own session found it; zero is 3.5.
	Version Version

	OnVideo func(*VideoFrame)
	OnAudio func(*AudioFrame)
	// OnClosed reports, once, a stream that died while open: a media read
	// failed, no conversation delivered media for mediaIdleTimeout, or the
	// signalling session failed. It is not called for a failure detected
	// after Close began or after the Start context ended; one detected just
	// before can still be delivered while Close runs, so the receiver must
	// tolerate a late call.
	OnClosed func(error)

	// Signaller, when set, carries the session's 302 frames instead of the
	// 6668 session: the imm path over the cloud broker, for monitors that
	// cannot take a LAN offer (Tuya LAN 3.3). IP is then only a hint, used to
	// announce our candidate before the answer; MotoID and ICEServers fill
	// the offer, as the app's cloud offers carry them. See cloud.go.
	Signaller  Signaller
	MotoID     string
	ICEServers []ICEServer
	// HandshakeTimeout caps the handshake in place of startTimeout when set.
	HandshakeTimeout time.Duration
	// StartupGap is the longest pause between two messages of the startup
	// sequence (the login and six setup messages): the next one goes out when
	// the monitor has answered the previous one, or after this long. There is
	// none after the last one. Zero means DefaultStartupGap; a negative value
	// sends them back to back without waiting for answers.
	StartupGap time.Duration
	// Clarity is the video clarity the startup sequence asks for: ClarityHD
	// or ClarityStandard, what the app asks for. Zero is ClarityStandard.
	Clarity uint32
	// OnControl, when set, is told the size of everything the control
	// conversation delivers after the login. Nothing reads those messages;
	// whether any came tells a refused login from a silent path.
	OnControl func(n int)
}

// Client streams from one monitor over the local network, without the cloud.
// It is used once: Start, then Close.
type Client struct {
	cfg ClientConfig

	mu     sync.Mutex
	closed bool
	failed bool // OnClosed has been called
	cancel context.CancelFunc
	// closers is everything Start opened, closed in reverse order by Close.
	closers []io.Closer
	// epoch keeps the idle clock monotonic: a wall-clock step (NTP on a board
	// without an RTC) must neither kill a live stream nor hide a dead one.
	epoch time.Time
	// lastMedia is when any conversation last delivered a media message, as
	// time since epoch.
	lastMedia atomic.Int64
}

// mediaReader is what consume and awaitReply need from a KCP conversation.
type mediaReader interface {
	Read([]byte) (int, error)
	SetReadDeadline(time.Time) error
}

// NewClient prepares a client; nothing touches the network until Start.
func NewClient(cfg ClientConfig) *Client {
	c := &Client{cfg: cfg, epoch: time.Now()}
	c.markMedia()
	return c
}

func (c *Client) markMedia() { c.lastMedia.Store(int64(time.Since(c.epoch))) }

func (c *Client) mediaDeadline() time.Time {
	return c.epoch.Add(time.Duration(c.lastMedia.Load()) + mediaIdleTimeout)
}

var candidateRE = regexp.MustCompile(`candidate:(\S+) (\d+) (\S+) (\d+) (\S+) (\d+) typ (\S+)`)

// Start negotiates a session and begins streaming.
//
// ctx bounds the handshake (dial, answer, ICE and login, at most startTimeout)
// and is also the stream's life: when it ends the client closes as with Close,
// without calling OnClosed. On error everything Start opened is closed.
func (c *Client) Start(ctx context.Context) error {
	ip := net.ParseIP(c.cfg.IP)
	if ip == nil && c.cfg.Signaller == nil {
		return fmt.Errorf("lan: no usable address for %s: %q", c.cfg.DeviceID, c.cfg.IP)
	}
	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return net.ErrClosed
	}
	c.cancel = cancel
	c.mu.Unlock()
	// Closing the sockets is also what unblocks a handshake step in progress.
	context.AfterFunc(ctx, func() { _ = c.Close() })

	timeout := startTimeout
	if c.cfg.HandshakeTimeout > 0 {
		timeout = c.cfg.HandshakeTimeout
	}
	setup, done := context.WithTimeout(ctx, timeout)
	defer done()
	connect := c.connect
	if c.cfg.Signaller != nil {
		connect = c.connectCloud
	}
	if err := connect(ctx, setup, ip); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			// Both wrapped, so the step the handshake stopped at (cloud.go's
			// errors) stays visible to errors.Is. The text is unchanged.
			err = fmt.Errorf("lan: start ended: %w (%w)", cerr, err)
		}
		_ = c.Close()
		return err
	}
	return nil
}

// connect runs the handshake under setup and leaves the media and keep-alive
// running under life.
func (c *Client) connect(life, setup context.Context, ip net.IP) error {
	session, err := Dial(setup, c.cfg.IP, c.cfg.DeviceID, c.cfg.LocalKey, c.cfg.Version)
	if err != nil {
		return err
	}
	if err := c.hold(session); err != nil {
		return err
	}
	sock, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return err
	}
	if err := c.hold(sock); err != nil {
		return err
	}

	local, err := newICECreds()
	if err != nil {
		return err
	}
	offer, err := NewOffer(c.cfg.UID, c.cfg.DeviceID, local.Ufrag, local.Pwd)
	if err != nil {
		return err
	}
	if err := offer.SendOffer(session); err != nil {
		return err
	}
	hostIP, err := localAddrFor(&net.UDPAddr{IP: ip, Port: Port})
	if err != nil {
		return err
	}
	port := sock.LocalAddr().(*net.UDPAddr).Port
	if err := offer.SendCandidate(session, hostCandidate(hostIP, port)); err != nil {
		return err
	}

	remote, aesKey, peer, err := collectAnswer(setup, session, offer.SessionID)
	if err != nil {
		return err
	}
	if _, err := iceConnect(setup, sock, onePeer(peer), local, remote); err != nil {
		return err
	}
	if err := c.stream(life, setup, sock, peer, local, aesKey); err != nil {
		return err
	}
	go c.keepAlive(life, session)
	return nil
}

// stream logs in over the pair ICE settled on and starts the readers, which
// run under life. The media path is the same whichever route carried the
// signalling.
func (c *Client) stream(life, setup context.Context, sock *net.UDPConn, peer *net.UDPAddr, local iceCreds, aesKey []byte) error {
	// The monitor splits control and media across two KCP conversations, so a
	// router under both sessions sorts the datagrams by conversation id.
	router := newPacketRouter(sock, peer, aesKey, local)
	control, err := newKCP(router, convControl, peer)
	if err != nil {
		return err
	}
	if err := c.hold(control); err != nil {
		return err
	}
	if err := login(setup, control, aesKey, LoginToken(c.cfg.Password, c.cfg.LocalKey), c.startupGap(), c.cfg.OnControl, c.cfg.Clarity); err != nil {
		return err
	}
	// The waits left a read deadline on the conversation; drainControl reads
	// it without one.
	if err := control.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	c.markMedia() // the idle clock starts with the stream, not with NewClient
	go c.drainControl(life, control)
	go c.attachMedia(life, router, peer, aesKey)
	return nil
}

// hold hands r to Close, or closes it at once if Close has already run.
func (c *Client) hold(r io.Closer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		_ = r.Close()
		return net.ErrClosed
	}
	c.closers = append(c.closers, r)
	return nil
}

// attachMedia starts a reader for each conversation the monitor opens.
//
// Which id carries the video is not fixed: it has been seen as 1 and as 2 on
// the same camera, so the session follows whatever arrives.
func (c *Client) attachMedia(ctx context.Context, router *packetRouter, peer *net.UDPAddr, aesKey []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case conv := <-router.opened:
			if conv == convControl {
				continue
			}
			stream, err := newKCP(router, conv, peer)
			if err != nil {
				c.fail(ctx, fmt.Errorf("lan: media conversation %d: %w", conv, err))
				return
			}
			if c.hold(stream) != nil {
				return
			}
			go c.consume(ctx, stream, aesKey)
		}
	}
}

// drainControl reads and drops the control conversation. Nothing needs it
// after login, but left unread it would fill the KCP receive window over
// hours of streaming. It ends with the client, silently.
func (c *Client) drainControl(ctx context.Context, control io.Reader) {
	var sink io.Writer = io.Discard
	if c.cfg.OnControl != nil {
		sink = byteCounter(c.cfg.OnControl)
	}
	_, err := io.Copy(sink, control)
	c.fail(ctx, fmt.Errorf("lan: control conversation: %w", err))
}

// byteCounter discards what it is given and reports how much.
type byteCounter func(n int)

func (f byteCounter) Write(p []byte) (int, error) {
	f(len(p))
	return len(p), nil
}

// collectAnswer waits for the monitor's answer and its first host candidate.
func collectAnswer(ctx context.Context, s *Session, sessionID string) (remote iceCreds, aesKey []byte, peer *net.UDPAddr, err error) {
	deadline, _ := ctx.Deadline()
	for time.Now().Before(deadline) {
		frame, err := ReadSignal(s, sessionID, time.Until(deadline))
		if err != nil {
			return iceCreds{}, nil, nil, err
		}
		switch frame.Header.Type {
		case TypeAnswer:
			remote.Ufrag = sdpAttr(frame.Msg.SDP, "ice-ufrag")
			remote.Pwd = sdpAttr(frame.Msg.SDP, "ice-pwd")
			raw, decErr := hex.DecodeString(sdpAttr(frame.Msg.SDP, "aes-key"))
			if decErr != nil {
				return iceCreds{}, nil, nil, fmt.Errorf("lan: bad aes-key in answer: %w", decErr)
			}
			aesKey = raw
		case TypeCandidate:
			if addr, parseErr := parseCandidate(frame.Msg.Candidate); parseErr == nil {
				peer = addr
			}
		case TypeDisconnect:
			return iceCreds{}, nil, nil, errors.New("lan: monitor refused the session")
		}
		if remote.Ufrag != "" && peer != nil {
			return remote, aesKey, peer, nil
		}
	}
	return iceCreds{}, nil, nil, errors.New("lan: monitor never completed the answer")
}

// controlConn is what login needs from the control conversation.
type controlConn interface {
	io.Writer
	mediaReader
}

// login sends the credentials, then the messages that start the stream (asking
// for clarity), each
// after the monitor answered the one before or after gap (sendPaced). What
// those answers carry goes to onControl, when set, as drainControl's later
// messages do.
func login(ctx context.Context, control controlConn, aesKey []byte, token string, gap time.Duration, onControl func(int), clarity uint32) error {
	msgs, err := startupSequence(token, clarity)
	if err != nil {
		return err
	}
	buf := make([]byte, maxControlMessage)
	return sendPaced(ctx, msgs, gap, func(msg []byte) error {
		sealed, err := seal(aesKey, msg)
		if err != nil {
			return err
		}
		_, err = control.Write(sealed)
		return err
	}, func(ctx context.Context, d time.Duration) error {
		n, err := awaitReply(ctx, control, buf, d)
		if n > 0 && onControl != nil {
			onControl(n)
		}
		return err
	})
}

// maxControlMessage bounds one message read from the control conversation
// during the login, as consume bounds a media message.
const maxControlMessage = 65535

// awaitReply waits at most d for one message on the control conversation:
// its size when one came, 0 when d (or ctx) ran out first. The message itself
// is not needed; drainControl discards the later ones the same way.
func awaitReply(ctx context.Context, r mediaReader, buf []byte, d time.Duration) (int, error) {
	deadline := time.Now().Add(d)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := r.SetReadDeadline(deadline); err != nil {
		return 0, err
	}
	n, err := r.Read(buf)
	if err != nil {
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return 0, ctx.Err()
		}
		return 0, err
	}
	return n, nil
}

func (c *Client) startupGap() time.Duration {
	switch {
	case c.cfg.StartupGap < 0:
		return 0
	case c.cfg.StartupGap == 0:
		return DefaultStartupGap
	}
	return c.cfg.StartupGap
}

// sendPaced sends msgs in order with gap between two of them, and none after
// the last: nothing follows it, so a pause there only delays the stream.
func sendPaced(ctx context.Context, msgs [][]byte, gap time.Duration, send func([]byte) error, wait func(context.Context, time.Duration) error) error {
	for i, msg := range msgs {
		if i > 0 && gap > 0 {
			if err := wait(ctx, gap); err != nil {
				return err
			}
		}
		if err := send(msg); err != nil {
			return err
		}
	}
	return nil
}

// consume reads one media conversation until it fails, handing each message to
// the callbacks.
//
// The idle deadline is client-wide: a conversation that stays quiet is fine
// while another delivers, and the stream is dead only when all of them are.
func (c *Client) consume(ctx context.Context, r mediaReader, aesKey []byte) error {
	buf := make([]byte, 65535)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.SetReadDeadline(c.mediaDeadline()); err != nil {
			return c.fail(ctx, err)
		}
		n, err := r.Read(buf)
		if isTimeout(err) {
			if time.Now().Before(c.mediaDeadline()) {
				continue // another conversation delivered meanwhile
			}
			return c.fail(ctx, fmt.Errorf("lan: %w for %v: %w", ErrMediaIdle, mediaIdleTimeout, err))
		}
		if err != nil {
			return c.fail(ctx, fmt.Errorf("lan: media read: %w", err))
		}
		msg, err := open(aesKey, buf[:n])
		if err != nil {
			continue
		}
		cmd, ts, _, payload, ok := mediaPayload(msg)
		if !ok {
			continue
		}
		c.markMedia()
		switch cmd {
		case CmdMediaVideo:
			if c.cfg.OnVideo != nil {
				c.cfg.OnVideo(parseVideo(msg, ts, payload))
			}
		case CmdMediaAudio:
			if c.cfg.OnAudio != nil {
				c.cfg.OnAudio(parseAudio(msg, ts, payload))
			}
		}
	}
}

// keepAlive holds the signalling session open; the monitor drops an idle one.
// Nothing else reads the session after the handshake, so it also drains it:
// heartbeat replies and pushes would otherwise fill the socket buffer over
// hours of streaming.
func (c *Client) keepAlive(ctx context.Context, s *Session) {
	go func() { c.fail(ctx, fmt.Errorf("lan: signalling session: %w", s.Drain())) }()

	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Heartbeat(); err != nil {
				c.fail(ctx, fmt.Errorf("lan: keep-alive: %w", err))
				return
			}
		}
	}
}

// fail reports a dead stream through OnClosed, once, unless the owner has
// already ended it. It returns err unchanged.
func (c *Client) fail(ctx context.Context, err error) error {
	c.mu.Lock()
	report := !c.closed && !c.failed && ctx.Err() == nil
	if report {
		c.failed = true
	}
	c.mu.Unlock()
	if report && c.cfg.OnClosed != nil {
		c.cfg.OnClosed(err)
	}
	return err
}

// Close tears the session down. OnClosed is not called after it.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cancel, closers := c.cancel, c.closers
	c.closers = nil
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		errs = append(errs, closers[i].Close())
	}
	return errors.Join(errs...)
}

func sdpAttr(sdp, name string) string {
	for _, line := range splitLines(sdp) {
		if strings.HasPrefix(line, "a="+name+":") {
			return strings.TrimPrefix(line, "a="+name+":")
		}
	}
	return ""
}

// splitLines splits an SDP on CRLF, or on a bare LF from a sender that uses
// one.
func splitLines(sdp string) []string {
	lines := strings.Split(sdp, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}

func parseCandidate(line string) (*net.UDPAddr, error) {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "a="))
	m := candidateRE.FindStringSubmatch(line)
	if m == nil {
		return nil, errors.New("lan: unusable candidate")
	}
	ip := net.ParseIP(m[5])
	if ip == nil {
		return nil, errors.New("lan: candidate carries no address")
	}
	port, err := strconv.Atoi(m[6])
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: ip, Port: port}, nil
}
