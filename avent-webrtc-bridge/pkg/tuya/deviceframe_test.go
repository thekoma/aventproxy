package tuya

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"strings"
	"testing"
)

// A made-up local key: 16 ASCII characters, as Tuya's are.
var fakeLocalKey = []byte("fakelocalkey0123")

// The 2.2 layout of qpbpqpq, checked byte by byte against a vector made with
// pycryptodome and zlib (scratchpad/imm-work/vector22.py), not with this
// package's code: "2.2" | CRC32(s|o|C) | s | o | C, C = AES-ECB-PKCS7.
func TestEncodeDeviceFrame22KnownAnswer(t *testing.T) {
	want := "322e323e9cecee0000000300001092dde201e200d91978697ec149a5fadf13ac415de9361cdc5ce6160c945fa4d032a0cc" +
		"36989466176cbd9925a5f5c33388b7fff4d0dfecaaee598be0bb51c3672d897fab60a0f8e3d5d88c95e61bd692bb"
	data := map[string]any{"header": map[string]any{"type": "offer"}}
	got, err := EncodeDeviceFrame("2.2", fakeLocalKey, DeviceFrameHeader{S: 3, O: 4242}, 302, 1700000000, data)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != want {
		t.Fatalf("frame\n got %x\nwant %s", got, want)
	}
}

func TestEncodeDeviceFrame22Layout(t *testing.T) {
	frame, err := EncodeDeviceFrame("2.2", fakeLocalKey, DeviceFrameHeader{S: 7, O: 123456}, 302, 1700000001,
		json.RawMessage(`{"header":{"type":"candidate","sessionid":"s1"},"msg":{"candidate":"a=candidate:1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(frame[:3]) != "2.2" {
		t.Fatalf("prefix %q", frame[:3])
	}
	if crc32.ChecksumIEEE(frame[7:]) != binary.BigEndian.Uint32(frame[3:7]) {
		t.Error("CRC does not cover s|o|ciphertext")
	}
	if s, o := binary.BigEndian.Uint32(frame[7:11]), binary.BigEndian.Uint32(frame[11:15]); s != 7 || o != 123456 {
		t.Errorf("s=%d o=%d", s, o)
	}
	block, _ := aes.NewCipher(fakeLocalKey)
	plain := make([]byte, len(frame)-15)
	for i := 0; i < len(plain); i += 16 {
		block.Decrypt(plain[i:i+16], frame[15+i:15+i+16])
	}
	plain = plain[:len(plain)-int(plain[len(plain)-1])]
	if !strings.HasPrefix(string(plain), `{"data":{"header":`) || !strings.HasSuffix(string(plain), `,"protocol":302,"t":1700000001}`) {
		t.Errorf("payload %s", plain)
	}
}

// 2.3 (dbbpbbb): header "2.3"|s|o|0x00 is the GCM AAD; nonce first, tag last.
func TestEncodeDeviceFrame23Layout(t *testing.T) {
	frame, err := EncodeDeviceFrame("2.3", fakeLocalKey, DeviceFrameHeader{S: 3, O: 9999}, 302, 1700000002, map[string]any{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	header := frame[:12]
	if string(header[:3]) != "2.3" || binary.BigEndian.Uint32(header[3:7]) != 3 || binary.BigEndian.Uint32(header[7:11]) != 9999 || header[11] != 0 {
		t.Fatalf("header % x", header)
	}
	block, _ := aes.NewCipher(fakeLocalKey)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, frame[12:24], frame[24:], header)
	if err != nil {
		t.Fatalf("GCM with the header as AAD: %v", err)
	}
	if string(plain) != `{"data":{"k":"v"},"protocol":302,"t":1700000002}` {
		t.Errorf("payload %s", plain)
	}
}

func TestDecodeDeviceFrameRoundTrip(t *testing.T) {
	data := json.RawMessage(`{"header":{"type":"answer","sessionid":"s1"},"msg":{"sdp":"v=0"}}`)
	for _, pv := range []string{"2.2", "2.3"} {
		frame, err := EncodeDeviceFrame(pv, fakeLocalKey, DeviceFrameHeader{S: 4, O: 1001}, 302, 1, data)
		if err != nil {
			t.Fatal(err)
		}
		protocol, got, err := DecodeDeviceFrame(fakeLocalKey, frame)
		if err != nil || protocol != 302 || string(got) != string(data) {
			t.Errorf("%s: %d %s %v", pv, protocol, got, err)
		}
	}
	// Plain JSON, which the app also accepts on this topic.
	protocol, got, err := DecodeDeviceFrame(fakeLocalKey, []byte(`{"protocol":4,"data":{"dps":{"1":true}}}`))
	if err != nil || protocol != 4 || string(got) != `{"dps":{"1":true}}` {
		t.Errorf("plain: %d %s %v", protocol, got, err)
	}
}

func TestDecodeDeviceFrameRejects(t *testing.T) {
	good, _ := EncodeDeviceFrame("2.2", fakeLocalKey, DeviceFrameHeader{S: 3, O: 1000}, 302, 1, map[string]any{})
	corrupt := append([]byte{}, good...)
	corrupt[len(corrupt)-1] ^= 1
	good23, _ := EncodeDeviceFrame("2.3", fakeLocalKey, DeviceFrameHeader{S: 3, O: 1000}, 302, 1, map[string]any{})
	cases := map[string]struct {
		key, payload []byte
	}{
		"crc":        {fakeLocalKey, corrupt},
		"wrong key":  {[]byte("otherfakekey0000"), good},
		"2.3 key":    {[]byte("otherfakekey0000"), good23},
		"truncated":  {fakeLocalKey, good[:20]},
		"unknown pv": {fakeLocalKey, []byte("3.3xxxxxxxxxxxxxxxxxxxxxxxx")},
		"empty":      {fakeLocalKey, nil},
	}
	for name, tc := range cases {
		if _, _, err := DecodeDeviceFrame(tc.key, tc.payload); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestUseDevicePV(t *testing.T) {
	for pv, want := range map[string]string{"2.2": "2.2", "2.3": "2.3", "2.30": "2.3", "2.4": "2.3", "": "2.2", "x": "2.2", "2.1": "2.2"} {
		if got := UseDevicePV(pv); got != want {
			t.Errorf("UseDevicePV(%q) = %q, want %q", pv, got, want)
		}
	}
}
