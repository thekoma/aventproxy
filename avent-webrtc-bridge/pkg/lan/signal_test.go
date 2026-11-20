package lan

import (
	"encoding/json"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newTestOffer(t *testing.T) *Offer {
	t.Helper()
	o, err := NewOffer("uid", "dev", "uf", "pw")
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// pipeSessions returns two Session ends over net.Pipe sharing testKey.
func pipeSessions() (client, monitor *Session) {
	a, b := net.Pipe()
	return &Session{conn: a, Key: testKey}, &Session{conn: b, Key: testKey}
}

func TestOfferSDPDeclaresLANImm(t *testing.T) {
	sdp := newTestOffer(t).SDP()
	for _, want := range []string{"m=application", "imm", "a=ice-ufrag:uf", "a=ice-pwd:pw"} {
		if !strings.Contains(sdp, want) {
			t.Errorf("SDP lacks %q:\n%s", want, sdp)
		}
	}
	if !regexp.MustCompile(`(?m)^a=aes-key:[0-9a-f]{32}\r?$`).MatchString(sdp) {
		t.Errorf("SDP lacks a 32-hex aes-key line:\n%s", sdp)
	}
}

func TestOfferHeaderUsesLANPath(t *testing.T) {
	client, monitor := pipeSessions()
	defer client.Close()
	defer monitor.Close()
	o := newTestOffer(t)

	go o.SendOffer(client)
	h, payload, err := monitor.Recv(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if h.Cmd != CmdIPCLan302 {
		t.Fatalf("cmd = %d, want %d", h.Cmd, CmdIPCLan302)
	}
	body := string(payload)
	if !strings.Contains(body, `"path":"lan"`) {
		t.Errorf("no lan path: %s", body)
	}
	if strings.Contains(body, "auth") {
		t.Errorf("offer carries auth: %s", body)
	}
	var f SignalFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Msg.Token) != 1 || f.Msg.Token[0].URLs != "stun:192.0.2.1:3478" {
		t.Errorf("token = %+v, want one stun:192.0.2.1 server", f.Msg.Token)
	}
}

func TestIVFromTraceIDIsASCII(t *testing.T) {
	iv := IVFromTraceID("1B2C3D4E-5F60-7182-93A4-B5C6D7E8F901")
	if len(iv) != 12 {
		t.Fatalf("len = %d, want 12", len(iv))
	}
	if string(iv) != "1B2C3D4E5F60" {
		t.Errorf("iv = %q, want the first 12 trace-id characters", iv)
	}
	for _, c := range iv {
		if c < 0x20 || c > 0x7e {
			t.Errorf("non-printable byte %#x", c)
		}
	}
}

func answerFrame(t *testing.T, sessionID string) []byte {
	t.Helper()
	b, err := json.Marshal(SignalFrame{
		Header: SignalHeader{Type: TypeAnswer, SessionID: sessionID},
		Msg:    SignalMessage{SDP: "v=0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadSignalSkipsOtherSessions(t *testing.T) {
	client, monitor := pipeSessions()
	defer client.Close()
	defer monitor.Close()
	iv := []byte("0123456789ab")

	go func() {
		monitor.Send(CmdIPCLan302, iv, answerFrame(t, "other"))
		monitor.Send(CmdIPCLan302, iv, answerFrame(t, "mine"))
	}()
	f, err := ReadSignal(client, "mine", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if f.Header.SessionID != "mine" {
		t.Errorf("sessionid = %q, want mine", f.Header.SessionID)
	}
}

func TestReadSignalTimesOut(t *testing.T) {
	client, monitor := pipeSessions()
	defer client.Close()
	defer monitor.Close()

	start := time.Now()
	if _, err := ReadSignal(client, "mine", 50*time.Millisecond); err == nil {
		t.Fatal("want a timeout error")
	}
	if time.Since(start) > time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestReadSignalTakesASessionlessDisconnect(t *testing.T) {
	client, monitor := pipeSessions()
	defer client.Close()
	defer monitor.Close()
	iv := []byte("0123456789ab")

	frame := func(sessionID string) []byte {
		b, err := json.Marshal(SignalFrame{Header: SignalHeader{Type: TypeDisconnect, SessionID: sessionID}})
		if err != nil {
			t.Error(err)
		}
		return b
	}
	go func() {
		_ = monitor.Send(CmdIPCLan302, iv, frame("other")) // another client's: skipped
		_ = monitor.Send(CmdIPCLan302, iv, frame(""))      // a refusal that names no session: ours
	}()
	f, err := ReadSignal(client, "mine", time.Second)
	if err != nil {
		t.Fatalf("a disconnect without a session id read as %v; a refusal must not look like a timeout", err)
	}
	if f.Header.Type != TypeDisconnect || f.Header.SessionID != "" {
		t.Fatalf("got %+v, want the sessionless disconnect", f.Header)
	}
}
