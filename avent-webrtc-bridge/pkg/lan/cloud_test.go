package lan

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v4"
)

// fakeCloud is the broker between the client and a fake monitor: what the
// client sends lands on sent, and the monitor's frames go out on frames.
type fakeCloud struct {
	sent      chan any
	frames    chan SignalFrame
	closed    atomic.Bool
	openErr   error
	mu        sync.Mutex
	sessionID string
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{sent: make(chan any, 64), frames: make(chan SignalFrame, 64)}
}

func (f *fakeCloud) Open(sessionID string) (<-chan SignalFrame, error) {
	f.mu.Lock()
	f.sessionID = sessionID
	f.mu.Unlock()
	return f.frames, f.openErr
}

func (f *fakeCloud) Send(frame any) error {
	select {
	case f.sent <- frame:
	default:
	}
	return nil
}

func (f *fakeCloud) Close() error {
	f.closed.Store(true)
	return nil
}

// next returns the next frame the client sent, failing the test after d.
// Only the test goroutine may call it.
func (f *fakeCloud) next(t *testing.T, d time.Duration) any {
	t.Helper()
	fr := f.await(d)
	if fr == nil {
		t.Fatalf("the client sent nothing within %v", d)
	}
	return fr
}

// await returns the next frame the client sent, or nil after d. Safe on any
// goroutine.
func (f *fakeCloud) await(d time.Duration) any {
	select {
	case fr := <-f.sent:
		return fr
	case <-time.After(d):
		return nil
	}
}

func (f *fakeCloud) say(typ, sdp, candidate string) {
	f.mu.Lock()
	id := f.sessionID
	f.mu.Unlock()
	f.frames <- SignalFrame{
		Header: SignalHeader{Type: typ, SessionID: id},
		Msg:    SignalMessage{SDP: sdp, Candidate: candidate},
	}
}

const monitorPwd = "monitorpasswordmonitorpw" // 24 characters, as the monitor answers

func answerSDP(withKey bool) string {
	lines := []string{"v=0", "m=application 9 imm 6001", "a=ice-ufrag:mntr", "a=ice-pwd:" + monitorPwd}
	if withKey {
		lines = append(lines, "a=aes-key:"+hex.EncodeToString(testKey))
	}
	lines = append(lines, "a=rtpmap:6001 AES/KCP 3", "")
	return strings.Join(lines, "\r\n")
}

func candidateLine(addr *net.UDPAddr) string {
	return fmt.Sprintf("a=candidate:1 1 UDP 2130706431 %s %d typ host\r\n", addr.IP, addr.Port)
}

func loopbackMonitors(t *testing.T) {
	t.Helper()
	acceptLoopback.Store(true)
	t.Cleanup(func() { acceptLoopback.Store(false) })
}

// fakeMonitor answers the ICE checks on its own socket, takes the login on
// conversation 0 and sends one video message, carrying an SPS, on conversation 1.
type fakeMonitor struct {
	sock  *net.UDPConn
	login chan []byte // the first control message, decrypted
}

func startFakeMonitor(t *testing.T) *fakeMonitor {
	t.Helper()
	sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sock.Close() })
	m := &fakeMonitor{sock: sock, login: make(chan []byte, 1)}
	go m.run()
	return m
}

func (m *fakeMonitor) addr() *net.UDPAddr { return m.sock.LocalAddr().(*net.UDPAddr) }

func (m *fakeMonitor) run() {
	local := iceCreds{Ufrag: "mntr", Pwd: monitorPwd}
	buf := make([]byte, 2048)
	var client *net.UDPAddr
	for client == nil {
		n, from, err := m.sock.ReadFromUDP(buf)
		if err != nil {
			return
		}
		msg := &stun.Message{Raw: append([]byte{}, buf[:n]...)}
		if !isSTUN(buf[:n]) || msg.Decode() != nil || msg.Type.Class != stun.ClassRequest {
			continue
		}
		// The real monitor answers without MESSAGE-INTEGRITY; the client
		// must not care either way.
		resp, err := stun.Build(stun.BindingSuccess, stun.NewTransactionIDSetter(msg.TransactionID),
			&stun.XORMappedAddress{IP: from.IP, Port: from.Port}, stun.Fingerprint)
		if err != nil {
			return
		}
		_, _ = m.sock.WriteToUDP(resp.Raw, from)
		client = from
	}
	router := newPacketRouter(m.sock, client, testKey, local)
	control, err := newKCP(router, convControl, client)
	if err != nil {
		return
	}
	defer control.Close()
	_ = control.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := control.Read(buf)
	if err != nil {
		return
	}
	plain, err := open(testKey, buf[:n])
	if err != nil {
		return
	}
	m.login <- plain

	media, err := newKCP(router, 1, client)
	if err != nil {
		return
	}
	defer media.Close()
	sps := []byte{0x67, 0x42, 0x00, 0x1f}
	sealed, err := seal(testKey, message(longSubHeader(len(sps), 1), sps))
	if err != nil {
		return
	}
	for i := 0; i < 50; i++ { // until the client stops; KCP needs the reader alive
		if _, err := media.Write(sealed); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func cloudClient(cloud *fakeCloud, videos chan *VideoFrame, closed chan error, timeout time.Duration) *Client {
	return NewClient(ClientConfig{
		DeviceID:         "dev",
		LocalKey:         "fake-local-key-0",
		Password:         "fake-p2p-pass",
		UID:              "uid",
		Signaller:        cloud,
		MotoID:           "moto-1",
		HandshakeTimeout: timeout,
		OnVideo: func(f *VideoFrame) {
			select {
			case videos <- f:
			default:
			}
		},
		OnClosed: func(err error) { closed <- err },
	})
}

func TestCloudHandshakeStreams(t *testing.T) {
	loopbackMonitors(t)
	cloud := newFakeCloud()
	monitor := startFakeMonitor(t)
	videos, closed := make(chan *VideoFrame, 4), make(chan error, 4)
	c := cloudClient(cloud, videos, closed, 5*time.Second)

	go func() {
		offer, ok := cloud.await(2 * time.Second).(SignalFrame)
		if !ok || offer.Header.Type != TypeOffer || offer.Header.Path != PathMQTT || offer.Header.MotoID != "moto-1" {
			t.Errorf("first frame = %+v, want the mqtt offer", offer)
			return
		}
		// Trickle after the answer, the way the monitor does.
		cloud.say(TypeAnswer, answerSDP(true), "")
		cloud.say(TypeCandidate, "", candidateLine(monitor.addr()))
	}()

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case login := <-monitor.login:
		if binary.LittleEndian.Uint32(login[0:4]) != mediaMagic || binary.LittleEndian.Uint32(login[4:8]) != 1 {
			t.Errorf("first control message is not the login: % x", login[:8])
		}
		if want := LoginToken("fake-p2p-pass", "fake-local-key-0"); string(login[40:72]) != want {
			t.Errorf("login token = %q, want %q", login[40:72], want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no login reached the monitor")
	}
	select {
	case v := <-videos:
		if len(v.NAL) == 0 || v.NAL[0]&0x1f != 7 {
			t.Errorf("video = % x, want the SPS", v.NAL)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no video")
	}

	if err := c.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	var sawDisconnect bool
	for !sawDisconnect {
		switch f := cloud.next(t, time.Second).(type) {
		case DisconnectFrame:
			sawDisconnect = f.Header.Type == TypeDisconnect
		}
	}
	if !cloud.closed.Load() {
		t.Error("signaller not closed")
	}
	select {
	case err := <-closed:
		t.Errorf("OnClosed(%v) after Close", err)
	default:
	}
}

// Each failing step names itself, and the monitor is told to drop the session.
func TestCloudHandshakeSteps(t *testing.T) {
	loopbackMonitors(t)
	silent, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()

	cases := []struct {
		name  string
		reply func(cloud *fakeCloud)
		want  error
	}{
		{"no answer", func(*fakeCloud) {}, ErrNoAnswer},
		{"refused", func(c *fakeCloud) { c.say(TypeDisconnect, "", "") }, ErrRefused},
		{"no aes-key", func(c *fakeCloud) { c.say(TypeAnswer, answerSDP(false), "") }, ErrBadAnswer},
		{"no candidate", func(c *fakeCloud) {
			c.say(TypeAnswer, answerSDP(true), "")
			c.say(TypeCandidate, "", "a=candidate:2 1 UDP 1694498815 203.0.113.9 50000 typ srflx")
		}, ErrNoCandidate},
		{"ice timeout", func(c *fakeCloud) {
			c.say(TypeAnswer, answerSDP(true), "")
			c.say(TypeCandidate, "", candidateLine(silent.LocalAddr().(*net.UDPAddr)))
		}, ErrICE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := newFakeCloud()
			c := cloudClient(cloud, make(chan *VideoFrame, 1), make(chan error, 1), 700*time.Millisecond)
			go func() {
				if _, ok := cloud.await(2 * time.Second).(SignalFrame); ok {
					tc.reply(cloud)
				}
			}()
			err := c.Start(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("start error = %v, want %v", err, tc.want)
			}
			deadline := time.After(time.Second)
			for {
				select {
				case f := <-cloud.sent:
					if _, ok := f.(DisconnectFrame); ok {
						if !cloud.closed.Load() {
							t.Error("signaller not closed after the disconnect")
						}
						return
					}
				case <-deadline:
					t.Fatal("no disconnect after a failed start")
				}
			}
		})
	}
}

// A disconnect from the monitor ends a live stream through OnClosed.
func TestCloudDisconnectEndsALiveStream(t *testing.T) {
	loopbackMonitors(t)
	cloud := newFakeCloud()
	monitor := startFakeMonitor(t)
	videos, closed := make(chan *VideoFrame, 4), make(chan error, 4)
	c := cloudClient(cloud, videos, closed, 5*time.Second)
	defer c.Close()
	go func() {
		cloud.await(2 * time.Second)
		cloud.say(TypeAnswer, answerSDP(true)+candidateLine(monitor.addr()), "") // inline candidate
	}()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	cloud.say(TypeDisconnect, "", "")
	select {
	case err := <-closed:
		if !errors.Is(err, ErrEnded) {
			t.Errorf("OnClosed(%v), want ErrEnded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a disconnect did not end the stream")
	}
}

// With the monitor's last known address, our candidate goes out with the offer.
func TestCloudHintAnnouncesEarly(t *testing.T) {
	cloud := newFakeCloud()
	c := NewClient(ClientConfig{DeviceID: "dev", UID: "uid", IP: "127.0.0.1", Signaller: cloud, HandshakeTimeout: 300 * time.Millisecond})
	go func() { _ = c.Start(context.Background()) }()
	defer c.Close()
	if f, ok := cloud.next(t, time.Second).(SignalFrame); !ok || f.Header.Type != TypeOffer {
		t.Fatalf("first frame = %+v, want the offer", f)
	}
	f, ok := cloud.next(t, time.Second).(SignalFrame)
	if !ok || f.Header.Type != TypeCandidate || !strings.Contains(f.Msg.Candidate, " 127.0.0.1 ") || !strings.Contains(f.Msg.Candidate, "typ host") {
		t.Fatalf("second frame = %+v, want our host candidate", f)
	}
}

func TestUsableCandidate(t *testing.T) {
	cases := map[string]bool{
		"a=candidate:1 1 UDP 2130706431 192.168.1.20 47154 typ host\r\n": true,
		"candidate:1 1 UDP 2130706431 10.0.0.7 5000 typ host":            true,
		"a=candidate:2 1 UDP 1694498815 203.0.113.9 50000 typ srflx":     false,
		"a=candidate:3 1 UDP 16777215 198.51.100.4 3478 typ relay":       false,
		"a=candidate:1 1 UDP 2130706431 fe80::1 47154 typ host":          false,
		"a=candidate:1 1 UDP 2130706431 169.254.3.3 47154 typ host":      false,
		"a=candidate:1 1 UDP 2130706431 127.0.0.1 47154 typ host":        false,
		"a=candidate:1 1 UDP 2130706431 192.168.1.20 0 typ host":         false,
		"garbage": false,
	}
	for line, want := range cases {
		if _, got := usableCandidate(line); got != want {
			t.Errorf("usableCandidate(%q) = %t, want %t", line, got, want)
		}
	}
}
