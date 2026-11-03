package rtsp

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/lan"
)

// recChannel records publishes in order and can play the monitor.
type recChannel struct {
	mu        sync.Mutex
	published []string // frame types, in order
	closed    bool
	onFrame   func(json.RawMessage)
}

func (c *recChannel) Publish(frame any) error {
	typ := "?"
	switch f := frame.(type) {
	case lan.SignalFrame:
		typ = f.Header.Type
		if f.Header.Type == lan.TypeCandidate {
			typ += ":" + f.Msg.Candidate
		}
	case lan.DisconnectFrame:
		typ = f.Header.Type
	}
	c.mu.Lock()
	c.published = append(c.published, typ)
	c.mu.Unlock()
	return nil
}

func (c *recChannel) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func (c *recChannel) log() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.published...)
}

func (c *recChannel) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// say plays the monitor on this route.
func (c *recChannel) say(sessionID, typ string) {
	c.onFrame(json.RawMessage(fmt.Sprintf(`{"header":{"type":%q,"sessionid":%q},"msg":{"sdp":"v=0"}}`, typ, sessionID)))
}

type signalRig struct {
	sig      *mqttSignaller
	moto     *recChannel
	dev      *recChannel
	dials    atomic.Int32
	dialErr  error
	dialed   chan struct{}
	frames   <-chan lan.SignalFrame
	session  string
	dialHold chan struct{} // when set, the dial waits for it
}

func newSignalRig(t *testing.T, withDevice bool) *signalRig {
	t.Helper()
	r := &signalRig{moto: &recChannel{}, dev: &recChannel{}, dialed: make(chan struct{}, 1), session: "sess-1"}
	r.sig = &mqttSignaller{
		deviceName: "Test Cam",
		motoWait:   50 * time.Millisecond,
		open: func(_ string, onFrame func(json.RawMessage)) immChannel {
			r.moto.onFrame = onFrame
			return r.moto
		},
	}
	if withDevice {
		r.sig.device = func(onData func(json.RawMessage)) (immChannel, error) {
			r.dials.Add(1)
			if r.dialHold != nil {
				<-r.dialHold
			}
			defer func() { r.dialed <- struct{}{} }()
			if r.dialErr != nil {
				return nil, r.dialErr
			}
			r.dev.onFrame = onData
			return r.dev, nil
		}
	}
	frames, err := r.sig.Open(r.session)
	if err != nil {
		t.Fatal(err)
	}
	r.frames = frames
	return r
}

func (r *signalRig) offer(t *testing.T) {
	t.Helper()
	if err := r.sig.Send(lan.SignalFrame{Header: lan.SignalHeader{Type: lan.TypeOffer, SessionID: r.session}, Msg: lan.SignalMessage{SDP: "v=0"}}); err != nil {
		t.Fatal(err)
	}
}

func (r *signalRig) candidate(t *testing.T, c string) {
	t.Helper()
	if err := r.sig.Send(lan.SignalFrame{Header: lan.SignalHeader{Type: lan.TypeCandidate, SessionID: r.session}, Msg: lan.SignalMessage{Candidate: c}}); err != nil {
		t.Fatal(err)
	}
}

func (r *signalRig) waitDial(t *testing.T) {
	t.Helper()
	select {
	case <-r.dialed:
	case <-time.After(2 * time.Second):
		t.Fatal("the device channel was not tried")
	}
}

func nextFrame(t *testing.T, frames <-chan lan.SignalFrame) lan.SignalFrame {
	t.Helper()
	select {
	case f := <-frames:
		return f
	case <-time.After(time.Second):
		t.Fatal("no frame delivered")
		return lan.SignalFrame{}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A moto that answers keeps the session there: the device channel is never opened.
func TestMotoAnswerKeepsTheDeviceChannelShut(t *testing.T) {
	r := newSignalRig(t, true)
	r.offer(t)
	r.moto.say(r.session, lan.TypeAnswer)
	if f := nextFrame(t, r.frames); f.Header.Type != lan.TypeAnswer {
		t.Fatalf("frame = %+v", f)
	}
	time.Sleep(150 * time.Millisecond) // past the wait
	if n := r.dials.Load(); n != 0 {
		t.Fatalf("device channel dialled %d times after a moto answer", n)
	}
	_ = r.sig.Close()
	if !r.moto.isClosed() {
		t.Error("the moto session must be closed")
	}
}

// A silent moto: after the wait the same offer, then the candidates sent so
// far, go out on the device channel in order; later frames go to both routes;
// the answer is read there; other sessions' frames on that channel are ignored.
func TestSilentMotoTriesTheDeviceChannel(t *testing.T) {
	r := newSignalRig(t, true)
	r.offer(t)
	r.candidate(t, "c1")
	r.waitDial(t)
	r.candidate(t, "c2")

	if got, want := r.dev.log(), []string{"offer", "candidate:c1", "candidate:c2"}; !equalStrings(got, want) {
		t.Fatalf("device channel got %v, want %v", got, want)
	}
	if got, want := r.moto.log(), []string{"offer", "candidate:c1", "candidate:c2"}; !equalStrings(got, want) {
		t.Fatalf("moto got %v, want %v", got, want)
	}

	r.dev.say("the-phone-apps-session", lan.TypeAnswer)
	r.dev.say(r.session, lan.TypeAnswer)
	if f := nextFrame(t, r.frames); f.Header.SessionID != r.session || f.Header.Type != lan.TypeAnswer {
		t.Fatalf("frame = %+v, want this session's answer", f)
	}
	select {
	case f := <-r.frames:
		t.Fatalf("an extra frame was delivered: %+v", f)
	default:
	}
	r.sig.mu.Lock()
	via := r.sig.answered
	r.sig.mu.Unlock()
	if via != viaDevice {
		t.Errorf("answered via %q, want %q", via, viaDevice)
	}

	// The disconnect at the end reaches both routes, then both close.
	if err := r.sig.Send(lan.DisconnectFrame{Header: lan.SignalHeader{Type: lan.TypeDisconnect}}); err != nil {
		t.Fatal(err)
	}
	_ = r.sig.Close()
	if d, m := r.dev.log(), r.moto.log(); d[len(d)-1] != "disconnect" || m[len(m)-1] != "disconnect" {
		t.Errorf("last frames: device %v, moto %v; want a disconnect on both", d, m)
	}
	if !r.dev.isClosed() || !r.moto.isClosed() {
		t.Error("both routes must be closed")
	}
}

func TestDeviceChannelFailureLeavesTheMoto(t *testing.T) {
	r := newSignalRig(t, true)
	r.dialErr = errors.New("connect refused")
	r.offer(t)
	r.waitDial(t)
	r.candidate(t, "c1")
	if got := r.dev.log(); len(got) != 0 {
		t.Fatalf("device channel got %v after failing to open", got)
	}
	if got := r.moto.log(); !equalStrings(got, []string{"offer", "candidate:c1"}) {
		t.Fatalf("moto got %v", got)
	}
	// A late moto answer still counts.
	r.moto.say(r.session, lan.TypeAnswer)
	if f := nextFrame(t, r.frames); f.Header.Type != lan.TypeAnswer {
		t.Fatalf("frame = %+v", f)
	}
	_ = r.sig.Close()
}

func TestNoDeviceChannelMeansMotoOnly(t *testing.T) {
	r := newSignalRig(t, false)
	r.offer(t)
	time.Sleep(150 * time.Millisecond)
	if n := r.dials.Load(); n != 0 {
		t.Fatalf("dialled %d times", n)
	}
	_ = r.sig.Close()
}

func TestCloseBeforeTheWaitNeverDials(t *testing.T) {
	r := newSignalRig(t, true)
	r.offer(t)
	_ = r.sig.Close()
	time.Sleep(150 * time.Millisecond)
	if n := r.dials.Load(); n != 0 {
		t.Fatalf("dialled %d times after Close", n)
	}
}

// A session closed while the device channel was connecting drops it at once.
func TestCloseDuringTheDialDropsTheChannel(t *testing.T) {
	r := newSignalRig(t, true)
	r.dialHold = make(chan struct{})
	r.offer(t)
	deadline := time.After(2 * time.Second)
	for r.dials.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("never dialled")
		case <-time.After(10 * time.Millisecond):
		}
	}
	_ = r.sig.Close()
	close(r.dialHold)
	r.waitDial(t)
	time.Sleep(50 * time.Millisecond)
	if !r.dev.isClosed() || len(r.dev.log()) != 0 {
		t.Fatalf("closed %v, published %v; want closed and nothing sent", r.dev.isClosed(), r.dev.log())
	}
}

func TestDeviceChannelClientID(t *testing.T) {
	cases := map[string]string{
		"com.example_mb_phone_abc123_DEFAULT": "com.example_mb_phone_abc123_IMM",
		"tuya_bridge_mb_phone_abc123":         "tuya_bridge_mb_phone_abc123_IMM",
	}
	for in, want := range cases {
		if got := deviceChannelClientID(in); got != want {
			t.Errorf("deviceChannelClientID(%q) = %q, want %q", in, got, want)
		}
	}
}

// The moto route's frames arrive on the MQTT dispatch goroutine every camera
// shares: the receiver must return even when the session reads nothing.
func TestReceiverNeverBlocksTheDispatch(t *testing.T) {
	r := newSignalRig(t, false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ { // well past the queue
			r.moto.say(r.session, lan.TypeCandidate)
		}
		r.moto.say(r.session, lan.TypeDisconnect)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the receiver blocked the dispatch goroutine")
	}
	_ = r.sig.Close()
}
