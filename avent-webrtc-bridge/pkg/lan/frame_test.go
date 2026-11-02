package lan

import (
	"bytes"
	"encoding/binary"
	"testing"
)

var (
	testKey = []byte("0123456789abcdef")
	testIV  = []byte("abcdefghijkl")
)

func TestPack6699RoundTrip(t *testing.T) {
	payload := []byte(`{"a":1}`)
	frame, err := Pack6699(testKey, testIV, 7, 32, payload)
	if err != nil {
		t.Fatal(err)
	}
	h, got, err := Unpack6699(testKey, frame)
	if err != nil {
		t.Fatal(err)
	}
	if h.Seq != 7 || h.Cmd != 32 || !bytes.Equal(got, payload) {
		t.Fatalf("got seq=%d cmd=%d payload=%q", h.Seq, h.Cmd, got)
	}
}

func TestPack6699LengthExcludesSuffix(t *testing.T) {
	payload := []byte(`{"a":1}`)
	frame, err := Pack6699(testKey, testIV, 1, 32, payload)
	if err != nil {
		t.Fatal(err)
	}
	length := binary.BigEndian.Uint32(frame[14:18])
	if want := uint32(gcmIVLen + len(payload) + gcmTagLen); length != want {
		t.Fatalf("length field %d, want %d", length, want)
	}
	if len(frame) != header6699Len+int(length)+suffixLen {
		t.Fatalf("frame is %d bytes, want header+length+suffix", len(frame))
	}
	if binary.BigEndian.Uint32(frame[len(frame)-suffixLen:]) != Suffix6699 {
		t.Fatal("frame does not end with the 6699 suffix")
	}
}

func TestUnpack6699RejectsWrongKey(t *testing.T) {
	frame, err := Pack6699(testKey, testIV, 1, 32, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Unpack6699([]byte("fedcba9876543210"), frame); err == nil {
		t.Fatal("expected a decrypt error with the wrong key")
	}
}
