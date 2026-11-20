package lan

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// message builds a media message from a sub-header and a payload. The 32-byte
// header carries the command and the stream description.
func message(subHeader, payload []byte) []byte {
	msg := make([]byte, mediaHeaderLen)
	binary.LittleEndian.PutUint32(msg[:4], CmdMediaVideo)
	binary.LittleEndian.PutUint16(msg[26:28], 1280)
	binary.LittleEndian.PutUint16(msg[28:30], 720)
	msg[30] = 20
	return append(append(msg, subHeader...), payload...)
}

// longSubHeader is the form that states a length: length, timestamp, flags.
func longSubHeader(payloadLen int, ts uint64) []byte {
	sub := make([]byte, subHeaderLen)
	binary.LittleEndian.PutUint32(sub[:4], uint32(payloadLen+subHeaderOverhead))
	binary.LittleEndian.PutUint64(sub[4:12], ts)
	copy(sub[12:16], []byte{0x00, 0x00, 0x00, 0x0a})
	return sub
}

// shortSubHeader is the form without a length: a 4-byte clock, then the flags.
func shortSubHeader(ts uint32) []byte {
	sub := make([]byte, shortSubHeaderLen)
	binary.LittleEndian.PutUint32(sub[:4], ts)
	copy(sub[4:8], []byte{0x00, 0x00, 0x00, 0x0a})
	return sub
}

func TestLongFormIsRecognisedByItsLength(t *testing.T) {
	payload := bytes.Repeat([]byte{0xab}, 1102)
	payload[0], payload[1] = 0x3c, 0x85

	cmd, ts, long, got, ok := mediaPayload(message(longSubHeader(len(payload), 9999), payload))
	if !ok || cmd != CmdMediaVideo {
		t.Fatalf("did not parse as video: ok=%v cmd=%#x", ok, cmd)
	}
	if !long {
		t.Fatal("a sub-header whose length accounts for the payload is the long form")
	}
	if ts != 9999 {
		t.Fatalf("timestamp = %d, want 9999", ts)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %d bytes, want %d", len(got), len(payload))
	}
}

// The short form has no length, so its first four bytes are a clock that reads
// as an impossible length. Taking it at face value is what dropped four fifths
// of the video.
func TestShortFormIsNotMistakenForALength(t *testing.T) {
	payload := bytes.Repeat([]byte{0xcd}, 1102)
	payload[0], payload[1] = 0x3c, 0x01

	_, _, long, got, ok := mediaPayload(message(shortSubHeader(0x985c1025), payload))
	if !ok {
		t.Fatal("a short-form message must still parse")
	}
	if long {
		t.Fatal("read as the long form; its length field is payload data")
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = % x..., want it to start 3c 01 and run %d bytes", got[:2], len(payload))
	}
}

// Bytes taken off the wire, from the sub-header to the start of the payload.
func TestCapturedSubHeaders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sub    string
		long   bool
		leader string
	}{
		{"keyframe fragment", "5a0400008060fe46250738c00000000a", true, "3c05"},
		{"predicted fragment", "25105c980000000a", false, "3c01"},
		{"predicted slice start", "25367e500000000a", false, "3c81"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := hex.DecodeString(tc.sub)
			if err != nil {
				t.Fatal(err)
			}
			leader, err := hex.DecodeString(tc.leader)
			if err != nil {
				t.Fatal(err)
			}
			payload := append(leader, bytes.Repeat([]byte{0x5a}, 1102-len(leader))...)

			_, _, long, got, ok := mediaPayload(message(sub, payload))
			if !ok {
				t.Fatal("did not parse")
			}
			if long != tc.long {
				t.Fatalf("long form = %v, want %v", long, tc.long)
			}
			if len(got) != 1102 {
				t.Fatalf("payload = %d bytes, want 1102", len(got))
			}
			if !bytes.HasPrefix(got, leader) {
				t.Fatalf("payload starts % x, want % x", got[:2], leader)
			}
		})
	}
}

func TestTooShortToParse(t *testing.T) {
	if _, _, _, _, ok := mediaPayload(make([]byte, mediaHeaderLen+4)); ok {
		t.Fatal("a message with no room for a sub-header must not parse")
	}
}

// The HMAC trailer sits on the datagram, around the sealed payload, so the
// tamper check goes through sign and verify rather than open.
func TestSealOpenRoundTrip(t *testing.T) {
	msg := []byte("a media message longer than one AES block")
	sealed, err := seal(testKey, msg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := open(testKey, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("open(seal(msg)) = %q, want %q", got, msg)
	}

	datagram := sign(testKey, sealed)
	body, ok := verify(testKey, datagram)
	if !ok || !bytes.Equal(body, sealed) {
		t.Fatalf("verify(sign(x)) = %v, ok=%v; want x back", body, ok)
	}
	datagram[len(datagram)-1] ^= 0x01
	if _, ok := verify(testKey, datagram); ok {
		t.Fatal("a flipped byte in the HMAC trailer must fail verification")
	}
}

func TestLoginToken(t *testing.T) {
	sum := md5.Sum([]byte("pw||lk"))
	if got, want := LoginToken("pw", "lk"), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("LoginToken = %s, want %s", got, want)
	}
}
