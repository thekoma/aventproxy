package lan

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Protocol-302 message types, the same vocabulary the cloud MQTT path uses.
const (
	TypeOffer      = "offer"
	TypeAnswer     = "answer"
	TypeCandidate  = "candidate"
	TypeDisconnect = "disconnect"
)

// SignalHeader is the "header" object of a 302 frame.
type SignalHeader struct {
	From          string `json:"from"`
	To            string `json:"to"`
	MotoID        string `json:"moto_id"`
	Path          string `json:"path"`
	Type          string `json:"type"`
	IsPre         int    `json:"is_pre"`
	P2PSkill      int    `json:"p2p_skill,omitempty"`
	SecurityLevel int    `json:"security_level,omitempty"`
	SessionID     string `json:"sessionid"`
	TraceID       string `json:"trace_id"`
}

// ICEServer is one entry of the token list carried in an offer or answer.
type ICEServer struct {
	URLs       any    `json:"urls,omitempty"`
	Username   string `json:"username,omitempty"`
	Credential string `json:"credential,omitempty"`
	TTL        int    `json:"ttl,omitempty"`
}

// SignalMessage is the "msg" object of a 302 frame.
type SignalMessage struct {
	SDP        string      `json:"sdp,omitempty"`
	Candidate  string      `json:"candidate,omitempty"`
	Preconnect bool        `json:"preconnect,omitempty"`
	Token      []ICEServer `json:"token"`
	// Log and TCPToken are cloud-issued and opaque to us; the app always sends
	// them, so they are passed through verbatim rather than modelled.
	Log      json.RawMessage `json:"log,omitempty"`
	TCPToken json.RawMessage `json:"tcp_token,omitempty"`
}

// SignalFrame is a complete 302 body.
type SignalFrame struct {
	Header SignalHeader  `json:"header"`
	Msg    SignalMessage `json:"msg"`
}

// Offer describes the session we want from the monitor.
type Offer struct {
	UID       string
	DeviceID  string
	SessionID string
	TraceID   string
	IV        []byte
	AESKey    string // 16 ASCII characters, sent hex-encoded in the SDP
	ICEUfrag  string
	ICEPwd    string
	// ICEServers is what the monitor calls the token. It gathers no candidates
	// at all when this is empty.
	ICEServers []ICEServer
	Log        json.RawMessage
	TCPToken   json.RawMessage
}

// NewOffer mints the identifiers a fresh session needs.
func NewOffer(uid, deviceID, iceUfrag, icePwd string) (*Offer, error) {
	key, err := randomASCII(16)
	if err != nil {
		return nil, err
	}
	suffix, err := randomASCII(8)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	trace := strings.ToUpper(uuid.NewString())
	return &Offer{
		UID:        uid,
		DeviceID:   deviceID,
		SessionID:  fmt.Sprintf("%s%d%s", deviceID, now, suffix),
		TraceID:    fmt.Sprintf("%s_%s_%d", trace, deviceID, now*1000),
		IV:         IVFromTraceID(trace),
		AESKey:     key,
		ICEUfrag:   iceUfrag,
		ICEPwd:     icePwd,
		ICEServers: placeholderICEServers(),
	}, nil
}

// IVFromTraceID derives the GCM IV the monitor expects.
//
// This is not cosmetic. A random binary IV produces a frame the monitor
// acknowledges and then discards without answering; only the ASCII text of a
// UUID gets acted on.
func IVFromTraceID(traceID string) []byte {
	flat := strings.ReplaceAll(traceID, "-", "")
	if len(flat) < gcmIVLen {
		flat += strings.Repeat("0", gcmIVLen-len(flat))
	}
	return []byte(flat[:gcmIVLen])
}

// SDP renders the offer. The monitor rejects a WebRTC/DTLS description on the
// LAN path with close_reason 7, so this always describes the imm transport.
func (o *Offer) SDP() string {
	now := time.Now().Unix()
	return strings.Join([]string{
		"v=0",
		fmt.Sprintf("o=- %d 1 IN IP4 127.0.0.1", now),
		"s=-",
		"t=0 0",
		"a=group:BUNDLE imm0",
		fmt.Sprintf("a=msid-semantic: WMS %s", o.SessionID),
		"m=application 9 imm 6001",
		"c=IN IP4 0.0.0.0",
		"a=rtcp:9 IN IP4 0.0.0.0",
		fmt.Sprintf("a=ice-ufrag:%s", o.ICEUfrag),
		fmt.Sprintf("a=ice-pwd:%s", o.ICEPwd),
		"a=ice-options:trickle",
		fmt.Sprintf("a=aes-key:%s", hex.EncodeToString([]byte(o.AESKey))),
		"a=mid:imm0",
		"a=rtpmap:6001 AES/KCP 330",
		fmt.Sprintf("a=ssrc:0 cname:%s", o.UID),
		"",
	}, "\r\n")
}

func (o *Offer) header(msgType string) SignalHeader {
	return SignalHeader{
		From:          o.UID,
		To:            o.DeviceID,
		MotoID:        "",
		Path:          "lan",
		Type:          msgType,
		IsPre:         0,
		P2PSkill:      1635,
		SecurityLevel: 3,
		SessionID:     o.SessionID,
		TraceID:       o.TraceID,
	}
}

// placeholderICEServers exists because the monitor gathers no candidates at all
// when the token list is empty: it answers, then sends an empty candidate and
// disconnects.
//
// The entry is never contacted on a LAN pairing, but it must be an IP literal.
// A hostname makes the monitor resolve it before gathering anything, so with no
// DNS it produces the same empty candidate as an empty list. This is TEST-NET-1
// from RFC 5737: reserved for documentation and never routable.
func placeholderICEServers() []ICEServer {
	return []ICEServer{{URLs: "stun:192.0.2.1:3478"}}
}

// SendOffer writes the offer to the monitor.
func (o *Offer) SendOffer(s *Session) error {
	return sendSignal(s, o.IV, SignalFrame{
		Header: o.header(TypeOffer),
		Msg: SignalMessage{
			SDP:        o.SDP(),
			Preconnect: true,
			Token:      o.ICEServers,
			Log:        o.Log,
			TCPToken:   o.TCPToken,
		},
	})
}

// SendCandidate trickles one local ICE candidate.
func (o *Offer) SendCandidate(s *Session, candidate string) error {
	if !strings.HasPrefix(candidate, "a=") {
		candidate = "a=" + candidate
	}
	if !strings.HasSuffix(candidate, "\r\n") {
		candidate += "\r\n"
	}
	return sendSignal(s, o.IV, SignalFrame{
		Header: o.header(TypeCandidate),
		Msg:    SignalMessage{Candidate: candidate, Token: []ICEServer{}},
	})
}

func sendSignal(s *Session, iv []byte, frame SignalFrame) error {
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return s.Send(CmdIPCLan302, iv, body)
}

// ReadSignal returns the next 302 frame for sessionID, skipping
// acknowledgements, status pushes and frames addressed to other sessions: the
// monitor answers every LAN client, the phone app included.
func ReadSignal(s *Session, sessionID string, timeout time.Duration) (*SignalFrame, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h, payload, err := s.Recv(time.Until(deadline))
		if err != nil {
			return nil, err
		}
		if h.Cmd != CmdIPCLan302 {
			continue
		}
		payload = StripRetcode(payload)
		var frame SignalFrame
		if err := json.Unmarshal(payload, &frame); err != nil {
			continue // 32-byte acknowledgements are not JSON
		}
		if frame.Header.Type == "" || frame.Header.SessionID != sessionID {
			continue
		}
		return &frame, nil
	}
	return nil, fmt.Errorf("lan: no signalling frame within %s", timeout)
}

func randomASCII(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
}
