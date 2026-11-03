package tuya

import (
	"encoding/json"
	"fmt"
	"time"
)

// IMMSession carries the protocol-302 frames of one imm session (Tuya's
// AES/KCP transport) on the topics the webrtc camera client uses: out on
// /av/moto/<motoId>/u/<devId>, back on the client's subscription.
//
// The frames themselves come from pkg/lan, whose header is the one the app's
// P2P SDK builds (trace_id, p2p_skill, security_level, path). This only wraps
// them in the 302 envelope and adds the header fields every frame this bridge
// has sent through the moto carried (sub_dev_id, tid, seq, rtx), so the relay
// sees nothing new. Experimental: the app itself sends its imm frames over
// the device channel, not the moto (see WHITEPAPER section 21).
type IMMSession struct {
	client    *MQTTClient
	topic     string
	SessionID string
	onFrame   func(data json.RawMessage)
}

// OpenIMMSession registers sessionID, so the frames the monitor sends for it
// reach onFrame (on the broker's goroutine) as the raw "data" object.
func (c *MQTTClient) OpenIMMSession(motoID, deviceID, sessionID string, onFrame func(json.RawMessage)) *IMMSession {
	s := &IMMSession{
		client:    c,
		topic:     fmt.Sprintf("/av/moto/%s/u/%s", motoID, deviceID),
		SessionID: sessionID,
		onFrame:   onFrame,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.imm == nil {
		c.imm = make(map[string]*IMMSession)
	}
	c.imm[sessionID] = s
	return s
}

// Close stops the delivery. It sends nothing.
func (s *IMMSession) Close() {
	s.client.mu.Lock()
	defer s.client.mu.Unlock()
	if s.client.imm[s.SessionID] == s {
		delete(s.client.imm, s.SessionID)
	}
}

func (c *MQTTClient) immSession(sessionID string) (*IMMSession, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.imm[sessionID]
	return s, ok
}

// rawMqttMessage is the 302 envelope around a frame built elsewhere.
type rawMqttMessage struct {
	Protocol int             `json:"protocol"`
	Pv       string          `json:"pv"`
	T        int64           `json:"t"`
	Data     json.RawMessage `json:"data"`
}

// envelopeHeaderFields are what the webrtc client's header has and the SDK's
// does not. The SDK parses sub_dev_id and ignores the rest.
var envelopeHeaderFields = map[string]json.RawMessage{
	"sub_dev_id": json.RawMessage(`""`),
	"tid":        json.RawMessage(`""`),
	"seq":        json.RawMessage(`0`),
	"rtx":        json.RawMessage(`0`),
}

// Publish sends one frame, a {"header": ..., "msg": ...} value.
func (s *IMMSession) Publish(frame any) error {
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	var parts struct {
		Header map[string]json.RawMessage `json:"header"`
		Msg    json.RawMessage            `json:"msg"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return err
	}
	if parts.Header == nil {
		return fmt.Errorf("imm frame without a header")
	}
	for k, v := range envelopeHeaderFields {
		if _, ok := parts.Header[k]; !ok {
			parts.Header[k] = v
		}
	}
	var frameType string
	_ = json.Unmarshal(parts.Header["type"], &frameType)
	data, err := json.Marshal(parts)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(rawMqttMessage{Protocol: 302, Pv: "2.2", T: time.Now().UnixMilli(), Data: data})
	if err != nil {
		return err
	}
	return s.client.publish(s.topic, "imm "+frameType, payload)
}

// deliver hands the data object of an inbound message to the session.
func (s *IMMSession) deliver(payload []byte) {
	if s.onFrame == nil {
		return
	}
	var msg struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil || len(msg.Data) == 0 {
		return
	}
	s.onFrame(msg.Data)
}
