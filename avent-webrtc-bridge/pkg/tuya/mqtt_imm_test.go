package tuya

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// ackedToken is a publish the broker acknowledged at once.
type ackedToken struct{ mqtt.Token }

func (ackedToken) Wait() bool                     { return true }
func (ackedToken) WaitTimeout(time.Duration) bool { return true }
func (ackedToken) Error() error                   { return nil }

// recordingBroker keeps every publish.
type recordingBroker struct {
	mqtt.Client
	mu       sync.Mutex
	topics   []string
	payloads [][]byte
}

func (b *recordingBroker) Publish(topic string, _ byte, _ bool, payload interface{}) mqtt.Token {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.topics = append(b.topics, topic)
	b.payloads = append(b.payloads, payload.([]byte))
	return ackedToken{}
}

// An imm frame goes out on the webrtc client's topic, inside the same 302
// envelope, with the header pkg/lan built plus the envelope fields every
// frame this bridge sends through the moto has carried.
func TestIMMSessionPublishesTheFrameInTheEnvelope(t *testing.T) {
	broker := &recordingBroker{}
	c := &MQTTClient{mqtt: broker, uid: "u1"}
	s := c.OpenIMMSession("moto-1", "dev-1", "sess-1", nil)
	defer s.Close()

	frame := map[string]any{
		"header": map[string]any{"type": "offer", "sessionid": "sess-1", "path": "mqtt", "p2p_skill": 1635},
		"msg":    map[string]any{"sdp": "v=0", "preconnect": true, "token": []any{}},
	}
	if err := s.Publish(frame); err != nil {
		t.Fatal(err)
	}
	if len(broker.topics) != 1 || broker.topics[0] != "/av/moto/moto-1/u/dev-1" {
		t.Fatalf("topics = %v", broker.topics)
	}
	var got struct {
		Protocol int    `json:"protocol"`
		Pv       string `json:"pv"`
		T        int64  `json:"t"`
		Data     struct {
			Header map[string]any `json:"header"`
			Msg    map[string]any `json:"msg"`
		} `json:"data"`
	}
	if err := json.Unmarshal(broker.payloads[0], &got); err != nil {
		t.Fatal(err)
	}
	if got.Protocol != 302 || got.Pv != "2.2" || got.T == 0 {
		t.Errorf("envelope = %d %q %d", got.Protocol, got.Pv, got.T)
	}
	h := got.Data.Header
	want := map[string]any{"type": "offer", "sessionid": "sess-1", "path": "mqtt", "p2p_skill": float64(1635),
		"sub_dev_id": "", "tid": "", "seq": float64(0), "rtx": float64(0)}
	for k, v := range want {
		if h[k] != v {
			t.Errorf("header[%s] = %v, want %v", k, h[k], v)
		}
	}
	if got.Data.Msg["sdp"] != "v=0" || got.Data.Msg["preconnect"] != true {
		t.Errorf("msg = %v", got.Data.Msg)
	}
	if _, ok := got.Data.Msg["mode"]; ok {
		t.Errorf("msg carries a mode: %v", got.Data.Msg)
	}
}

// Frames for an imm session reach its callback whole; webrtc sessions and
// closed imm sessions are left alone.
func TestIMMSessionReceivesItsFrames(t *testing.T) {
	c := &MQTTClient{}
	var mu sync.Mutex
	var got []json.RawMessage
	s := c.OpenIMMSession("moto-1", "dev-1", "sess-1", func(data json.RawMessage) {
		mu.Lock()
		got = append(got, data)
		mu.Unlock()
	})
	answer := []byte(`{"protocol":302,"pv":"2.2","t":1,"data":{"header":{"type":"answer","sessionid":"sess-1",` +
		`"p2p_skill":1123,"path":"mqtt"},"msg":{"sdp":"v=0\r\na=aes-key:00"}}}`)
	c.consume(nil, fakeMessage{payload: answer})
	other := []byte(`{"protocol":302,"data":{"header":{"type":"answer","sessionid":"other"},"msg":{}}}`)
	c.consume(nil, fakeMessage{payload: other})

	mu.Lock()
	if len(got) != 1 {
		t.Fatalf("delivered %d frames, want 1", len(got))
	}
	var f struct {
		Header struct {
			Type     string `json:"type"`
			P2PSkill int    `json:"p2p_skill"`
		} `json:"header"`
		Msg struct {
			SDP string `json:"sdp"`
		} `json:"msg"`
	}
	if err := json.Unmarshal(got[0], &f); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	if f.Header.Type != "answer" || f.Header.P2PSkill != 1123 || f.Msg.SDP != "v=0\r\na=aes-key:00" {
		t.Errorf("frame = %+v", f)
	}

	s.Close()
	c.consume(nil, fakeMessage{payload: answer})
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Errorf("a closed session still received a frame")
	}
}

func TestIMMSessionPublishFailsOnAClosedClient(t *testing.T) {
	c := &MQTTClient{mqtt: &recordingBroker{}}
	c.closed.Store(true)
	s := c.OpenIMMSession("m", "d", "s", nil)
	if err := s.Publish(map[string]any{"header": map[string]any{"type": "offer"}, "msg": map[string]any{}}); err == nil {
		t.Fatal("a publish on a closed client must fail")
	}
}

func TestRTCConfigCarriesThePassword(t *testing.T) {
	var cfg WebRTCConfig
	if err := json.Unmarshal([]byte(`{"localKey":"fake-local-key","password":"fake-p2p-pass","motoId":"m1"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.LocalKey != "fake-local-key" || cfg.Password != "fake-p2p-pass" || cfg.MotoId != "m1" {
		t.Errorf("config = %+v", cfg)
	}
}
