package lan

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// A monitor that never answers the offer ends the wait with an error that
// names the step and keeps the cause underneath.
func TestCollectAnswerNamesTheStepWhenNoAnswerComes(t *testing.T) {
	client, monitor := pipeSessions()
	defer client.Close()
	defer monitor.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, _, err := collectAnswer(ctx, client, "mine")
	if want := "lan: waiting for the answer to the offer: "; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("err = %v, want it to start with %q", err, want)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("err = %v, want the read deadline underneath", err)
	}
}

// A connection that closes during the wait is named the same way.
func TestCollectAnswerNamesTheStepWhenTheConnectionCloses(t *testing.T) {
	client, monitor := pipeSessions()
	defer client.Close()
	_ = monitor.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, _, err := collectAnswer(ctx, client, "mine")
	if want := "lan: waiting for the answer to the offer: "; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("err = %v, want it to start with %q", err, want)
	}
}

// An answer and a candidate come through as before.
func TestCollectAnswerTakesTheAnswerAndCandidate(t *testing.T) {
	client, monitor := pipeSessions()
	defer client.Close()
	defer monitor.Close()
	key := strings.Repeat("ab", 16)
	frames := []SignalFrame{
		{Header: SignalHeader{Type: TypeAnswer, SessionID: "mine"}, Msg: SignalMessage{SDP: "v=0\r\na=ice-ufrag:ru\r\na=ice-pwd:rp\r\na=aes-key:" + key + "\r\n"}},
		{Header: SignalHeader{Type: TypeCandidate, SessionID: "mine"}, Msg: SignalMessage{Candidate: "a=candidate:1 1 udp 2113937151 192.0.2.7 41234 typ host"}},
	}
	go func() {
		for _, f := range frames {
			b, _ := json.Marshal(f)
			_ = monitor.Send(CmdIPCLan302, []byte("0123456789ab"), b)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	remote, aesKey, peer, err := collectAnswer(ctx, client, "mine")
	if err != nil {
		t.Fatal(err)
	}
	want := &net.UDPAddr{IP: net.ParseIP("192.0.2.7"), Port: 41234}
	if remote.Ufrag != "ru" || remote.Pwd != "rp" || hex.EncodeToString(aesKey) != key || peer.String() != want.String() {
		t.Errorf("got %+v %x %v", remote, aesKey, peer)
	}
}

// A cause that is pkg/lan's own error says "lan:" once.
func TestStepErrorSaysLanOnce(t *testing.T) {
	inner := errors.New("lan: no signalling frame within 8s")
	err := stepError{"waiting for the answer to the offer", inner}
	if got, want := err.Error(), "lan: waiting for the answer to the offer: no signalling frame within 8s"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	if !errors.Is(err, inner) {
		t.Fatal("the cause is not underneath")
	}
}
