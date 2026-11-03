package rtsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/lan"
	"avent-webrtc-bridge/pkg/tuya"
)

// An imm session over cloud signalling: the app's own transport (AES/KCP)
// with its offer, answer and candidates carried by the cloud broker, and the
// media of the LAN path. It is for monitors that cannot take a LAN offer
// (Tuya LAN 3.3), and it is opt-in and experimental: the Dev card's
// cloud_transport option. See WHITEPAPER section 21 and pkg/lan/cloud.go.
//
// Issue #72: the session sends what the app's live view sends (pkg/lan's
// startup sequence, StartPreview without the talk command 8) and no 312
// speaker request. That request is a webrtc-mode control with no imm
// counterpart; on imm the speaker stays closed because nothing asks for it.
// The audio check and the per-session counts are kept, as on webrtc.

const (
	// immHandshakeTimeout bounds the cloud round trip, the monitor's
	// candidate gathering, ICE and the login. The first frame then has
	// lanFirstFrameDeadline, as on the LAN.
	immHandshakeTimeout = 8 * time.Second
	// immAttemptDeadline bounds a whole attempt: the cloud calls before the
	// offer, the handshake and the first frame. It is how long a viewer can
	// wait before the webrtc fallback starts.
	immAttemptDeadline = 2*time.Second + immHandshakeTimeout + lanFirstFrameDeadline
	// immAudioCheckDelay is when an imm session says whether audio flows.
	immAudioCheckDelay = 10 * time.Second
)

// Why an imm session that logged in delivered no usable video.
var (
	errIMMSilent     = errors.New("no reply to the login: refused, or the media path works one way only")
	errIMMNoMedia    = errors.New("the monitor answered the login but sent no media")
	errIMMNoVideo    = errors.New("audio arrives but no video")
	errIMMNoKeyframe = errors.New("video arrives but no H.264 SPS or IDR")
	// errIMMCloud is a failed cloud call before anything was sent.
	errIMMCloud = errors.New("cloud setup")
)

// immCloudAPI is the part of the mobile API client an imm session needs.
type immCloudAPI interface {
	P2PPreLink(deviceID string) error
	GetWebRTCConfig(deviceID string) (*tuya.WebRTCConfigResponse, error)
	RTCSessionInit(deviceID string) error
}

// immChannel is one imm session on the broker (tuya.IMMSession).
type immChannel interface {
	Publish(frame any) error
	Close()
}

// immBroker is what an imm session needs from the MQTT client.
type immBroker struct {
	uid  string
	open func(motoID, deviceID, sessionID string, onFrame func(json.RawMessage)) immChannel
}

func brokerOf(c *tuya.MQTTClient) immBroker {
	return immBroker{
		uid: c.UID(),
		open: func(motoID, deviceID, sessionID string, onFrame func(json.RawMessage)) immChannel {
			return c.OpenIMMSession(motoID, deviceID, sessionID, onFrame)
		},
	}
}

// immSource is a LANSource whose signalling goes through the cloud.
type immSource struct {
	*LANSource
	deviceName string
	control    atomic.Uint64 // bytes the control conversation delivered after the login
	live       atomic.Bool   // Start succeeded
	done       chan struct{} // closed by Stop
	stopOnce   sync.Once
}

var _ lanStreamer = (*immSource)(nil)

// newIMMSource is the default immFactory. Callers hold cs.mutex; nothing
// touches the network until Start.
func (cs *CameraStream) newIMMSource(forwarder *RTPForwarder) lanStreamer {
	s := cs.server
	cloud := immCloud{deviceID: cs.camera.DeviceID, deviceName: cs.camera.DeviceName}
	if s.MobileClient != nil {
		cloud.api = s.MobileClient
	}
	if s.mqttManager != nil {
		cloud.broker = func(deviceID string) (immBroker, error) {
			c, err := s.mqttManager.GetClient(deviceID)
			if err != nil {
				return immBroker{}, err
			}
			return brokerOf(c), nil
		}
	}
	// The integration's LAN credentials and address, when it published them,
	// are only a fallback and a hint here: the cloud answers for both.
	return newIMMSource(cs.camera.DeviceID, cs.camera.DeviceName, s.LANTargets[cs.camera.DeviceID], forwarder, cloud)
}

func newIMMSource(deviceID, deviceName string, target LANTarget, forwarder *RTPForwarder, cloud immCloud) *immSource {
	src := &immSource{
		LANSource:  NewLANSource(deviceID, deviceName, "", target, forwarder),
		deviceName: deviceName,
		done:       make(chan struct{}),
	}
	src.frameWait = lanFirstFrameDeadline
	src.stall = src.stallReason
	src.prepare = func(ctx context.Context, cfg *lan.ClientConfig) error {
		return cloud.prepare(ctx, cfg, func(n int) { src.control.Add(uint64(n)) })
	}
	return src
}

// Start runs the handshake and waits for the first frame, as a LAN source does.
func (s *immSource) Start(ctx context.Context) error {
	if err := s.LANSource.Start(ctx); err != nil {
		return err
	}
	s.live.Store(true)
	go s.checkAudio(s.audioFrames.Load(), immAudioCheckDelay)
	return nil
}

// Stop ends the session and, for one that streamed, logs what it carried, the
// way a webrtc session does (issue #72: does audio keep flowing?).
func (s *immSource) Stop() {
	s.LANSource.Stop()
	s.stopOnce.Do(func() {
		close(s.done)
		if s.live.Load() {
			core.Logger.Info().Msgf("imm session for %s carried %d video and %d audio messages",
				s.deviceName, s.videoFrames.Load(), s.audioFrames.Load())
		}
	})
}

// checkAudio says, once, whether the session carries audio. An always-on
// consumer such as Frigate never reaches the count logged at Stop.
func (s *immSource) checkAudio(before uint64, after time.Duration) {
	select {
	case <-s.done:
		return
	case <-time.After(after):
	}
	if n := s.audioFrames.Load() - before; n > 0 {
		core.Logger.Info().Msgf("Audio from %s flows on the imm session: %d messages in %v", s.deviceName, n, after)
		return
	}
	core.Logger.Warn().Msgf("No audio from %s on the imm session after %v", s.deviceName, after)
}

// stallReason names the step a logged-in session stopped at.
func (s *immSource) stallReason() error {
	switch {
	case s.videoFrames.Load() > 0:
		return errIMMNoKeyframe
	case s.audioFrames.Load() > 0:
		return errIMMNoVideo
	case s.control.Load() > 0:
		return errIMMNoMedia
	default:
		return errIMMSilent
	}
}

// immCloud fetches what an imm session needs from the cloud.
type immCloud struct {
	deviceID, deviceName string
	api                  immCloudAPI
	broker               func(deviceID string) (immBroker, error)
}

// prepare fills the client config: the moto relay, the ICE servers, the media
// credentials and the signaller. The calls are the webrtc session's (pre-link,
// rtc.config.get, session init), so the cloud sees the same sequence. It
// returns when ctx ends even if a call does not.
func (ic immCloud) prepare(ctx context.Context, cfg *lan.ClientConfig, onControl func(int)) error {
	if ic.api == nil || ic.broker == nil {
		return fmt.Errorf("%w: no cloud client", errIMMCloud)
	}
	type result struct {
		rtc    *tuya.WebRTCConfig
		broker immBroker
		err    error
	}
	done := make(chan result, 1)
	go func() {
		if err := ic.api.P2PPreLink(ic.deviceID); err != nil {
			core.Logger.Debug().Err(err).Msgf("P2P pre-link for %s failed (imm, non-fatal)", ic.deviceName)
		}
		resp, err := ic.api.GetWebRTCConfig(ic.deviceID)
		if err != nil {
			done <- result{err: fmt.Errorf("%w: rtc.config.get: %v", errIMMCloud, err)}
			return
		}
		if err := ic.api.RTCSessionInit(ic.deviceID); err != nil {
			core.Logger.Debug().Err(err).Msgf("RTC session init for %s failed (imm, non-fatal)", ic.deviceName)
		}
		b, err := ic.broker(ic.deviceID)
		if err != nil {
			done <- result{err: fmt.Errorf("%w: MQTT: %v", errIMMCloud, err)}
			return
		}
		done <- result{rtc: &resp.Result, broker: b}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", errIMMCloud, ctx.Err())
	}
	if r.err != nil {
		return r.err
	}

	moto := r.rtc.MotoId
	if moto == "" {
		moto = r.rtc.P2PConfig.MotoId
	}
	if moto == "" {
		return fmt.Errorf("%w: rtc.config.get named no moto relay", errIMMCloud)
	}
	// The cloud's values win: they are current. Whichever is used is never logged.
	if r.rtc.LocalKey != "" {
		cfg.LocalKey = r.rtc.LocalKey
	}
	if r.rtc.Password != "" {
		cfg.Password = r.rtc.Password
	}
	if cfg.LocalKey == "" || cfg.Password == "" {
		return fmt.Errorf("%w: no local key or P2P password for the media login", errIMMCloud)
	}
	cfg.UID = r.broker.uid // the account the broker delivers the answer to
	cfg.MotoID = moto
	cfg.ICEServers = lanICEServers(r.rtc.P2PConfig.Ices)
	cfg.HandshakeTimeout = immHandshakeTimeout
	cfg.OnControl = onControl
	cfg.Signaller = &mqttSignaller{
		deviceName: ic.deviceName,
		open: func(sessionID string, onFrame func(json.RawMessage)) immChannel {
			return r.broker.open(moto, ic.deviceID, sessionID, onFrame)
		},
	}
	core.Logger.Debug().Msgf("imm session for %s: %d ICE servers, address hint %t",
		ic.deviceName, len(cfg.ICEServers), cfg.IP != "")
	return nil
}

// lanICEServers turns rtc.config.get's ICE servers into the offer's token.
func lanICEServers(in []tuya.ICEServer) []lan.ICEServer {
	out := make([]lan.ICEServer, 0, len(in))
	for _, s := range in {
		out = append(out, lan.ICEServer{URLs: s.Urls, Username: s.Username, Credential: s.Credential, TTL: s.Ttl})
	}
	return out
}

// mqttSignaller is lan.Signaller over the cloud broker. It is also where the
// session's signalling is logged: the descriptions only through summarizeSDP,
// so ICE credentials and the aes-key never reach the log.
type mqttSignaller struct {
	deviceName string
	open       func(sessionID string, onFrame func(json.RawMessage)) immChannel

	ch     immChannel
	frames chan lan.SignalFrame
}

var _ lan.Signaller = (*mqttSignaller)(nil)

func (m *mqttSignaller) Open(sessionID string) (<-chan lan.SignalFrame, error) {
	m.frames = make(chan lan.SignalFrame, 64)
	m.ch = m.open(sessionID, m.receive)
	return m.frames, nil
}

func (m *mqttSignaller) Send(frame any) error {
	if f, ok := frame.(lan.SignalFrame); ok && f.Header.Type == lan.TypeOffer {
		core.Logger.Info().Msgf("Offer to %s: mode=imm over cloud signalling %s", m.deviceName, summarizeSDP(f.Msg.SDP))
	}
	return m.ch.Publish(frame)
}

func (m *mqttSignaller) Close() error {
	if m.ch != nil {
		m.ch.Close()
	}
	return nil
}

// receive runs on the broker's goroutine.
func (m *mqttSignaller) receive(data json.RawMessage) {
	f, err := lan.ParseSignalFrame(data)
	if err != nil {
		core.Logger.Debug().Err(err).Msgf("Unreadable imm frame from %s", m.deviceName)
		return
	}
	switch f.Header.Type {
	case lan.TypeAnswer:
		core.Logger.Info().Msgf("Answer from %s: %s", m.deviceName, summarizeSDP(f.Msg.SDP))
	case lan.TypeDisconnect:
		core.Logger.Info().Msgf("Disconnect from %s on the imm session", m.deviceName)
	}
	select {
	case m.frames <- f:
	default:
		core.Logger.Warn().Msgf("Dropped an imm %s frame from %s: the session is not reading", f.Header.Type, m.deviceName)
	}
}
