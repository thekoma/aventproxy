package lan

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// vector33 is one frame tinytuya 1.20.0 built for a fake key;
// testdata/gen_tinytuya33.py regenerates them. Client frames are as tinytuya
// sends them, monitor frames carry the return code tinytuya strips.
type vector33 struct {
	Seq   uint32 `json:"seq"`
	Cmd   uint32 `json:"cmd"`
	Plain string `json:"plain"`
	Frame string `json:"frame"`
}

func loadVectors33(t *testing.T) (key string, frames map[string]vector33) {
	t.Helper()
	raw, err := os.ReadFile("testdata/tinytuya33.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	frames = map[string]vector33{}
	for name, v := range doc {
		if name == "local_key" {
			if err := json.Unmarshal(v, &key); err != nil {
				t.Fatal(err)
			}
			continue
		}
		var f vector33
		if err := json.Unmarshal(v, &f); err != nil {
			t.Fatal(err)
		}
		frames[name] = f
	}
	return key, frames
}

func TestPack55AA33MatchesTinytuya(t *testing.T) {
	key, frames := loadVectors33(t)
	for _, name := range []string{"client7Header", "client10NoHeader"} {
		v := frames[name]
		got, err := Pack55AA33([]byte(key), v.Seq, v.Cmd, []byte(v.Plain), !noVersionHeader33[v.Cmd])
		assertFrame(t, name, got, err, unhex(t, v.Frame))
	}
}

func TestUnpack55AA33ReadsTinytuyaFrames(t *testing.T) {
	key, frames := loadVectors33(t)
	for name, v := range frames {
		h, got, err := Unpack55AA33([]byte(key), unhex(t, v.Frame))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if h.Cmd != v.Cmd || string(got) != v.Plain {
			t.Errorf("%s: cmd %d %q, want cmd %d %q", name, h.Cmd, got, v.Cmd, v.Plain)
		}
	}
}

func TestUnpack55AA33RefusesADamagedFrame(t *testing.T) {
	key, frames := loadVectors33(t)
	frame := unhex(t, frames["device10Retcode"].Frame)
	frame[20] ^= 0xFF
	if _, _, err := Unpack55AA33([]byte(key), frame); !errors.Is(err, errBadCRC) {
		t.Fatalf("got %v, want a CRC mismatch", err)
	}
}

func TestUnpack55AA33WithAnotherKeyIsNoJSON(t *testing.T) {
	_, frames := loadVectors33(t)
	_, got, err := Unpack55AA33([]byte("another-key-1234"), unhex(t, frames["device10Retcode"].Frame))
	if err != nil {
		return // refused outright is fine too
	}
	var doc map[string]any
	if json.Unmarshal(got, &doc) == nil {
		t.Fatalf("another key decrypted to JSON: %q", got)
	}
}

const fakeKey33 = "fake-local-key!!"

// fake33 is a 3.3 monitor on a local port, which Dial reaches through
// dialPort: it reads one client frame and answers it with what reply returns.
func fake33(t *testing.T, reply func(cmd uint32, plain []byte) [][]byte) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	old := dialPort
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	dialPort, _ = strconv.Atoi(port)
	t.Cleanup(func() { dialPort = old })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		frame, err := readFrame55AA(conn)
		if err != nil {
			return
		}
		h, plain, err := Unpack55AA33([]byte(fakeKey33), frame)
		if err != nil {
			return
		}
		for _, f := range reply(h.Cmd, plain) {
			if _, err := conn.Write(f); err != nil {
				return
			}
		}
		time.Sleep(time.Second)
	}()
}

func readFrame55AA(conn net.Conn) ([]byte, error) {
	head, err := readExactly(conn, header55AALen)
	if err != nil {
		return nil, err
	}
	h, err := ParseHeader(head)
	if err != nil {
		return nil, err
	}
	rest, err := readExactly(conn, h.Total-header55AALen)
	return append(head, rest...), err
}

func monitor33(t *testing.T, key string, cmd uint32, plain string) []byte {
	t.Helper()
	f, err := pack55AA33([]byte(key), 1, cmd, []byte(plain), false, true)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func dial33() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := Dial(ctx, "127.0.0.1", "dev1", fakeKey33, V33)
	if err == nil {
		s.Close()
	}
	return err
}

func TestDial33ProvesTheMonitor(t *testing.T) {
	var asked uint32
	fake33(t, func(cmd uint32, plain []byte) [][]byte {
		asked = cmd
		var q map[string]string
		if json.Unmarshal(plain, &q) != nil || q["devId"] != "dev1" || q["gwId"] != "dev1" {
			return nil
		}
		return [][]byte{monitor33(t, fakeKey33, CmdDPQuery33, `{"devId":"dev1","dps":{"1":true}}`)}
	})
	if err := dial33(); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if asked != CmdDPQuery33 {
		t.Fatalf("the proof sent command %d, want %d", asked, CmdDPQuery33)
	}
}

func TestDial33AcceptsDataUnvalid(t *testing.T) {
	// A monitor with a 22-character id answers command 10 with this text,
	// under its own key.
	fake33(t, func(uint32, []byte) [][]byte {
		return [][]byte{monitor33(t, fakeKey33, CmdDPQuery33, `json obj data unvalid`)}
	})
	if err := dial33(); err != nil {
		t.Fatalf("Dial: %v", err)
	}
}

func TestDial33DataUnvalidUnderAnotherKeyIsNoProof(t *testing.T) {
	fake33(t, func(uint32, []byte) [][]byte {
		return [][]byte{
			monitor33(t, "another-key-1234", CmdDPQuery33, `json obj data unvalid`),
			monitor33(t, "another-key-1234", CmdDPQuery33, `json obj data unvalid`),
		}
	})
	if err := dial33(); err == nil || !strings.Contains(err.Error(), "do not decrypt") {
		t.Fatalf("got %v, want answers that do not decrypt", err)
	}
}

func TestDial33SkipsAnAcknowledgement(t *testing.T) {
	fake33(t, func(uint32, []byte) [][]byte {
		// An acknowledgement on the wire is the return code alone.
		ack := rawFrame33(1, CmdDPQuery33, make([]byte, retcodeLen))
		return [][]byte{ack, monitor33(t, fakeKey33, CmdDPQuery33, `{"devId":"dev1","dps":{}}`)}
	})
	if err := dial33(); err != nil {
		t.Fatalf("Dial: %v", err)
	}
}

func TestDial33RefusesAnotherKey(t *testing.T) {
	fake33(t, func(uint32, []byte) [][]byte {
		return [][]byte{
			monitor33(t, "another-key-1234", CmdDPQuery33, `{"devId":"dev2","dps":{}}`),
			monitor33(t, "another-key-1234", 8, `{"devId":"dev2","dps":{}}`),
		}
	})
	if err := dial33(); err == nil || !strings.Contains(err.Error(), "do not decrypt") {
		t.Fatalf("got %v, want answers that do not decrypt", err)
	}
}

func TestDial33RefusesAnotherDevice(t *testing.T) {
	fake33(t, func(uint32, []byte) [][]byte {
		return [][]byte{monitor33(t, fakeKey33, CmdDPQuery33, `{"devId":"dev2","dps":{}}`)}
	})
	if err := dial33(); err == nil || !strings.Contains(err.Error(), "another device") {
		t.Fatalf("got %v, want another device", err)
	}
}

func TestDial33WithoutAnswerFails(t *testing.T) {
	fake33(t, func(uint32, []byte) [][]byte { return nil })
	if err := dial33(); err == nil {
		t.Fatal("a monitor that never answers was accepted")
	}
}

// rawFrame33 is a 3.3 frame around body as it is, with length and CRC.
func rawFrame33(seq, cmd uint32, body []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, Prefix55AA)
	out = binary.BigEndian.AppendUint32(out, seq)
	out = binary.BigEndian.AppendUint32(out, cmd)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)+crcLen+suffixLen))
	out = append(out, body...)
	out = binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
	return binary.BigEndian.AppendUint32(out, Suffix55AA)
}

// The offer goes without the version header, the heartbeat carries "{}", and
// both are sealed under the localKey.
func TestSession33Frames(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	s := &Session{Version: V33, Key: []byte(fakeKey33), conn: a}
	for _, tc := range []struct {
		send func() error
		cmd  uint32
		body string
	}{
		{func() error { return s.Send(CmdIPCLan302, testIV, []byte(`{"header":{}}`)) }, CmdIPCLan302, `{"header":{}}`},
		{s.Heartbeat, CmdHeartbeat, `{}`},
	} {
		go func() { _ = tc.send() }()
		f, err := readFrame55AA(b)
		if err != nil {
			t.Fatal(err)
		}
		if string(f[header55AALen:header55AALen+3]) == "3.3" {
			t.Errorf("command %d carries the version header", tc.cmd)
		}
		if h, plain, err := Unpack55AA33([]byte(fakeKey33), f); err != nil || h.Cmd != tc.cmd || string(plain) != tc.body {
			t.Errorf("command %d: cmd %d %q, %v", tc.cmd, h.Cmd, plain, err)
		}
	}
}

// A 3.3 answer from the monitor, return code in clear, reaches the offer's
// reader.
func TestReadSignalOn33(t *testing.T) {
	client, monitor := net.Pipe()
	defer monitor.Close()
	s := &Session{conn: client, Key: []byte(fakeKey33), Version: V33}
	body := `{"header":{"type":"answer","path":"lan"},"msg":{"sdp":"a=ice-ufrag:abcd"}}`
	go func() { _, _ = monitor.Write(monitor33(t, fakeKey33, CmdIPCLan302, body)) }()
	frame, err := ReadSignal(s, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Header.Type != TypeAnswer || frame.Msg.SDP != "a=ice-ufrag:abcd" {
		t.Fatalf("got %+v", frame)
	}
}
