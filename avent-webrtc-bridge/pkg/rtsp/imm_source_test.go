package rtsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/lan"
	"avent-webrtc-bridge/pkg/tuya"
)

// fakeCloudAPI answers rtc.config.get with rtc, or fails with err.
type fakeCloudAPI struct {
	rtc   tuya.WebRTCConfig
	err   error
	block chan struct{} // when set, GetWebRTCConfig waits for it
	calls []string
	mu    sync.Mutex
}

func (f *fakeCloudAPI) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeCloudAPI) P2PPreLink(string) error     { f.record("prelink"); return nil }
func (f *fakeCloudAPI) RTCSessionInit(string) error { f.record("init"); return nil }
func (f *fakeCloudAPI) GetWebRTCConfig(string) (*tuya.WebRTCConfigResponse, error) {
	f.record("config")
	if f.block != nil {
		<-f.block
	}
	if f.err != nil {
		return nil, f.err
	}
	return &tuya.WebRTCConfigResponse{Result: f.rtc, Success: true}, nil
}

// fakeChannel records what a session publishes.
type fakeChannel struct {
	mu        sync.Mutex
	published []any
	closed    bool
	motoID    string
	deviceID  string
	sessionID string
	onFrame   func(json.RawMessage)
}

func (c *fakeChannel) Publish(frame any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.published = append(c.published, frame)
	return nil
}

func (c *fakeChannel) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func fakeBroker(ch *fakeChannel) func(string) (immBroker, error) {
	return func(string) (immBroker, error) {
		return immBroker{
			uid: "cloud-uid",
			open: func(motoID, deviceID, sessionID string, onFrame func(json.RawMessage)) immChannel {
				ch.motoID, ch.deviceID, ch.sessionID, ch.onFrame = motoID, deviceID, sessionID, onFrame
				return ch
			},
		}, nil
	}
}

func testRTCConfig() tuya.WebRTCConfig {
	return tuya.WebRTCConfig{
		MotoId:   "moto-1",
		LocalKey: "fake-cloud-key",
		Password: "fake-cloud-pass",
		P2PConfig: tuya.P2PConfig{Ices: []tuya.ICEServer{
			{Urls: "stun:stun.example:3478"},
			{Urls: "turn:turn.example:3478", Username: "u", Credential: "c", Ttl: 600},
		}},
	}
}

func TestIMMPrepareFillsTheClientConfig(t *testing.T) {
	api := &fakeCloudAPI{rtc: testRTCConfig()}
	ch := &fakeChannel{}
	cloud := immCloud{deviceID: "testdev", deviceName: "Test Cam", api: api, broker: fakeBroker(ch)}
	cfg := lan.ClientConfig{DeviceID: "testdev", LocalKey: "fake-old-key", Password: "fake-old-pass", IP: "192.0.2.10"}

	if err := cloud.prepare(context.Background(), &cfg, func(int) {}); err != nil {
		t.Fatal(err)
	}
	if cfg.MotoID != "moto-1" || cfg.UID != "cloud-uid" || cfg.HandshakeTimeout != immHandshakeTimeout {
		t.Errorf("moto %q, uid %q, timeout %v", cfg.MotoID, cfg.UID, cfg.HandshakeTimeout)
	}
	if cfg.LocalKey != "fake-cloud-key" || cfg.Password != "fake-cloud-pass" {
		t.Error("the cloud's media credentials must win over the integration's")
	}
	if len(cfg.ICEServers) != 2 || cfg.ICEServers[1].URLs != "turn:turn.example:3478" || cfg.ICEServers[1].Credential != "c" {
		t.Errorf("ICE servers = %+v", cfg.ICEServers)
	}
	if cfg.IP != "192.0.2.10" || cfg.OnControl == nil || cfg.Signaller == nil {
		t.Errorf("hint %q, OnControl set %t, Signaller set %t", cfg.IP, cfg.OnControl != nil, cfg.Signaller != nil)
	}
	if want := []string{"prelink", "config", "init"}; len(api.calls) != 3 || api.calls[0] != want[0] || api.calls[1] != want[1] || api.calls[2] != want[2] {
		t.Errorf("cloud calls = %v, want the webrtc session's %v", api.calls, want)
	}

	// The signaller opens the session on the moto the cloud named.
	if _, err := cfg.Signaller.Open("sess-1"); err != nil {
		t.Fatal(err)
	}
	if ch.motoID != "moto-1" || ch.deviceID != "testdev" || ch.sessionID != "sess-1" {
		t.Errorf("session opened on %q/%q/%q", ch.motoID, ch.deviceID, ch.sessionID)
	}
}

func TestIMMPrepareFallsBackToTheIntegrationsCredentials(t *testing.T) {
	rtc := testRTCConfig()
	rtc.LocalKey, rtc.Password = "", ""
	cloud := immCloud{deviceID: "testdev", api: &fakeCloudAPI{rtc: rtc}, broker: fakeBroker(&fakeChannel{})}
	cfg := lan.ClientConfig{LocalKey: "fake-old-key", Password: "fake-old-pass"}
	if err := cloud.prepare(context.Background(), &cfg, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.LocalKey != "fake-old-key" || cfg.Password != "fake-old-pass" {
		t.Error("with nothing from the cloud, the integration's credentials must be used")
	}

	cfg = lan.ClientConfig{}
	if err := cloud.prepare(context.Background(), &cfg, nil); !errors.Is(err, errIMMCloud) {
		t.Errorf("no credentials anywhere: error %v, want errIMMCloud", err)
	}
}

func TestIMMPrepareFailures(t *testing.T) {
	noMoto := testRTCConfig()
	noMoto.MotoId = ""
	cases := map[string]immCloud{
		"no cloud client": {},
		"api error":       {api: &fakeCloudAPI{err: errors.New("PERMISSION_DENIED")}, broker: fakeBroker(&fakeChannel{})},
		"no moto":         {api: &fakeCloudAPI{rtc: noMoto}, broker: fakeBroker(&fakeChannel{})},
		"no mqtt": {api: &fakeCloudAPI{rtc: testRTCConfig()}, broker: func(string) (immBroker, error) {
			return immBroker{}, errors.New("broker down")
		}},
	}
	for name, cloud := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := lan.ClientConfig{LocalKey: "fake-old-key", Password: "fake-old-pass"}
			if err := cloud.prepare(context.Background(), &cfg, nil); !errors.Is(err, errIMMCloud) {
				t.Fatalf("error = %v, want errIMMCloud", err)
			}
			if cfg.Signaller != nil {
				t.Fatal("a failed prepare must not leave a signaller")
			}
		})
	}
}

// A cloud call that hangs does not hold the attempt past its deadline.
func TestIMMPrepareHonoursTheDeadline(t *testing.T) {
	api := &fakeCloudAPI{rtc: testRTCConfig(), block: make(chan struct{})}
	defer close(api.block)
	cloud := immCloud{api: api, broker: fakeBroker(&fakeChannel{})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := cloud.prepare(ctx, &lan.ClientConfig{}, nil)
	if !errors.Is(err, errIMMCloud) || time.Since(start) > time.Second {
		t.Fatalf("error %v after %v, want errIMMCloud at the deadline", err, time.Since(start))
	}
}

func TestMQTTSignallerRoutesTheMonitorsFrames(t *testing.T) {
	ch := &fakeChannel{}
	sig := &mqttSignaller{deviceName: "Test Cam", open: func(sessionID string, onFrame func(json.RawMessage)) immChannel {
		ch.sessionID, ch.onFrame = sessionID, onFrame
		return ch
	}}
	frames, err := sig.Open("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	ch.onFrame(json.RawMessage(`{"header":{"type":"candidate","sessionid":"sess-1"},"msg":{"candidate":"a=candidate:1 1 UDP 1 192.0.2.10 5000 typ host"}}`))
	ch.onFrame(json.RawMessage(`not json`))
	select {
	case f := <-frames:
		if f.Header.Type != lan.TypeCandidate || f.Msg.Candidate == "" {
			t.Errorf("frame = %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("no frame delivered")
	}
	select {
	case f := <-frames:
		t.Fatalf("an unreadable frame was delivered: %+v", f)
	default:
	}

	offer := lan.SignalFrame{Header: lan.SignalHeader{Type: lan.TypeOffer}, Msg: lan.SignalMessage{SDP: "v=0\r\nm=application 9 imm 6001\r\na=aes-key:00112233445566778899aabbccddeeff\r\n"}}
	if err := sig.Send(offer); err != nil {
		t.Fatal(err)
	}
	if len(ch.published) != 1 {
		t.Fatalf("published %d frames, want 1", len(ch.published))
	}
	_ = sig.Close()
	if !ch.closed {
		t.Error("Close must close the broker session")
	}
}

// fakeIMMClient stands in for lan.Client on the imm path.
type fakeIMMClient struct {
	cfg   lan.ClientConfig
	start func(cfg lan.ClientConfig) error
}

func (f *fakeIMMClient) Start(context.Context) error { return f.start(f.cfg) }
func (f *fakeIMMClient) Close() error                { return nil }

func newTestIMMSource(t *testing.T, start func(cfg lan.ClientConfig) error) (*immSource, *fakeIMMClient) {
	t.Helper()
	api := &fakeCloudAPI{rtc: testRTCConfig()}
	cloud := immCloud{deviceID: "testdev", deviceName: "Test Cam", api: api, broker: fakeBroker(&fakeChannel{})}
	src := newIMMSource("testdev", "Test Cam", LANTarget{IP: "192.0.2.10", Protocol: "3.3"}, NewRTPForwarder(), cloud)
	src.frameWait = 100 * time.Millisecond
	fake := &fakeIMMClient{start: start}
	src.newClient = func(cfg lan.ClientConfig) lanClient {
		fake.cfg = cfg
		return fake
	}
	t.Cleanup(src.Stop)
	return src, fake
}

func TestIMMSourceStartsOnFirstFrame(t *testing.T) {
	src, fake := newTestIMMSource(t, func(cfg lan.ClientConfig) error {
		go cfg.OnVideo(testIDR)
		return nil
	})
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if fake.cfg.Signaller == nil || fake.cfg.MotoID != "moto-1" || fake.cfg.IP != "192.0.2.10" {
		t.Errorf("client config: signaller %t, moto %q, hint %q", fake.cfg.Signaller != nil, fake.cfg.MotoID, fake.cfg.IP)
	}
	if !src.live.Load() {
		t.Error("a started source must count as live")
	}
}

// A session that logged in and then failed to deliver says how far it got.
func TestIMMSourceNamesTheStall(t *testing.T) {
	cases := map[string]struct {
		feed func(cfg lan.ClientConfig)
		want error
	}{
		"silent":   {func(lan.ClientConfig) {}, errIMMSilent},
		"no media": {func(cfg lan.ClientConfig) { cfg.OnControl(64) }, errIMMNoMedia},
		"audio only": {func(cfg lan.ClientConfig) {
			cfg.OnAudio(&lan.AudioFrame{SampleRate: 16000, Channels: 1, Samples: make([]byte, 640)})
		}, errIMMNoVideo},
		"no keyframe": {func(cfg lan.ClientConfig) {
			cfg.OnVideo(&lan.VideoFrame{Width: 1280, Height: 720, FPS: 20, NAL: []byte{0x41, 0x9a, 0x00}})
		}, errIMMNoKeyframe},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			src, _ := newTestIMMSource(t, func(cfg lan.ClientConfig) error {
				tc.feed(cfg)
				return nil
			})
			err := src.Start(context.Background())
			if !errors.Is(err, errNoFirstFrame) || !errors.Is(err, tc.want) {
				t.Fatalf("Start error = %v, want errNoFirstFrame and %v", err, tc.want)
			}
			if src.live.Load() {
				t.Error("a failed source must not count as live")
			}
		})
	}
}

func TestIMMSourceReportsACloudFailure(t *testing.T) {
	src := newIMMSource("testdev", "Test Cam", LANTarget{}, NewRTPForwarder(), immCloud{})
	t.Cleanup(src.Stop)
	if err := src.Start(context.Background()); !errors.Is(err, errIMMCloud) {
		t.Fatalf("Start error = %v, want errIMMCloud", err)
	}
}

// The device channel, when there is one, gets the media login's local key:
// the app frames that channel with the same key.
func TestIMMPrepareWiresTheDeviceChannel(t *testing.T) {
	var gotDevice, gotKey string
	cloud := immCloud{
		deviceID: "testdev", deviceName: "Test Cam",
		api:    &fakeCloudAPI{rtc: testRTCConfig()},
		broker: fakeBroker(&fakeChannel{}),
		deviceChannel: func(deviceID, localKey string, _ func(json.RawMessage)) (immChannel, error) {
			gotDevice, gotKey = deviceID, localKey
			return &fakeChannel{}, nil
		},
	}
	cfg := lan.ClientConfig{LocalKey: "fake-old-key", Password: "fake-old-pass"}
	if err := cloud.prepare(context.Background(), &cfg, nil); err != nil {
		t.Fatal(err)
	}
	sig, ok := cfg.Signaller.(*mqttSignaller)
	if !ok || sig.device == nil || sig.motoWait != immMotoAnswerWait {
		t.Fatalf("signaller = %+v, want a device channel after %v", cfg.Signaller, immMotoAnswerWait)
	}
	if _, err := sig.device(func(json.RawMessage) {}); err != nil {
		t.Fatal(err)
	}
	if gotDevice != "testdev" || gotKey != "fake-cloud-key" {
		t.Errorf("device channel opened for %q with the wrong key", gotDevice)
	}

	// Without an opener the session stays on the moto.
	cloud.deviceChannel = nil
	cfg = lan.ClientConfig{LocalKey: "fake-old-key", Password: "fake-old-pass"}
	if err := cloud.prepare(context.Background(), &cfg, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Signaller.(*mqttSignaller).device != nil {
		t.Error("no opener, yet a device channel")
	}
}

// The review's probe (M2): the attempt deadline, not the handshake timeout,
// ends a start that waits for an answer. The log must still say "no answer".
func TestIMMAttemptDeadlineKeepsTheStep(t *testing.T) {
	cloud := immCloud{deviceID: "testdev", deviceName: "Test Cam", api: &fakeCloudAPI{rtc: testRTCConfig()}, broker: fakeBroker(&fakeChannel{})}
	src := newIMMSource("testdev", "Test Cam", LANTarget{}, NewRTPForwarder(), cloud) // the real lan.Client
	t.Cleanup(src.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := src.Start(ctx)
	if err == nil {
		t.Fatal("a start nobody answers must fail")
	}
	if got := immStep(err); got != "no answer" {
		t.Fatalf("immStep = %q for %v, want \"no answer\"", got, err)
	}
}

// Media that opens but never parses runs into the client's idle timeout
// before the first-frame wait: that is "no media after login" (M2).
func TestIMMMediaIdleIsNamed(t *testing.T) {
	src, _ := newTestIMMSource(t, func(cfg lan.ClientConfig) error {
		go cfg.OnClosed(fmt.Errorf("lan: %w for 3s: i/o timeout", lan.ErrMediaIdle))
		return nil
	})
	src.frameWait = 2 * time.Second
	err := src.Start(context.Background())
	if got := immStep(err); got != "no media after login" {
		t.Fatalf("immStep = %q for %v", got, err)
	}
}
