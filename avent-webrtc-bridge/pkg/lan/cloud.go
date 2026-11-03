package lan

// The imm session over the cloud broker: the app's own transport (AES/KCP),
// with its 302 frames carried by the cloud instead of the 6668 session.
// Monitors on Tuya LAN 3.3 ignore a LAN offer but run the same P2P SDK, and
// the SDK parses a 302 frame the same way whichever route brought it. Only
// the signalling changes: ICE, KCP, the login and the media are the LAN
// path's. Untested on hardware; every step that can fail says which one it
// was, so a wrong guess falls back fast and names itself.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

// Signaller carries an imm session's 302 frames when the 6668 session cannot.
type Signaller interface {
	// Open starts delivering the monitor's frames for sessionID, in arrival
	// order. The channel is never closed.
	Open(sessionID string) (<-chan SignalFrame, error)
	// Send publishes one frame of the session.
	Send(frame any) error
	// Close stops the delivery. It tells the monitor nothing: the client
	// sends the disconnect frame first.
	Close() error
}

// The steps a cloud-signalled start can fail at, so the log can say which.
var (
	ErrOfferNotSent = errors.New("offer not sent")
	ErrRefused      = errors.New("monitor refused the session")
	ErrNoAnswer     = errors.New("no answer")
	ErrBadAnswer    = errors.New("answer unusable")
	ErrNoCandidate  = errors.New("answer without a usable candidate")
	ErrICE          = errors.New("ICE did not connect")
	ErrLogin        = errors.New("login not sent")
	// ErrEnded is a live session the monitor ended with a disconnect.
	ErrEnded = errors.New("monitor ended the session")
)

// connectCloud runs the handshake under setup with the frames going through
// c.cfg.Signaller, and leaves the media running under life. hint, when not
// nil, is the monitor's last known address: our candidate for it goes out
// with the offer, so the monitor's checks can start before its answer is back.
func (c *Client) connectCloud(life, setup context.Context, hint net.IP) error {
	sig := c.cfg.Signaller
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
	offer.Path, offer.MotoID = PathMQTT, c.cfg.MotoID
	if len(c.cfg.ICEServers) > 0 {
		offer.ICEServers = c.cfg.ICEServers
	}

	frames, err := sig.Open(offer.SessionID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOfferNotSent, err)
	}
	// Held before the offer goes out, so any later failure, and Close, tell
	// the monitor to drop the session before the next one starts.
	if err := c.hold(&cloudCloser{sig: sig, offer: offer}); err != nil {
		return err
	}
	if err := sig.Send(offer.OfferFrame()); err != nil {
		return fmt.Errorf("%w: %v", ErrOfferNotSent, err)
	}

	port := sock.LocalAddr().(*net.UDPAddr).Port
	announce := newAnnouncer(sig, offer, port)
	if hint != nil {
		announce.towards(hint)
	}
	signals := newCloudSignals()
	go c.pump(life, frames, signals, announce)

	remote, aesKey, err := signals.awaitAnswer(setup)
	if err != nil {
		return err
	}
	if err := signals.awaitPeer(setup); err != nil {
		return err
	}
	peer, err := iceConnect(setup, sock, signals.snapshot, local, remote)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrICE, err)
	}
	if err := c.stream(life, setup, sock, peer, local, aesKey); err != nil {
		return fmt.Errorf("%w: %v", ErrLogin, err)
	}
	signals.live.Store(true)
	if signals.isEnded() { // the disconnect came in between: pump did not report it
		return ErrEnded
	}
	return nil
}

// cloudCloser ends the session at the monitor, then stops the delivery.
type cloudCloser struct {
	sig   Signaller
	offer *Offer
}

func (cc *cloudCloser) Close() error {
	sendErr := cc.sig.Send(cc.offer.DisconnectFrame())
	return errors.Join(sendErr, cc.sig.Close())
}

// announcer trickles our host candidate, once per local address, for each
// route a monitor candidate needs.
type announcer struct {
	sig   Signaller
	offer *Offer
	port  int

	mu   sync.Mutex
	sent map[string]bool
}

func newAnnouncer(sig Signaller, offer *Offer, port int) *announcer {
	return &announcer{sig: sig, offer: offer, port: port, sent: map[string]bool{}}
}

// towards announces the address this host reaches ip from. A failure only
// costs this candidate: the monitor also learns our address from our checks.
func (a *announcer) towards(ip net.IP) {
	hostIP, err := localAddrFor(&net.UDPAddr{IP: ip, Port: Port})
	if err != nil {
		return
	}
	a.mu.Lock()
	if a.sent[hostIP.String()] {
		a.mu.Unlock()
		return
	}
	a.sent[hostIP.String()] = true
	a.mu.Unlock()
	_ = a.sig.Send(a.offer.CandidateFrame(hostCandidate(hostIP, a.port)))
}

// cloudSignals is what the monitor has said so far.
type cloudSignals struct {
	answer chan SignalFrame // the first answer
	more   chan struct{}    // a peer was added
	ended  chan struct{}    // closed by a disconnect
	end    sync.Once
	live   atomic.Bool // the handshake is over: a disconnect now fails the stream

	mu    sync.Mutex
	peers []*net.UDPAddr
	seen  map[string]bool
}

func newCloudSignals() *cloudSignals {
	return &cloudSignals{
		answer: make(chan SignalFrame, 1),
		more:   make(chan struct{}, 1),
		ended:  make(chan struct{}),
		seen:   map[string]bool{},
	}
}

func (s *cloudSignals) isEnded() bool {
	select {
	case <-s.ended:
		return true
	default:
		return false
	}
}

// add records a monitor candidate and reports whether it was new.
func (s *cloudSignals) add(addr *net.UDPAddr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[addr.String()] {
		return false
	}
	s.seen[addr.String()] = true
	s.peers = append(s.peers, addr)
	select {
	case s.more <- struct{}{}:
	default:
	}
	return true
}

func (s *cloudSignals) snapshot() []*net.UDPAddr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*net.UDPAddr(nil), s.peers...)
}

// awaitAnswer waits for the answer and reads its ICE credentials and key.
func (s *cloudSignals) awaitAnswer(ctx context.Context) (iceCreds, []byte, error) {
	var answer SignalFrame
	select {
	case answer = <-s.answer:
	case <-s.ended:
		return iceCreds{}, nil, ErrRefused
	case <-ctx.Done():
		return iceCreds{}, nil, fmt.Errorf("%w: %v", ErrNoAnswer, ctx.Err())
	}
	remote := iceCreds{Ufrag: sdpAttr(answer.Msg.SDP, "ice-ufrag"), Pwd: sdpAttr(answer.Msg.SDP, "ice-pwd")}
	if remote.Ufrag == "" || remote.Pwd == "" {
		return iceCreds{}, nil, fmt.Errorf("%w: no ICE credentials", ErrBadAnswer)
	}
	key, err := hex.DecodeString(sdpAttr(answer.Msg.SDP, "aes-key"))
	if err != nil || len(key) != 16 {
		return iceCreds{}, nil, fmt.Errorf("%w: no 16-byte aes-key", ErrBadAnswer)
	}
	return remote, key, nil
}

// awaitPeer waits until the monitor has named a candidate the checks can use.
func (s *cloudSignals) awaitPeer(ctx context.Context) error {
	for len(s.snapshot()) == 0 {
		select {
		case <-s.more:
		case <-s.ended:
			return ErrRefused
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrNoCandidate, ctx.Err())
		}
	}
	return nil
}

// pump sorts the monitor's frames for the life of the session.
func (c *Client) pump(life context.Context, frames <-chan SignalFrame, s *cloudSignals, announce *announcer) {
	for {
		select {
		case <-life.Done():
			return
		case f := <-frames:
			switch f.Header.Type {
			case TypeAnswer:
				select {
				case s.answer <- f:
				default: // a repeated answer changes nothing
				}
				for _, line := range sdpLines(f.Msg.SDP, "a=candidate:") {
					c.offerPeer(s, announce, line)
				}
			case TypeCandidate:
				c.offerPeer(s, announce, f.Msg.Candidate)
			case TypeDisconnect:
				s.end.Do(func() { close(s.ended) })
				if s.live.Load() {
					c.fail(life, ErrEnded)
				}
			}
		}
	}
}

// offerPeer takes a monitor candidate the checks can use and announces our
// own candidate for its route.
func (c *Client) offerPeer(s *cloudSignals, announce *announcer, line string) {
	addr, ok := usableCandidate(line)
	if !ok || !s.add(addr) {
		return
	}
	announce.towards(addr.IP)
}

// acceptLoopback lets tests run a fake monitor on 127.0.0.1. A real monitor
// never offers loopback, and checks sent there would only reach this host.
var acceptLoopback atomic.Bool

// usableCandidate accepts the IPv4 host candidates: the first version reaches
// the monitor on the local network only, with no relay.
func usableCandidate(line string) (*net.UDPAddr, bool) {
	addr, err := parseCandidate(line)
	if err != nil {
		return nil, false
	}
	m := candidateRE.FindStringSubmatch(line)
	if m == nil || m[7] != "host" {
		return nil, false
	}
	ip := addr.IP.To4()
	if ip == nil || (ip.IsLoopback() && !acceptLoopback.Load()) || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || addr.Port == 0 {
		return nil, false
	}
	return &net.UDPAddr{IP: ip, Port: addr.Port}, true
}

// sdpLines returns the lines of sdp that start with prefix.
func sdpLines(sdp, prefix string) []string {
	var out []string
	for _, line := range splitLines(sdp) {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}
