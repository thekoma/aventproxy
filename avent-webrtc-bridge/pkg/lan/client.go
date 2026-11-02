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
	"time"
)

const (
	// startTimeout caps the handshake when the caller's context does not.
	startTimeout = 20 * time.Second
	// mediaIdleTimeout is how long a media conversation may stay silent before
	// the stream counts as dead; audio alone arrives about 50 times a second.
	mediaIdleTimeout = 3 * time.Second
	loginPacing      = 120 * time.Millisecond
)

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

	OnVideo func(*VideoFrame)
	OnAudio func(*AudioFrame)
	// OnClosed reports, once, a stream that died while open: a media read
	// failed or stayed idle for mediaIdleTimeout, or the keep-alive failed. It
	// is not called after Close or once the Start context has ended.
	OnClosed func(error)
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
}

// mediaReader is what consume needs from a KCP conversation.
type mediaReader interface {
	Read([]byte) (int, error)
	SetReadDeadline(time.Time) error
}

// NewClient prepares a client; nothing touches the network until Start.
func NewClient(cfg ClientConfig) *Client {
	return &Client{cfg: cfg}
}

var candidateRE = regexp.MustCompile(`candidate:(\S+) (\d+) (\S+) (\d+) (\S+) (\d+) typ (\S+)`)

// Start negotiates a session and begins streaming.
//
// ctx bounds the handshake (dial, answer, ICE and login, at most startTimeout)
// and is also the stream's life: when it ends the client closes as with Close,
// without calling OnClosed. On error everything Start opened is closed.
func (c *Client) Start(ctx context.Context) error {
	ip := net.ParseIP(c.cfg.IP)
	if ip == nil {
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

	setup, done := context.WithTimeout(ctx, startTimeout)
	defer done()
	if err := c.connect(ctx, setup, ip); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			err = fmt.Errorf("lan: start ended: %w (%v)", cerr, err)
		}
		_ = c.Close()
		return err
	}
	return nil
}

// connect runs the handshake under setup and leaves the media and keep-alive
// running under life.
func (c *Client) connect(life, setup context.Context, ip net.IP) error {
	session, err := Dial(setup, c.cfg.IP, c.cfg.DeviceID, c.cfg.LocalKey)
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
	if err := iceConnect(setup, sock, peer, local, remote); err != nil {
		return err
	}

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
	if err := login(setup, control, aesKey, LoginToken(c.cfg.Password, c.cfg.LocalKey)); err != nil {
		return err
	}
	go c.attachMedia(life, router, peer, aesKey)
	go c.keepAlive(life, session)
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

// login sends the credentials, then the messages that start the stream.
func login(ctx context.Context, control io.Writer, aesKey []byte, token string) error {
	msgs, err := startupSequence(token)
	if err != nil {
		return err
	}
	for _, msg := range msgs {
		sealed, err := seal(aesKey, msg)
		if err != nil {
			return err
		}
		if _, err := control.Write(sealed); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(loginPacing):
		}
	}
	return nil
}

// consume reads one media conversation until it fails, handing each message to
// the callbacks.
func (c *Client) consume(ctx context.Context, r mediaReader, aesKey []byte) error {
	buf := make([]byte, 65535)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.SetReadDeadline(time.Now().Add(mediaIdleTimeout)); err != nil {
			return c.fail(ctx, err)
		}
		n, err := r.Read(buf)
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
func (c *Client) keepAlive(ctx context.Context, s *Session) {
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
	for _, line := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(line, "a="+name+":") {
			return strings.TrimPrefix(line, "a="+name+":")
		}
	}
	return ""
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
