package tuya

import (
	"encoding/json"
	"testing"
)

func TestDeviceChannelPublishesTheAppsFrame(t *testing.T) {
	broker := &recordingBroker{}
	d := newDeviceChannel(DeviceChannelConfig{DeviceID: "dev-1", LocalKey: string(fakeLocalKey), PV: "2.2"})
	d.client = broker
	frame := map[string]any{"header": map[string]any{"type": "offer", "sessionid": "s1", "path": "mqtt"}, "msg": map[string]any{"sdp": "v=0"}}
	if err := d.Publish(frame); err != nil {
		t.Fatal(err)
	}
	if err := d.Publish(frame); err != nil {
		t.Fatal(err)
	}
	if len(broker.topics) != 2 || broker.topics[0] != "smart/mb/out/dev-1" {
		t.Fatalf("topics = %v, want smart/mb/out/dev-1", broker.topics)
	}
	for i, payload := range broker.payloads {
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
	var got []json.RawMessage
	d := newDeviceChannel(DeviceChannelConfig{DeviceID: "dev-1", LocalKey: string(fakeLocalKey), OnData: func(b json.RawMessage) { got = append(got, b) }})
	answer, _ := EncodeDeviceFrame("2.2", fakeLocalKey, DeviceFrameHeader{S: 9, O: 5000}, 302, 1, json.RawMessage(`{"header":{"type":"answer"}}`))
	status, _ := EncodeDeviceFrame("2.2", fakeLocalKey, DeviceFrameHeader{S: 10, O: 5000}, 4, 1, json.RawMessage(`{"dps":{"1":true}}`))
	d.receive(nil, fakeMessage{payload: answer})
	d.receive(nil, fakeMessage{payload: status})
	d.receive(nil, fakeMessage{payload: []byte("garbage")})
	if len(got) != 1 || string(got[0]) != `{"header":{"type":"answer"}}` {
		t.Fatalf("delivered %s", got)
	}
}

func TestDeviceChannelPicksTheFramingByPV(t *testing.T) {
	for pv, want := range map[string]string{"2.2": "2.2", "": "2.2", "2.3": "2.3"} {
		broker := &recordingBroker{}
		d := newDeviceChannel(DeviceChannelConfig{DeviceID: "d", LocalKey: string(fakeLocalKey), PV: pv})
		d.client = broker
		if err := d.Publish(map[string]any{"header": map[string]any{}, "msg": map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		if got := string(broker.payloads[0][:3]); got != want {
			t.Errorf("pv %q: frame %s, want %s", pv, got, want)
		}
	}
}

func TestDialDeviceChannelRefusesABadKey(t *testing.T) {
	if _, err := DialDeviceChannel(DeviceChannelConfig{DeviceID: "d", LocalKey: "short"}); err == nil {
		t.Fatal("a local key that is not 16 bytes must be refused before connecting")
	}
}
