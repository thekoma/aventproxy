package rtsp

import (
	"fmt"
	"sync"
	"time"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/storage"

	"github.com/pion/rtp"
)

// Talkback modes. With session, every camera session is two-way, as before.
// With on_demand the camera session stays one-way, and a separate two-way
// session opens only while an RTSP client sends audio on the backchannel.
//
// Issue #72: a two-way session pauses a playing lullaby, and imm and the LAN
// carry no return audio, so a session-wide talkback costs every viewer the
// lullaby and the app's own transport. Home Assistant 2026.11 sends the
// microphone only while its toggle is on (ha-web-rtc-player replaces the
// track with none when it is off), so on demand the lullaby pauses only while
// a parent talks.
const (
	TalkbackSession  = "session"
	TalkbackOnDemand = "on_demand"
)

// ParseTalkbackMode checks a --talkback-mode value; empty means session.
func ParseTalkbackMode(mode string) (string, error) {
	switch mode {
	case "", TalkbackSession:
		return TalkbackSession, nil
	case TalkbackOnDemand:
		return TalkbackOnDemand, nil
	}
	return "", fmt.Errorf("talkback mode %q: want %s or %s", mode, TalkbackSession, TalkbackOnDemand)
}

const (
	// defaultTalkIdle is how long a talk session outlives the last packet.
	// The microphone sends a packet every 20 ms while it is on.
	defaultTalkIdle = 3 * time.Second
	// defaultTalkRetry is how long a failed talk session blocks a new one, so
	// a client that keeps sending does not start a session per packet.
	defaultTalkRetry = 10 * time.Second
)

// talkSession is a two-way camera session that carries a client's audio.
type talkSession interface {
	Start() error
	Stop()
	BackchannelReady() bool
	ForwardBackchannelAudioPacket(packet *rtp.Packet)
}

// talker opens a talk session when backchannel audio arrives and ends it once
// the audio stops. Packets that arrive before the session can carry them are
// dropped and counted: the first words of a talk are lost, which the log
// reports so the cost is measured before anything buffers them.
type talker struct {
	name      string
	open      func(onError func(error)) (talkSession, error)
	idleAfter time.Duration
	retryWait time.Duration

	mu       sync.Mutex
	session  talkSession // up and owned by the current talk
	starting bool
	gen      uint64 // bumped by end, so a start that outlived its talk knows
	early    error  // an error the starting session reported before it was kept
	idle     *time.Timer
	last     time.Time // the last packet
	retryAt  time.Time
	began    time.Time
	sent     uint64
	dropped  uint64
}

func newTalker(name string, open func(onError func(error)) (talkSession, error)) *talker {
	return &talker{name: name, open: open, idleAfter: defaultTalkIdle, retryWait: defaultTalkRetry}
}

// packet takes one backchannel packet from a client.
func (t *talker) packet(p *rtp.Packet) {
	t.mu.Lock()
	t.last = time.Now()
	if t.idle == nil {
		t.idle = time.AfterFunc(t.idleAfter, t.idleFired)
	}
	if t.began.IsZero() {
		t.began = t.last
	}
	if s := t.session; s != nil && s.BackchannelReady() {
		t.sent++
		t.mu.Unlock()
		s.ForwardBackchannelAudioPacket(p)
		return
	}
	t.dropped++
	if t.session == nil && !t.starting && !t.last.Before(t.retryAt) {
		t.starting = true
		go t.start(t.gen)
	}
	t.mu.Unlock()
}

// start opens and starts a talk session for the talk gen belongs to.
func (t *talker) start(gen uint64) {
	core.Logger.Info().Msgf("Audio arrived on the backchannel for %s: opening a two-way session to talk", t.name)
	var s talkSession
	s, err := t.open(func(err error) { t.failed(s, gen, err) })
	if err == nil {
		err = s.Start()
	}

	t.mu.Lock()
	if gen != t.gen { // the talk ended while the session started
		if err != nil {
			t.retryAt = time.Now().Add(t.retryWait)
		}
		t.mu.Unlock()
		if s != nil {
			s.Stop()
		}
		return
	}
	t.starting = false
	if err == nil && t.early != nil { // it failed between Start returning and here
		err = t.early
	}
	t.early = nil
	if err != nil {
		t.retryAt = time.Now().Add(t.retryWait)
		t.mu.Unlock()
		core.Logger.Warn().Err(err).Msgf("Could not open a two-way session to talk to %s; trying again in %v at the earliest",
			t.name, t.retryWait)
		if s != nil {
			s.Stop()
		}
		return
	}
	t.session = s
	waited := time.Since(t.began)
	t.mu.Unlock()
	core.Logger.Info().Msgf("Talking to %s: the two-way session took %v", t.name, waited.Round(time.Millisecond))
}

// failed ends the talk whose session reported an error. An error from a
// session still starting for the current talk is kept for start to act on.
func (t *talker) failed(s talkSession, gen uint64, err error) {
	t.mu.Lock()
	if s == nil || t.session != s {
		if s != nil && gen == t.gen && t.starting && t.early == nil {
			t.early = err
		}
		t.mu.Unlock()
		return
	}
	t.retryAt = time.Now().Add(t.retryWait)
	t.mu.Unlock()
	core.Logger.Warn().Err(err).Msgf("The two-way session to %s failed", t.name)
	t.end("its session failed")
}

// idleFired ends the talk once no packet has come for idleAfter.
func (t *talker) idleFired() {
	t.mu.Lock()
	if t.idle == nil {
		t.mu.Unlock()
		return
	}
	if quiet := time.Since(t.last); quiet < t.idleAfter { // a packet came as the timer fired
		t.idle.Reset(t.idleAfter - quiet)
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	t.end("the audio stopped")
}

// end closes the current talk, if any, and resets the counts.
func (t *talker) end(why string) {
	t.mu.Lock()
	s := t.session
	talked := !t.began.IsZero()
	since, sent, dropped := time.Since(t.began), t.sent, t.dropped
	t.session = nil
	t.starting = false
	t.early = nil
	t.gen++
	if t.idle != nil {
		t.idle.Stop()
		t.idle = nil
	}
	t.began = time.Time{}
	t.sent, t.dropped = 0, 0
	t.mu.Unlock()

	if s != nil {
		s.Stop()
	}
	if talked {
		core.Logger.Info().Msgf("Stopped talking to %s (%s) after %v: %d packets sent, %d dropped before the session could carry them",
			t.name, why, since.Round(time.Millisecond), sent, dropped)
	}
}

// talkBridge is a talk session over webrtc: a sendrecv audio offer on a bridge
// of its own, whose camera media goes to a forwarder no client reads.
type talkBridge struct{ *WebRTCBridge }

func (b talkBridge) BackchannelReady() bool { return b.backchannel.Load() != nil }

// openTalkSession builds the webrtc session a talk uses. It asks for the sub
// stream: its video is thrown away, and the main session keeps serving the
// clients.
func (cs *CameraStream) openTalkSession(onError func(error)) (talkSession, error) {
	var storageManager *storage.StorageManager
	if cs.server != nil {
		storageManager = cs.server.storageManager
	}
	wb := NewWebRTCBridge(cs.camera, "sd", cs.user, storageManager, nil)
	wb.Talkback = true
	if cs.server != nil && cs.server.MobileClient != nil {
		wb.SetMobileClient(cs.server.MobileClient)
	}
	if cs.server != nil && cs.server.mqttManager != nil {
		mqttClient, err := cs.server.mqttManager.GetClient(cs.camera.DeviceID)
		if err != nil {
			wb.Stop()
			wb.cancel() // Stop returns early on a bridge that never started
			return nil, fmt.Errorf("failed to get MQTT client: %v", err)
		}
		wb.SetMQTTClient(mqttClient)
	}
	wb.SetOnError(onError)
	return talkBridge{wb}, nil
}
