package lan

import (
	"encoding/json"
	"strings"
	"testing"
)

// frameMap marshals v and reads it back as plain maps, so a test sees the
// keys on the wire rather than the Go fields.
func frameMap(t *testing.T, v any) (header, msg map[string]any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Header map[string]any `json:"header"`
		Msg    map[string]any `json:"msg"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Header, f.Msg
}

// The LAN offer must not move: the 3.5 path is tested on hardware.
func TestLANOfferFrameUnchanged(t *testing.T) {
	o := newTestOffer(t)
	h, m := frameMap(t, o.OfferFrame())
	want := map[string]any{
		"from": "uid", "to": "dev", "moto_id": "", "path": "lan", "type": "offer",
		"is_pre": float64(0), "p2p_skill": float64(1635), "security_level": float64(3),
		"sessionid": o.SessionID, "trace_id": o.TraceID,
	}
	if len(h) != len(want) {
		t.Errorf("header keys = %v, want %v", h, want)
	}
	for k, v := range want {
		if h[k] != v {
			t.Errorf("header[%s] = %v, want %v", k, h[k], v)
		}
	}
	for _, k := range []string{"sdp", "preconnect", "token"} {
		if _, ok := m[k]; !ok {
			t.Errorf("msg lacks %s: %v", k, m)
		}
	}
	for _, k := range []string{"mode", "auth", "stream_type"} {
		if _, ok := m[k]; ok {
			t.Errorf("msg carries %s: %v", k, m)
		}
	}
}

// The cloud offer is the app's: the LAN offer with path mqtt and the moto id,
// and still no mode, auth or stream_type (libThingP2PSDK builds none of them).
func TestCloudOfferFrame(t *testing.T) {
	o := newTestOffer(t)
	o.Path, o.MotoID = PathMQTT, "moto-1"
	o.ICEServers = []ICEServer{{URLs: "stun:stun.example:3478"}, {URLs: "turn:turn.example:3478", Username: "u", Credential: "c", TTL: 600}}
	h, m := frameMap(t, o.OfferFrame())
	if h["path"] != "mqtt" || h["moto_id"] != "moto-1" || h["type"] != "offer" {
		t.Errorf("header = %v", h)
	}
	for _, k := range []string{"mode", "auth", "stream_type"} {
		if _, ok := m[k]; ok {
			t.Errorf("msg carries %s: %v", k, m)
		}
	}
	token, ok := m["token"].([]any)
	if !ok || len(token) != 2 {
		t.Fatalf("token = %v, want the two ICE servers", m["token"])
	}
	if m["preconnect"] != true {
		t.Errorf("preconnect = %v, want true", m["preconnect"])
	}
	if sdp, _ := m["sdp"].(string); !strings.Contains(sdp, "a=rtpmap:6001 AES/KCP 330") {
		t.Errorf("sdp is not the imm description:\n%s", sdp)
	}
}

func TestCandidateFrameAddsTheSDPPrefix(t *testing.T) {
	o := newTestOffer(t)
	o.Path = PathMQTT
	h, m := frameMap(t, o.CandidateFrame("candidate:1 1 UDP 2130706431 192.168.1.5 40000 typ host"))
	if h["type"] != "candidate" || h["path"] != "mqtt" || h["sessionid"] != o.SessionID {
		t.Errorf("header = %v", h)
	}
	if m["candidate"] != "a=candidate:1 1 UDP 2130706431 192.168.1.5 40000 typ host\r\n" {
		t.Errorf("candidate = %q", m["candidate"])
	}
}

// The SDK's own disconnect carries close reasons and no token.
func TestDisconnectFrame(t *testing.T) {
	o := newTestOffer(t)
	o.Path, o.MotoID = PathMQTT, "moto-1"
	h, m := frameMap(t, o.DisconnectFrame())
	if h["type"] != "disconnect" || h["sessionid"] != o.SessionID || h["moto_id"] != "moto-1" {
		t.Errorf("header = %v", h)
	}
	if m["close_reason"] != float64(4) || m["close_reason_local"] != float64(0) {
		t.Errorf("msg = %v", m)
	}
	if _, ok := m["token"]; ok {
		t.Errorf("disconnect carries a token: %v", m)
	}
}

func TestParseSignalFrameIgnoresAnOddToken(t *testing.T) {
	data := []byte(`{"header":{"type":"answer","sessionid":"s1","is_pre":"0"},` +
		`"msg":{"sdp":"v=0","token":[{"urls":"stun:x","ttl":"600"}]}}`)
	f, err := ParseSignalFrame(data)
	if err != nil {
		t.Fatal(err)
	}
	if f.Header.Type != TypeAnswer || f.Header.SessionID != "s1" || f.Msg.SDP != "v=0" {
		t.Errorf("frame = %+v", f)
	}
}
