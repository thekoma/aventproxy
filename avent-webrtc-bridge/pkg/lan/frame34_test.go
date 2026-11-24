package lan

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// vectors34 are frames tinytuya 1.20.0 produced for a fake key and pinned
// nonces; testdata/gen_tinytuya34.py regenerates them. Matching them byte for
// byte is what says the Go framing is the one the monitor accepted from
// tinytuya.
type vectors34 struct {
	LocalKey     string `json:"local_key"`
	LocalNonce   string `json:"local_nonce"`
	RemoteNonce  string `json:"remote_nonce"`
	NegStart     string `json:"neg_start"`
	NegResp      string `json:"neg_resp"`
	NegFinish    string `json:"neg_finish"`
	SessionKey   string `json:"session_key"`
	HeartbeatSeq uint32 `json:"heartbeat_seq"`
	Heartbeat    string `json:"heartbeat"`
	ControlBody  string `json:"control_body"`
	ControlSeq   uint32 `json:"control_seq"`
	Control      string `json:"control"`
	SignalBody   string `json:"signal_body"`
	SignalSeq    uint32 `json:"signal_seq"`
	Signal       string `json:"signal"`
	WithHeader   string `json:"signal_with_header"`
	AnswerBody   string `json:"answer_body"`
	Answer       string `json:"answer"`
}

func loadVectors34(t *testing.T) vectors34 {
	t.Helper()
	raw, err := os.ReadFile("testdata/tinytuya34.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors34
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertFrame(t *testing.T, name string, got []byte, err error, want []byte) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from tinytuya:\n got %x\nwant %x", name, got, want)
	}
}

func TestPack55AA34MatchesTinytuya(t *testing.T) {
	v := loadVectors34(t)
	key := []byte(v.LocalKey)
	session := unhex(t, v.SessionKey)

	got, err := Pack55AA34(key, 1, CmdSessKeyNegStart, unhex(t, v.LocalNonce))
	assertFrame(t, "neg start", got, err, unhex(t, v.NegStart))

	got, err = Pack55AA34(session, v.HeartbeatSeq, CmdHeartbeat, versionHeader34For(CmdHeartbeat))
	assertFrame(t, "heartbeat", got, err, unhex(t, v.Heartbeat))

	// Commands outside the exception list keep tinytuya's version header.
	got, err = Pack55AA34(session, v.ControlSeq, 7, append(versionHeader34For(7), v.ControlBody...))
	assertFrame(t, "control", got, err, unhex(t, v.Control))
}

// Command 32 goes without the version header: the SCD951 ignored the offer
// with it and answered at once without it.
func TestCommand32HasNoVersionHeaderOn34(t *testing.T) {
	v := loadVectors34(t)
	session := unhex(t, v.SessionKey)

	if h := versionHeader34For(CmdIPCLan302); h != nil {
		t.Fatalf("command 32 gets a %d-byte version header", len(h))
	}
	got, err := Pack55AA34(session, v.SignalSeq, CmdIPCLan302, append(versionHeader34For(CmdIPCLan302), v.SignalBody...))
	assertFrame(t, "signal", got, err, unhex(t, v.Signal))

	// tinytuya's own encoding adds the header; that is the frame the monitor
	// stayed silent to, and it is not what the session sends.
	got, err = Pack55AA34(session, v.SignalSeq+1, CmdIPCLan302, append(append([]byte{}, versionHeader34...), v.SignalBody...))
	assertFrame(t, "signal with header", got, err, unhex(t, v.WithHeader))

	// And the session sends the first form.
	client, monitor := net.Pipe()
	defer monitor.Close()
	s := &Session{conn: client, Key: session, Version: V34}
	s.seq.Store(v.SignalSeq - 1)
	go func() { _ = s.Send(CmdIPCLan302, testIV, []byte(v.SignalBody)) }()
	wire, err := readExactly(monitor, len(v.Signal)/2)
	assertFrame(t, "signal on the wire", wire, err, unhex(t, v.Signal))
}

func TestUnpack55AA34MonitorFrames(t *testing.T) {
	v := loadVectors34(t)
	key := []byte(v.LocalKey)

	h, plain, err := Unpack55AA34(key, unhex(t, v.NegResp))
	if err != nil {
		t.Fatal(err)
	}
	if h.Cmd != CmdSessKeyNegResp || len(plain) != 48 {
		t.Fatalf("neg response: cmd %d, %d bytes", h.Cmd, len(plain))
	}
	if !bytes.Equal(plain[:16], unhex(t, v.RemoteNonce)) {
		t.Fatalf("remote nonce = %x", plain[:16])
	}
	if !bytes.Equal(plain[16:], hmacSHA256(key, unhex(t, v.LocalNonce))) {
		t.Fatal("neg response HMAC field is not HMAC(localKey, localNonce)")
	}

	h, plain, err = Unpack55AA34(unhex(t, v.SessionKey), unhex(t, v.Answer))
	if err != nil {
		t.Fatal(err)
	}
	if h.Cmd != CmdIPCLan302 || string(plain) != v.AnswerBody {
		t.Fatalf("answer: cmd %d body %q", h.Cmd, plain)
	}
}

func TestUnpack55AA34RejectsATamperedFrame(t *testing.T) {
	v := loadVectors34(t)
	frame := unhex(t, v.Answer)
	frame[30] ^= 0x01
	if _, _, err := Unpack55AA34(unhex(t, v.SessionKey), frame); err == nil {
		t.Fatal("a tampered frame was accepted")
	}
	if _, _, err := Unpack55AA34([]byte("another-fake-key"), unhex(t, v.Answer)); err == nil {
		t.Fatal("a frame under another key was accepted")
	}
}

func TestSessionKey34MatchesTinytuya(t *testing.T) {
	v := loadVectors34(t)
	got, err := sessionKey(V34, []byte(v.LocalKey), unhex(t, v.LocalNonce), unhex(t, v.RemoteNonce))
	assertFrame(t, "session key", got, err, unhex(t, v.SessionKey))
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]Version{"3.3": V33, "3.4": V34, "3.5": V35, " 3.5 ": V35} {
		if got, err := ParseVersion(in); err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "3.1", "3.6", "auto", "35"} {
		if _, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) accepted", in)
		}
	}
}

func readExactly(conn net.Conn, n int) ([]byte, error) {
	buf := make([]byte, n)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := io.ReadFull(conn, buf)
	return buf, err
}

// The whole negotiation against a scripted monitor, compared on the wire with
// what tinytuya sends, then a heartbeat under the session key.
func TestDial34MatchesTinytuyaOnTheWire(t *testing.T) {
	v := loadVectors34(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	mismatch := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		expect := func(name, want string) bool {
			got, err := readExactly(conn, len(want)/2)
			if err != nil || hex.EncodeToString(got) != want {
				mismatch <- name
				return false
			}
			return true
		}
		if !expect("neg start", v.NegStart) {
			return
		}
		_, _ = conn.Write(unhex(t, v.NegResp))
		if !expect("neg finish", v.NegFinish) || !expect("heartbeat", v.Heartbeat) {
			return
		}
		mismatch <- ""
	}()

	oldPort, oldNonce := dialPort, nonceSource
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	dialPort, _ = strconv.Atoi(port)
	nonceSource = bytes.NewReader(unhex(t, v.LocalNonce))
	defer func() { dialPort, nonceSource = oldPort, oldNonce }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := Dial(ctx, "127.0.0.1", "dev", v.LocalKey, V34)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if hex.EncodeToString(s.Key) != v.SessionKey {
		t.Fatalf("session key = %x, want %s", s.Key, v.SessionKey)
	}
	if err := s.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	select {
	case name := <-mismatch:
		if name != "" {
			t.Fatalf("%s on the wire differs from tinytuya", name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the scripted monitor never saw the heartbeat")
	}
}

// A 3.4 answer, with its return code in clear, reaches the offer's reader.
func TestReadSignalOn34(t *testing.T) {
	v := loadVectors34(t)
	client, monitor := net.Pipe()
	defer monitor.Close()
	s := &Session{conn: client, Key: unhex(t, v.SessionKey), Version: V34}
	go func() { _, _ = monitor.Write(unhex(t, v.Answer)) }()
	frame, err := ReadSignal(s, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Header.Type != TypeAnswer || frame.Msg.SDP != "a=ice-ufrag:abcd" {
		t.Fatalf("got %+v", frame)
	}
}

func TestDialRefusesAnUnknownVersion(t *testing.T) {
	if _, err := Dial(context.Background(), "127.0.0.1", "dev", string(testKey), Version(31)); err == nil {
		t.Fatal("Dial accepted protocol 3.1")
	}
}
