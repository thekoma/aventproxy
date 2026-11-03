package tuya

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// fakeDeviceBroker is one device-channel connection: it records what is
// subscribed, unsubscribed and published, and plays the device.
type fakeDeviceBroker struct {
	mqtt.Client
	mu           sync.Mutex
	handlers     map[string]mqtt.MessageHandler
	subscribes   []string
	unsubscribes []string
	topics       []string
	payloads     [][]byte
	disconnected bool
	subscribeErr error
}

func newFakeDeviceBroker() *fakeDeviceBroker {
	return &fakeDeviceBroker{handlers: map[string]mqtt.MessageHandler{}}
}

type errToken struct {
	mqtt.Token
	err error
}

func (t errToken) Wait() bool                     { return true }
func (t errToken) WaitTimeout(time.Duration) bool { return true }
func (t errToken) Error() error                   { return t.err }

func (b *fakeDeviceBroker) Subscribe(topic string, _ byte, cb mqtt.MessageHandler) mqtt.Token {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribes = append(b.subscribes, topic)
	if b.subscribeErr != nil {
		return errToken{err: b.subscribeErr}
	}
	b.handlers[topic] = cb
	return ackedToken{}
}

func (b *fakeDeviceBroker) Unsubscribe(topics ...string) mqtt.Token {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unsubscribes = append(b.unsubscribes, topics...)
	for _, t := range topics {
		delete(b.handlers, t)
	}
	return ackedToken{}
}

func (b *fakeDeviceBroker) Publish(topic string, _ byte, _ bool, payload interface{}) mqtt.Token {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.topics = append(b.topics, topic)
	b.payloads = append(b.payloads, payload.([]byte))
	return ackedToken{}
}

func (b *fakeDeviceBroker) Disconnect(uint) {
	b.mu.Lock()
	b.disconnected = true
	b.mu.Unlock()
}

// deliver plays the device publishing on topic.
func (b *fakeDeviceBroker) deliver(topic string, payload []byte) {
	b.mu.Lock()
	cb := b.handlers[topic]
	b.mu.Unlock()
	if cb != nil {
		cb(nil, fakeMessage{payload: payload})
	}
}

func (b *fakeDeviceBroker) snapshot() (subs, unsubs []string, disconnected bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.subscribes...), append([]string(nil), b.unsubscribes...), b.disconnected
}

// fakePool is a pool whose dials hand out fake brokers.
type fakePool struct {
	*DeviceChannels
	mu      sync.Mutex
	brokers []*fakeDeviceBroker
	lost    []mqtt.ConnectionLostHandler
	next    func(b *fakeDeviceBroker)
}

func newFakePool() *fakePool {
	fp := &fakePool{DeviceChannels: NewDeviceChannels()}
	fp.DeviceChannels.dial = func(_ MobileMQTTConfig, onLost mqtt.ConnectionLostHandler) (mqtt.Client, error) {
		b := newFakeDeviceBroker()
		fp.mu.Lock()
		if fp.next != nil {
			fp.next(b)
		}
		fp.brokers = append(fp.brokers, b)
		fp.lost = append(fp.lost, onLost)
		fp.mu.Unlock()
		return b, nil
	}
	return fp
}

func (fp *fakePool) dials() int {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return len(fp.brokers)
}

func (fp *fakePool) broker(i int) *fakeDeviceBroker {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.brokers[i]
}

func accountConfig(deviceID string, onData func(json.RawMessage)) DeviceChannelConfig {
	return DeviceChannelConfig{
		MQTT:     MobileMQTTConfig{ClientID: "pkg_mb_phone_uidhash_IMM"},
		DeviceID: deviceID,
		LocalKey: string(fakeLocalKey),
		PV:       "2.2",
		OnData:   onData,
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func answerFrame(t *testing.T, sessionID string) []byte {
	t.Helper()
	frame, err := EncodeDeviceFrame("2.2", fakeLocalKey, DeviceFrameHeader{S: 9, O: 5000}, 302, 1,
		json.RawMessage(`{"header":{"type":"answer","sessionid":"`+sessionID+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestDeviceChannelPublishesTheAppsFrame(t *testing.T) {
	fp := newFakePool()
	d, err := fp.Open(accountConfig("dev-1", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	frame := map[string]any{"header": map[string]any{"type": "offer", "sessionid": "s1", "path": "mqtt"}, "msg": map[string]any{"sdp": "v=0"}}
	for range 2 {
		if err := d.Publish(frame); err != nil {
			t.Fatal(err)
		}
	}
	b := fp.broker(0)
	if len(b.topics) != 2 || b.topics[0] != "smart/mb/out/dev-1" {
		t.Fatalf("topics = %v, want smart/mb/out/dev-1", b.topics)
	}
	for i, payload := range b.payloads {
		if string(payload[:3]) != "2.2" {
			t.Fatalf("payload %d is not a 2.2 frame", i)
		}
		// The app's counter starts at 2 and goes up before each publish.
		if s := uint32(payload[7])<<24 | uint32(payload[8])<<16 | uint32(payload[9])<<8 | uint32(payload[10]); s != uint32(3+i) {
			t.Errorf("payload %d: s = %d, want %d", i, s, 3+i)
		}
		protocol, data, err := DecodeDeviceFrame(fakeLocalKey, payload)
		if err != nil || protocol != 302 {
			t.Fatalf("payload %d: %d %v", i, protocol, err)
		}
		var got map[string]map[string]any
		if err := json.Unmarshal(data, &got); err != nil || got["header"]["path"] != "mqtt" || got["msg"]["sdp"] != "v=0" {
			t.Errorf("payload %d data = %s (%v)", i, data, err)
		}
		// The device channel carries the SDK's frame as is: no moto envelope.
		if _, ok := got["header"]["tid"]; ok {
			t.Errorf("payload %d carries the moto envelope: %s", i, data)
		}
	}
}

func TestDeviceChannelHandsOnOnly302(t *testing.T) {
	fp := newFakePool()
	var mu sync.Mutex
	var got []string
	d, err := fp.Open(accountConfig("dev-1", func(b json.RawMessage) {
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	b := fp.broker(0)
	status, _ := EncodeDeviceFrame("2.2", fakeLocalKey, DeviceFrameHeader{S: 10, O: 5000}, 4, 1, json.RawMessage(`{"dps":{"1":true}}`))
	b.deliver("smart/mb/in/dev-1", answerFrame(t, "s1"))
	b.deliver("smart/mb/in/dev-1", status)
	b.deliver("smart/mb/in/dev-1", []byte("garbage"))
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != `{"header":{"type":"answer","sessionid":"s1"}}` {
		t.Fatalf("delivered %v", got)
	}
}

func TestDeviceChannelPicksTheFramingByPV(t *testing.T) {
	for pv, want := range map[string]string{"2.2": "2.2", "": "2.2", "2.3": "2.3"} {
		fp := newFakePool()
		cfg := accountConfig("d", nil)
		cfg.PV = pv
		d, err := fp.Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Publish(map[string]any{"header": map[string]any{}, "msg": map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		if got := string(fp.broker(0).payloads[0][:3]); got != want {
			t.Errorf("pv %q: frame %s, want %s", pv, got, want)
		}
		d.Close()
	}
}

// I1: two cameras of one account share one connection, so neither kicks the
// other off the broker, and each has its own subscription.
func TestTwoCamerasShareOneDeviceChannel(t *testing.T) {
	fp := newFakePool()
	var mu sync.Mutex
	got := map[string]int{}
	reader := func(name string) func(json.RawMessage) {
		return func(json.RawMessage) { mu.Lock(); got[name]++; mu.Unlock() }
	}
	a, err := fp.Open(accountConfig("dev-a", reader("a")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := fp.Open(accountConfig("dev-b", reader("b")))
	if err != nil {
		t.Fatal(err)
	}
	if n := fp.dials(); n != 1 {
		t.Fatalf("dialled %d connections for one account, want 1", n)
	}
	broker := fp.broker(0)
	if subs, _, _ := broker.snapshot(); len(subs) != 2 || subs[0] != "smart/mb/in/dev-a" || subs[1] != "smart/mb/in/dev-b" {
		t.Fatalf("subscriptions = %v", subs)
	}
	broker.deliver("smart/mb/in/dev-a", answerFrame(t, "sa"))
	broker.deliver("smart/mb/in/dev-b", answerFrame(t, "sb"))
	broker.deliver("smart/mb/in/dev-b", answerFrame(t, "sb2"))
	mu.Lock()
	if got["a"] != 1 || got["b"] != 2 {
		t.Fatalf("routed %v, want each device's frames to its own session", got)
	}
	mu.Unlock()

	// Releasing one camera keeps the other's subscription and the connection.
	a.Close()
	eventually(t, "camera A unsubscribed", func() bool {
		_, unsubs, _ := broker.snapshot()
		return len(unsubs) == 1 && unsubs[0] == "smart/mb/in/dev-a"
	})
	time.Sleep(20 * time.Millisecond)
	if _, unsubs, disc := broker.snapshot(); len(unsubs) != 1 || disc {
		t.Fatalf("after A: unsubscribed %v, disconnected %v; want only A unsubscribed", unsubs, disc)
	}
	broker.deliver("smart/mb/in/dev-b", answerFrame(t, "sb3"))
	mu.Lock()
	if got["b"] != 3 {
		t.Fatalf("camera B stopped receiving after A left: %v", got)
	}
	mu.Unlock()

	// The last one out closes the connection.
	b.Close()
	eventually(t, "the connection closed", func() bool { _, _, disc := broker.snapshot(); return disc })

	// The next session dials afresh.
	c, err := fp.Open(accountConfig("dev-a", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if n := fp.dials(); n != 2 {
		t.Fatalf("dials = %d, want a new connection after the last one closed", n)
	}
}

// Two sessions on one device (an abandoned attempt and a newer one) share the
// subscription; the first one leaving keeps it.
func TestTwoSessionsOnOneDevice(t *testing.T) {
	fp := newFakePool()
	var n1, n2 int
	var mu sync.Mutex
	s1, _ := fp.Open(accountConfig("dev-a", func(json.RawMessage) { mu.Lock(); n1++; mu.Unlock() }))
	s2, _ := fp.Open(accountConfig("dev-a", func(json.RawMessage) { mu.Lock(); n2++; mu.Unlock() }))
	broker := fp.broker(0)
	if subs, _, _ := broker.snapshot(); len(subs) != 1 {
		t.Fatalf("subscriptions = %v, want one", subs)
	}
	s1.Close()
	s1.Close() // idempotent
	time.Sleep(20 * time.Millisecond)
	broker.deliver("smart/mb/in/dev-a", answerFrame(t, "x"))
	if _, unsubs, disc := broker.snapshot(); len(unsubs) != 0 || disc {
		t.Fatalf("unsubscribed %v, disconnected %v while a session still reads", unsubs, disc)
	}
	mu.Lock()
	if n1 != 0 || n2 != 1 {
		t.Fatalf("n1=%d n2=%d", n1, n2)
	}
	mu.Unlock()
	s2.Close()
	eventually(t, "closed", func() bool { _, _, disc := broker.snapshot(); return disc })
}

// A connection the broker dropped is forgotten: the next session dials anew.
func TestLostDeviceChannelIsRedialled(t *testing.T) {
	fp := newFakePool()
	a, err := fp.Open(accountConfig("dev-a", nil))
	if err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	onLost := fp.lost[0]
	fp.mu.Unlock()
	onLost(nil, errors.New("connection reset"))
	b, err := fp.Open(accountConfig("dev-b", nil))
	if err != nil {
		t.Fatal(err)
	}
	if n := fp.dials(); n != 2 {
		t.Fatalf("dials = %d, want a new connection after a loss", n)
	}
	a.Close()
	b.Close()
}

func TestFailedSubscribeLeavesNoConnection(t *testing.T) {
	fp := newFakePool()
	fp.next = func(b *fakeDeviceBroker) { b.subscribeErr = errors.New("not authorised") }
	if _, err := fp.Open(accountConfig("dev-a", nil)); err == nil {
		t.Fatal("a refused subscription must fail the open")
	}
	if _, _, disc := fp.broker(0).snapshot(); !disc {
		t.Fatal("the unused connection must be closed")
	}
	fp.next = nil
	d, err := fp.Open(accountConfig("dev-a", nil))
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if n := fp.dials(); n != 2 {
		t.Fatalf("dials = %d", n)
	}
}

func TestDialDeviceChannelRefusesABadKey(t *testing.T) {
	if _, err := DialDeviceChannel(DeviceChannelConfig{DeviceID: "d", LocalKey: "short"}); err == nil {
		t.Fatal("a local key that is not 16 bytes must be refused before connecting")
	}
}
