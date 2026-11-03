package rtsp

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/lan"
	"avent-webrtc-bridge/pkg/tuya"
)

// An imm session's signalling goes out on two routes, in order:
//
//  1. the moto relay, /av/moto/<motoId>/u/<devId>, which the bridge's webrtc
//     sessions use and which is known to reach the monitor's P2P SDK;
//  2. when the moto has not answered within immMotoAnswerWait, the device's
//     own channel, smart/mb/out/<devId>, which is the route the Philips app
//     uses for these frames (tuya.DeviceChannel). The same offer, with the
//     same session id, goes out again there, followed by the candidates sent
//     so far, and the answer is read on both routes from then on.
//
// The monitor's SDK drops a repeated offer for a session it already has, so a
// late moto answer and a device-channel answer cannot make two sessions.

// immMotoAnswerWait is how long the moto relay gets before the device channel
// is tried too. A webrtc answer through the moto normally comes back within a
// second or two (one cloud round trip and the monitor's SDK). Three seconds
// leaves that margin and still gives the device channel about seven of the
// handshake's ten: about two to connect (user info, device pv, TLS, MQTT),
// one for the answer, and the rest for ICE and the login.
const immMotoAnswerWait = 3 * time.Second

// immDeviceDisconnectWait bounds the device channel's part of an imm
// session's disconnect. Teardown sends it with the stream's lock held; the
// moto's part is bounded like the webrtc session's SendDisconnect
// (tuya.publishTimeout), and the two run side by side, so the lock is held
// no longer than for a webrtc session.
const immDeviceDisconnectWait = time.Second

// The routes, as the log names them.
const (
	viaMoto   = "moto"
	viaDevice = "device channel"
)

// mqttSignaller is lan.Signaller over the cloud. It is also where the
// session's signalling is logged: descriptions only through summarizeSDP, so
// ICE credentials and the aes-key never reach the log, and the local key
// never at all.
type mqttSignaller struct {
	deviceName string
	open       func(sessionID string, onFrame func(json.RawMessage)) immChannel
	// device opens the device channel; nil keeps the session on the moto.
	device   func(onData func(json.RawMessage)) (immChannel, error)
	motoWait time.Duration

	frames    chan lan.SignalFrame
	sessionID string

	mu        sync.Mutex
	ch        immChannel // the moto
	dev       immChannel // the device channel, once open
	sent      []any      // the offer and candidates, replayed on the device channel
	answered  string     // the route that answered first
	escalated bool
	closed    bool
	timer     *time.Timer

	// devMu orders the device channel's publishes: the replay of the offer
	// and earlier candidates goes out before any later frame.
	devMu sync.Mutex
}

var _ lan.Signaller = (*mqttSignaller)(nil)

func (m *mqttSignaller) Open(sessionID string) (<-chan lan.SignalFrame, error) {
	m.frames = make(chan lan.SignalFrame, 64)
	m.sessionID = sessionID
	m.ch = m.open(sessionID, m.receiver(viaMoto))
	return m.frames, nil
}

// Send publishes on the moto, and on the device channel once it is open. Only
// the moto's error is returned: the device channel is the second try, and its
// failures are logged.
func (m *mqttSignaller) Send(frame any) error {
	f, isSignal := frame.(lan.SignalFrame)
	m.mu.Lock()
	if isSignal && f.Header.Type == lan.TypeOffer {
		core.Logger.Info().Msgf("Offer to %s: mode=imm over cloud signalling %s", m.deviceName, summarizeSDP(f.Msg.SDP))
		if m.device != nil && m.timer == nil && !m.closed {
			m.timer = time.AfterFunc(m.motoWait, m.escalate)
		}
	}
	if isSignal {
		m.sent = append(m.sent, frame)
	}
	ch, dev := m.ch, m.dev
	m.mu.Unlock()

	if _, isDisconnect := frame.(lan.DisconnectFrame); isDisconnect && dev != nil {
		return m.sendDisconnect(ch, dev, frame)
	}
	err := ch.Publish(frame)
	if dev != nil {
		m.devMu.Lock()
		if derr := dev.Publish(frame); derr != nil {
			core.Logger.Debug().Err(derr).Msgf("imm frame for %s not sent on the device channel", m.deviceName)
		}
		m.devMu.Unlock()
	}
	return err
}

// sendDisconnect ends the session on both routes at once: the moto as the
// webrtc session does, the device channel within immDeviceDisconnectWait.
func (m *mqttSignaller) sendDisconnect(ch, dev immChannel, frame any) error {
	deadline := time.Now().Add(immDeviceDisconnectWait)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.devMu.Lock()
		defer m.devMu.Unlock()
		if err := publishWithin(dev, frame, immDeviceDisconnectWait); err != nil {
			core.Logger.Debug().Err(err).Msgf("imm disconnect for %s not sent on the device channel", m.deviceName)
		}
	}()
	err := ch.Publish(frame)
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
	}
	return err
}

// publishWithin bounds a device-channel publish where the channel allows it.
func publishWithin(ch immChannel, frame any, wait time.Duration) error {
	if b, ok := ch.(interface {
		PublishWithin(any, time.Duration) error
	}); ok {
		return b.PublishWithin(frame, wait)
	}
	return ch.Publish(frame)
}

// escalate opens the device channel when the moto has not answered, and
// replays the session's frames on it.
func (m *mqttSignaller) escalate() {
	m.mu.Lock()
	if m.answered != "" || m.closed || m.escalated {
		m.mu.Unlock()
		return
	}
	m.escalated = true
	m.mu.Unlock()

	core.Logger.Info().Msgf("No answer from %s via moto in %v; sending the offer on the device channel too",
		m.deviceName, m.motoWait)
	dev, err := m.device(m.receiver(viaDevice))
	if err != nil {
		core.Logger.Warn().Msgf("Device channel for %s unavailable: %v", m.deviceName, err)
		return
	}

	m.devMu.Lock()
	defer m.devMu.Unlock()
	m.mu.Lock()
	// The dial takes a second or two: the session may have ended meanwhile,
	// or the moto answered late. Either way the offer must not go out again.
	if m.closed || m.answered != "" {
		m.mu.Unlock()
		dev.Close()
		return
	}
	m.dev = dev
	replay := append([]any(nil), m.sent...)
	m.mu.Unlock()
	for _, frame := range replay {
		if err := dev.Publish(frame); err != nil {
			core.Logger.Warn().Msgf("Could not send the imm offer to %s on the device channel: %v", m.deviceName, err)
			return
		}
	}
}

func (m *mqttSignaller) Close() error {
	m.mu.Lock()
	m.closed = true
	if m.timer != nil {
		m.timer.Stop()
	}
	ch, dev := m.ch, m.dev
	m.mu.Unlock()
	if ch != nil {
		ch.Close()
	}
	if dev != nil {
		dev.Close()
	}
	return nil
}

// receiver reads one route's frames, on its broker's dispatch goroutine,
// which every camera's signalling shares on the moto route (MQTTManager). It
// never blocks there: a short lock, and a send that drops rather than waits.
// The answer and the candidates are queued here, in arrival order. A
// disconnect is queued too, and acted on by the session's own goroutine
// (lan.Client's pump), which is where the stream's lock is taken. The device
// channel carries every 302 frame for the monitor, the phone app's sessions
// included, so only this session's frames are kept.
func (m *mqttSignaller) receiver(via string) func(json.RawMessage) {
	return func(data json.RawMessage) {
		f, err := lan.ParseSignalFrame(data)
		if err != nil {
			core.Logger.Debug().Err(err).Msgf("Unreadable imm frame from %s via %s", m.deviceName, via)
			return
		}
		if f.Header.SessionID != m.sessionID {
			return
		}
		switch f.Header.Type {
		case lan.TypeAnswer:
			m.mu.Lock()
			first := m.answered == ""
			if first {
				m.answered = via
				if m.timer != nil {
					m.timer.Stop()
				}
			}
			m.mu.Unlock()
			if first {
				core.Logger.Info().Msgf("Answer from %s via %s: %s", m.deviceName, via, summarizeSDP(f.Msg.SDP))
			}
		case lan.TypeDisconnect:
			core.Logger.Info().Msgf("Disconnect from %s on the imm session via %s", m.deviceName, via)
		}
		select {
		case m.frames <- f:
		default:
			core.Logger.Warn().Msgf("Dropped an imm %s frame from %s: the session is not reading", f.Header.Type, m.deviceName)
		}
	}
}

// deviceChannelTag replaces the shared connection's client id suffix, so the
// device channel is a connection of its own (the app tags its connections the
// same way: <package>_mb_<phone>_<uid hash>_<tag>). The id names the account,
// not the camera: every camera's imm session shares the one connection
// (tuya.DeviceChannelPool), because a broker drops the older connection when
// a newer one arrives under the same id.
const deviceChannelTag = "_IMM"

// deviceChannelClientID derives the device channel's client id from the
// shared connection's.
func deviceChannelClientID(shared string) string {
	return strings.TrimSuffix(shared, "_DEFAULT") + deviceChannelTag
}

// devicePV reads the device's MQTT protocol version from tuya.m.device.get.
// Empty when the call fails or the field is missing: tuya.UseDevicePV then
// picks 2.2.
func devicePV(mc *tuya.MobileSDKClient, deviceID string) string {
	raw, err := mc.GetDeviceInfo(deviceID)
	if err != nil {
		return ""
	}
	var dev struct {
		PV string `json:"pv"`
	}
	if json.Unmarshal(raw, &dev) != nil {
		return ""
	}
	return dev.PV
}

// dialIMMDeviceChannel opens a session on the account's device channel, with
// the account's mobile MQTT identity, as the shared webrtc connection does,
// under a client id of its own: a broker that refuses this publish can then
// only drop the device channel, never the connection every camera's webrtc
// session shares. The connection is dialled by the first camera that needs it
// and closed when the last one lets go.
func dialIMMDeviceChannel(mc *tuya.MobileSDKClient, deviceID, localKey string, onData func(json.RawMessage)) (immChannel, error) {
	info, err := mc.GetUserInfo()
	if err != nil {
		return nil, fmt.Errorf("user info: %w", err)
	}
	uid := info.ID
	if uid == "" {
		uid = mc.UID
	}
	pv := devicePV(mc, deviceID)
	core.Logger.Debug().Msgf("Device channel for %s: pv %q, framing %s", deviceID, pv, tuya.UseDevicePV(pv))
	dc, err := tuya.DialDeviceChannel(tuya.DeviceChannelConfig{
		MQTT: tuya.MobileMQTTConfig{
			Username:  mc.DeriveMQTTUsername(mc.SID, mc.Ecode, mc.PartnerIdentity),
			Password:  mc.DeriveMQTTConfig(mc.Ecode).Password,
			ClientID:  deviceChannelClientID(mc.DeriveMQTTClientID(uid)),
			UID:       uid,
			BrokerURL: fmt.Sprintf("ssl://%s:8883", info.Domain.MobileMqttsUrl),
		},
		DeviceID: deviceID,
		LocalKey: localKey,
		PV:       pv,
		OnData:   onData,
	})
	if err != nil {
		return nil, err
	}
	return dc, nil
}
