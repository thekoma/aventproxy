package lan

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestSessionKeyMatchesSDK(t *testing.T) {
	localNonce := []byte("0123456789ABCDEF")
	remoteNonce := []byte("fedcba9876543210")

	mixed := make([]byte, nonceLen)
	for i := range mixed {
		mixed[i] = localNonce[i] ^ remoteNonce[i]
	}
	block, err := aes.NewCipher(testKey)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, gcmIVLen)
	if err != nil {
		t.Fatal(err)
	}
	// Go's Seal returns ciphertext||tag with no IV prefix, so the key is the first 16 bytes.
	want := gcm.Seal(nil, localNonce[:gcmIVLen], mixed, nil)[:nonceLen]

	got, err := sessionKey(testKey, localNonce, remoteNonce)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("sessionKey = %x, want %x", got, want)
	}
}

func TestDialHonoursContextDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(3 * time.Second) // accept and never write
		}
	}()

	old := dialPort
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	dialPort, _ = strconv.Atoi(port)
	defer func() { dialPort = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Dial(ctx, "127.0.0.1", "dev", string(testKey)); err == nil {
		t.Fatal("expected an error from a silent monitor")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Dial took %v, want under 1s", d)
	}
}

func TestRecvResyncsPastAnImplausibleLength(t *testing.T) {
	client, monitor := pipeSessions()
	defer client.Close()
	defer monitor.Close()

	// A 6699 header whose length field is zero, as a desynced stream would
	// produce, followed by a real frame.
	bogus := make([]byte, header6699Len)
	binary.BigEndian.PutUint32(bogus[0:4], Prefix6699)
	go func() {
		_, _ = monitor.conn.Write(bogus)
		_ = monitor.Send(CmdHeartbeat, testIV, []byte("ok"))
	}()

	h, payload, err := client.Recv(time.Second)
	if err != nil {
		t.Fatalf("Recv after a bad length: %v", err)
	}
	if h.Cmd != CmdHeartbeat || string(payload) != "ok" {
		t.Fatalf("got cmd %d payload %q, want the frame after the bad header", h.Cmd, payload)
	}
	if len(client.readBuf) != 0 {
		t.Fatalf("%d bytes left in the read buffer", len(client.readBuf))
	}
}

func TestDialReturnsTheContextErrorWhenCancelled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second) // accept and never write
		}
	}()

	old := dialPort
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	dialPort, _ = strconv.Atoi(port)
	defer func() { dialPort = old }()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err = Dial(ctx, "127.0.0.1", "dev", string(testKey))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Dial = %v, want context.Canceled", err)
	}
}
