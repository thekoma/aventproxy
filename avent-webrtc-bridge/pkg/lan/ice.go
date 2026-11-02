package lan

// The monitor speaks a subset of ICE that a conformant agent cannot complete.
// It always claims the controlling role, answering 487 Role Conflict to anyone
// who claims it too, and its binding success responses carry no
// MESSAGE-INTEGRITY, which pion discards outright. On a LAN there is exactly
// one candidate pair, so the connectivity checks are done here instead: the
// pairing is a single exchange, not a NAT-traversal problem.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/stun/v4"
	kcp "github.com/xtaci/kcp-go/v5"
)

const (
	// The app uses a 4-character ufrag and a 24-character password; the monitor
	// answers in kind.
	iceUfragLen = 4
	icePwdLen   = 24
	// Host candidates all carry this priority, so the value is fixed.
	icePriority = 2130706431

	iceCheckInterval = 200 * time.Millisecond
	iceStunMagic     = 0x2112A442

	// kcpHeaderLen is the fixed KCP segment header, whose first field is the
	// conversation id.
	kcpHeaderLen = 24

	attrPriority      = 0x0024
	attrICEControlled = 0x8029
)

// iceCreds is one side's short-term credentials.
type iceCreds struct {
	Ufrag string
	Pwd   string
}

func newICECreds() (iceCreds, error) {
	ufrag, err := randomASCII(iceUfragLen)
	if err != nil {
		return iceCreds{}, err
	}
	pwd, err := randomASCII(icePwdLen)
	if err != nil {
		return iceCreds{}, err
	}
	return iceCreds{Ufrag: ufrag, Pwd: pwd}, nil
}

// isSTUN reports whether a datagram is STUN rather than media.
func isSTUN(b []byte) bool {
	return len(b) >= 20 &&
		b[0]&0xC0 == 0 &&
		uint32(b[4])<<24|uint32(b[5])<<16|uint32(b[6])<<8|uint32(b[7]) == iceStunMagic
}

// bindingRequest builds one connectivity check.
//
// We announce the controlled role: the monitor rejects a second controlling
// agent, and on a single pair there is nothing to nominate anyway.
func bindingRequest(local, remote iceCreds) (*stun.Message, error) {
	tiebreaker := make([]byte, 8)
	if _, err := rand.Read(tiebreaker); err != nil {
		return nil, err
	}
	priority := make([]byte, 4)
	binary.BigEndian.PutUint32(priority, icePriority)
	return stun.Build(
		stun.BindingRequest,
		stun.NewTransactionIDSetter(stun.NewTransactionID()),
		stun.NewUsername(remote.Ufrag+":"+local.Ufrag),
		&stun.RawAttribute{Type: attrPriority, Value: priority},
		&stun.RawAttribute{Type: attrICEControlled, Value: tiebreaker},
		stun.NewShortTermIntegrity(remote.Pwd),
		stun.Fingerprint,
	)
}

// bindingResponse answers the monitor's own check.
func bindingResponse(req *stun.Message, from *net.UDPAddr, local iceCreds) (*stun.Message, error) {
	return stun.Build(
		stun.BindingSuccess,
		stun.NewTransactionIDSetter(req.TransactionID),
		&stun.XORMappedAddress{IP: from.IP, Port: from.Port},
		stun.NewShortTermIntegrity(local.Pwd),
		stun.Fingerprint,
	)
}

// iceConnect runs checks against the monitor until one of them is answered.
//
// The monitor's response is deliberately not authenticated: it omits the
// integrity attribute, so its arrival on the pair is the only signal available.
func iceConnect(ctx context.Context, sock *net.UDPConn, peer *net.UDPAddr, local, remote iceCreds) error {
	// The checks poll with a read deadline; leaving it set would make every
	// later media read fail instantly.
	defer func() { _ = sock.SetReadDeadline(time.Time{}) }()

	buf := make([]byte, 2048)
	next := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("lan: no answer to the ICE checks: %w", err)
		}
		if time.Now().After(next) {
			req, err := bindingRequest(local, remote)
			if err != nil {
				return err
			}
			if _, err := sock.WriteToUDP(req.Raw, peer); err != nil {
				return err
			}
			next = time.Now().Add(iceCheckInterval)
		}

		if err := sock.SetReadDeadline(time.Now().Add(iceCheckInterval)); err != nil {
			return err
		}
		n, from, err := sock.ReadFromUDP(buf)
		if isTimeout(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !isSTUN(buf[:n]) {
			continue
		}
		msg := &stun.Message{Raw: append([]byte{}, buf[:n]...)}
		if err := msg.Decode(); err != nil {
			continue
		}
		switch msg.Type.Class {
		case stun.ClassSuccessResponse:
			return nil
		case stun.ClassRequest:
			if resp, err := bindingResponse(msg, from, local); err == nil {
				_, _ = sock.WriteToUDP(resp.Raw, from)
			}
		case stun.ClassErrorResponse:
			var code stun.ErrorCodeAttribute
			if err := code.GetFrom(msg); err == nil {
				return fmt.Errorf("lan: monitor rejected the ICE check: %d %s", code.Code, code.Reason)
			}
		}
	}
}

// isTimeout reports a read deadline passing, as opposed to a failed
// connection. kcp-go's timeout unwraps to a net.Error too.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// hostCandidate renders the SDP line for a socket the monitor can reach.
func hostCandidate(ip net.IP, port int) string {
	return fmt.Sprintf("candidate:1 1 UDP %d %s %d typ host", icePriority, ip, port)
}

// localAddrFor reports the address this host uses to reach the monitor.
func localAddrFor(peer *net.UDPAddr) (net.IP, error) {
	probe, err := net.DialUDP("udp4", nil, peer)
	if err != nil {
		return nil, err
	}
	defer probe.Close()
	return probe.LocalAddr().(*net.UDPAddr).IP, nil
}

// The monitor runs two KCP conversations over the one socket: conv 0 carries
// the control messages, another the media. A kcp session is bound to a single
// conv and drops everything else, so the datagrams are demultiplexed here and
// each conversation gets its own session.
type packetRouter struct {
	sock  *net.UDPConn
	peer  *net.UDPAddr
	key   []byte
	local iceCreds

	mu    sync.Mutex
	conns map[uint32]*convConn
	// opened announces a conversation the monitor started.
	opened chan uint32
	done   chan struct{}
}

func newPacketRouter(sock *net.UDPConn, peer *net.UDPAddr, key []byte, local iceCreds) *packetRouter {
	r := &packetRouter{
		sock:   sock,
		peer:   peer,
		key:    key,
		local:  local,
		conns:  map[uint32]*convConn{},
		opened: make(chan uint32, 8),
		done:   make(chan struct{}),
	}
	go r.run()
	return r
}

// conn returns the packet channel for one conversation.
func (r *packetRouter) conn(conv uint32) *convConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[conv]
	if !ok {
		c = &convConn{router: r, in: make(chan []byte, 1024)}
		r.conns[conv] = c
	}
	return c
}

// run sorts datagrams until the socket closes.
func (r *packetRouter) run() {
	buf := make([]byte, 65535)
	for {
		n, from, err := r.sock.ReadFromUDP(buf)
		if err != nil {
			close(r.done)
			return
		}
		if isSTUN(buf[:n]) {
			msg := &stun.Message{Raw: append([]byte{}, buf[:n]...)}
			if msg.Decode() == nil && msg.Type.Class == stun.ClassRequest {
				if resp, err := bindingResponse(msg, from, r.local); err == nil {
					_, _ = r.sock.WriteToUDP(resp.Raw, from)
				}
			}
			continue
		}
		if n < macLen+kcpHeaderLen {
			continue
		}
		body, ok := verify(r.key, buf[:n])
		if !ok {
			continue
		}
		conv := binary.LittleEndian.Uint32(body[:4])
		r.mu.Lock()
		c, known := r.conns[conv]
		if !known {
			c = &convConn{router: r, in: make(chan []byte, 1024)}
			r.conns[conv] = c
		}
		r.mu.Unlock()
		if !known {
			// The monitor chooses the media conversation id per session, so the
			// session is attached when the first segment for it arrives.
			select {
			case r.opened <- conv:
			default:
			}
		}
		select {
		case c.in <- append([]byte{}, body...):
		default: // queue full: drop, KCP retransmits
		}
	}
}

// convConn presents one conversation to kcp as if it owned the socket.
type convConn struct {
	router   *packetRouter
	in       chan []byte
	deadline atomic.Pointer[time.Time]
}

func (c *convConn) ReadFrom(p []byte) (int, net.Addr, error) {
	var timeout <-chan time.Time
	if d := c.deadline.Load(); d != nil && !d.IsZero() {
		t := time.NewTimer(time.Until(*d))
		defer t.Stop()
		timeout = t.C
	}
	select {
	case b := <-c.in:
		return copy(p, b), c.router.peer, nil
	case <-c.router.done:
		return 0, c.router.peer, net.ErrClosed
	case <-timeout:
		return 0, c.router.peer, os.ErrDeadlineExceeded
	}
}

func (c *convConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	if _, err := c.router.sock.WriteToUDP(sign(c.router.key, p), c.router.peer); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *convConn) Close() error        { return nil }
func (c *convConn) LocalAddr() net.Addr { return c.router.sock.LocalAddr() }

func (c *convConn) SetDeadline(t time.Time) error {
	c.deadline.Store(&t)
	return nil
}
func (c *convConn) SetReadDeadline(t time.Time) error { return c.SetDeadline(t) }
func (c *convConn) SetWriteDeadline(time.Time) error  { return nil }

// convControl is the conversation the monitor answers control messages on;
// the media conversation id varies per session.
const convControl = 0

// newKCP starts one conversation on the shared socket.
func newKCP(r *packetRouter, conv uint32, peer *net.UDPAddr) (*kcp.UDPSession, error) {
	s, err := kcp.NewConn3(conv, peer, nil, 0, 0, r.conn(conv))
	if err != nil {
		return nil, err
	}
	s.SetStreamMode(false) // preserve message boundaries
	s.SetWindowSize(512, 512)
	s.SetNoDelay(1, 10, 2, 1)
	return s, nil
}
