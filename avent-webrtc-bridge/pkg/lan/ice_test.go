package lan

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v4"
)

// stunPeer answers each binding request it receives with reply, after delay.
// reply gets the request's transaction id; nil means stay silent.
func stunPeer(t *testing.T, delay time.Duration, reply func(tx [stun.TransactionIDSize]byte) *stun.Message) *net.UDPAddr {
	t.Helper()
	sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sock.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := sock.ReadFromUDP(buf)
			if err != nil {
				return
			}
			msg := &stun.Message{Raw: append([]byte{}, buf[:n]...)}
			if msg.Decode() != nil || msg.Type.Class != stun.ClassRequest {
				continue
			}
			resp := reply(msg.TransactionID)
			if resp == nil {
				continue
			}
			go func() {
				time.Sleep(delay)
				_, _ = sock.WriteToUDP(resp.Raw, from)
			}()
		}
	}()
	return sock.LocalAddr().(*net.UDPAddr)
}

func success(tx [stun.TransactionIDSize]byte) *stun.Message {
	m, _ := stun.Build(stun.BindingSuccess, stun.NewTransactionIDSetter(tx), stun.Fingerprint)
	return m
}

func rejection(tx [stun.TransactionIDSize]byte) *stun.Message {
	m, _ := stun.Build(stun.NewType(stun.MethodBinding, stun.ClassErrorResponse), stun.NewTransactionIDSetter(tx),
		stun.CodeUnauthorized, stun.Fingerprint)
	return m
}

func checkingSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sock.Close() })
	return sock
}

var (
	testLocal  = iceCreds{Ufrag: "loca", Pwd: "localpasswordlocalpasswd"}
	testRemote = iceCreds{Ufrag: "mntr", Pwd: monitorPwd}
)

// The LAN case: one candidate that rejects ends the checks at once, with the
// rejection, as it always did.
func TestICEOneRejectingCandidateFailsAtOnce(t *testing.T) {
	bad := stunPeer(t, 0, rejection)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := iceConnect(ctx, checkingSocket(t), onePeer(bad), testLocal, testRemote)
	if err == nil || !strings.Contains(err.Error(), "monitor rejected the ICE check: 401") {
		t.Fatalf("err = %v, want the rejection", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took %v: a lone rejection must end the checks at once", time.Since(start))
	}
}

// The review's probe (M3): a candidate that rejects at once does not end the
// checks while another one may still answer.
func TestICERejectionLeavesTheOtherCandidates(t *testing.T) {
	bad := stunPeer(t, 0, rejection)
	good := stunPeer(t, 300*time.Millisecond, success)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer, err := iceConnect(ctx, checkingSocket(t), func() []*net.UDPAddr { return []*net.UDPAddr{bad, good} }, testLocal, testRemote)
	if err != nil {
		t.Fatalf("err = %v, want the good candidate", err)
	}
	if peer.String() != good.String() {
		t.Fatalf("peer = %v, want %v", peer, good)
	}
}

// When nobody answers in time, the last rejection is what is reported.
func TestICEReportsTheLastRejectionAtTheEnd(t *testing.T) {
	bad := stunPeer(t, 0, rejection)
	silent := stunPeer(t, 0, func([stun.TransactionIDSize]byte) *stun.Message { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_, err := iceConnect(ctx, checkingSocket(t), func() []*net.UDPAddr { return []*net.UDPAddr{bad, silent} }, testLocal, testRemote)
	if err == nil || !strings.Contains(err.Error(), "monitor rejected the ICE check: 401") {
		t.Fatalf("err = %v, want the last rejection", err)
	}
}

// A success from an address we never checked chooses nothing: the checks go
// on until the real answer.
func TestICEIgnoresASuccessFromAStranger(t *testing.T) {
	good := stunPeer(t, 300*time.Millisecond, success)
	sock := checkingSocket(t)
	stranger, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Close()
	var forged [stun.TransactionIDSize]byte
	copy(forged[:], "not-our-txid")
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				_, _ = stranger.WriteToUDP(success(forged).Raw, sock.LocalAddr().(*net.UDPAddr))
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer, err := iceConnect(ctx, sock, onePeer(good), testLocal, testRemote)
	if err != nil || peer.String() != good.String() {
		t.Fatalf("peer = %v, err = %v; want %v", peer, err, good)
	}
}

// dev3's LAN behaviour (review N1): a monitor that does not echo the
// transaction id still connects, and still rejects at once, because its reply
// comes from the candidate we checked.
func TestICEUnechoedReplyFromTheCandidateCounts(t *testing.T) {
	fresh := func([stun.TransactionIDSize]byte) [stun.TransactionIDSize]byte {
		var tx [stun.TransactionIDSize]byte
		copy(tx[:], "fresh-txid12")
		return tx
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ok := stunPeer(t, 0, func(tx [stun.TransactionIDSize]byte) *stun.Message { return success(fresh(tx)) })
	peer, err := iceConnect(ctx, checkingSocket(t), onePeer(ok), testLocal, testRemote)
	if err != nil || peer.String() != ok.String() {
		t.Fatalf("success without the echo: peer %v, err %v; want %v", peer, err, ok)
	}

	bad := stunPeer(t, 0, func(tx [stun.TransactionIDSize]byte) *stun.Message { return rejection(fresh(tx)) })
	start := time.Now()
	_, err = iceConnect(ctx, checkingSocket(t), onePeer(bad), testLocal, testRemote)
	if err == nil || !strings.Contains(err.Error(), "monitor rejected the ICE check: 401") || time.Since(start) > time.Second {
		t.Fatalf("rejection without the echo: err %v after %v; want it at once", err, time.Since(start))
	}
}
