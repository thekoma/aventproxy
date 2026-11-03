package tuya

// Frames on a device's own MQTT channel, the way the Philips app (Tuya SDK)
// writes and reads them: com.thingclips.sdk.mqtt qpbpqpq / qbpppdb (pv 2.2)
// and dbbpbbb / dbpdpbp (pv 2.3) in the app's classes9.dex. The app publishes
// on smart/mb/out/<devId> and reads the device's messages on
// smart/mb/in/<devId>. WHITEPAPER section 21 has the trace.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
)

// devicePayload is the JSON inside a frame: PublishBean2_2 / PublishBean2_3,
// whose fields fastjson writes in this order.
type devicePayload struct {
	Data     any   `json:"data"`
	Protocol int   `json:"protocol"`
	T        int64 `json:"t"` // seconds
}

// DeviceFrameHeader is the sequence pair every 2.2 and 2.3 frame carries:
// S counts the sender's publishes, O is a random number fixed per sender.
type DeviceFrameHeader struct {
	S, O uint32
}

const (
	gcmNonceLen = 12
	header22Len = 15 // "2.2" + crc + s + o
	header23Len = 12 // "2.3" + s + o + 0x00
)

// UseDevicePV says which framing a device's pv asks for: "2.3" for 2.3 and
// up, else "2.2". The app compares pv as a float, so "2.30" counts as 2.3 and
// an empty or unreadable pv falls to 2.2, the framing this bridge expects.
func UseDevicePV(pv string) string {
	f, err := strconv.ParseFloat(pv, 32)
	if err == nil && float32(f) >= float32(2.3) {
		return "2.3"
	}
	return "2.2"
}

// EncodeDeviceFrame builds one frame for the device channel. pv is "2.2" or
// "2.3" (see UseDevicePV); the key is the device's local key.
func EncodeDeviceFrame(pv string, localKey []byte, h DeviceFrameHeader, protocol int, t int64, data any) ([]byte, error) {
	plain, err := json.Marshal(devicePayload{Data: data, Protocol: protocol, T: t})
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(localKey)
	if err != nil {
		return nil, fmt.Errorf("device frame key: %w", err)
	}
	so := make([]byte, 8)
	binary.BigEndian.PutUint32(so[0:4], h.S)
	binary.BigEndian.PutUint32(so[4:8], h.O)

	switch pv {
	case "2.2":
		sealed := ecbEncrypt(block, pkcs7Pad(plain, aes.BlockSize))
		signed := append(append([]byte{}, so...), sealed...)
		crc := make([]byte, 4)
		binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE(signed))
		return concat([]byte("2.2"), crc, signed), nil
	case "2.3":
		header := concat([]byte("2.3"), so, []byte{0})
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		nonce := make([]byte, gcmNonceLen)
		if _, err := rand.Read(nonce); err != nil {
			return nil, err
		}
		return concat(header, nonce, gcm.Seal(nil, nonce, plain, header)), nil
	}
	return nil, fmt.Errorf("device frame pv %q: want 2.2 or 2.3", pv)
}

// errNotDeviceFrame is a payload the device channel cannot read.
var errNotDeviceFrame = errors.New("not a device frame")

// DecodeDeviceFrame reads one frame from the device channel and returns its
// protocol and data object. It reads 2.2, 2.3 and plain JSON, whichever the
// payload's first bytes name, as the app does.
func DecodeDeviceFrame(localKey, payload []byte) (protocol int, data json.RawMessage, err error) {
	var plain []byte
	switch {
	case len(payload) > 0 && payload[0] == '{':
		plain = payload
	case len(payload) >= 3 && string(payload[:3]) == "2.2":
		plain, err = decode22(localKey, payload)
	case len(payload) >= 3 && string(payload[:3]) == "2.3":
		plain, err = decode23(localKey, payload)
	default:
		return 0, nil, errNotDeviceFrame
	}
	if err != nil {
		return 0, nil, err
	}
	var p struct {
		Protocol int             `json:"protocol"`
		Data     json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(plain, &p); err != nil {
		return 0, nil, fmt.Errorf("device frame JSON: %w", err)
	}
	return p.Protocol, p.Data, nil
}

func decode22(localKey, payload []byte) ([]byte, error) {
	if len(payload) < header22Len+aes.BlockSize || (len(payload)-header22Len)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("%w: 2.2 frame of %d bytes", errNotDeviceFrame, len(payload))
	}
	if crc32.ChecksumIEEE(payload[7:]) != binary.BigEndian.Uint32(payload[3:7]) {
		return nil, errors.New("2.2 frame: CRC mismatch")
	}
	block, err := aes.NewCipher(localKey)
	if err != nil {
		return nil, fmt.Errorf("device frame key: %w", err)
	}
	return pkcs7Unpad(ecbDecrypt(block, payload[header22Len:]), aes.BlockSize)
}

func decode23(localKey, payload []byte) ([]byte, error) {
	if len(payload) < header23Len+gcmNonceLen+16 {
		return nil, fmt.Errorf("%w: 2.3 frame of %d bytes", errNotDeviceFrame, len(payload))
	}
	block, err := aes.NewCipher(localKey)
	if err != nil {
		return nil, fmt.Errorf("device frame key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	header, body := payload[:header23Len], payload[header23Len:]
	plain, err := gcm.Open(nil, body[:gcmNonceLen], body[gcmNonceLen:], header)
	if err != nil {
		return nil, errors.New("2.3 frame: authentication failed")
	}
	return plain, nil
}

// ecbEncrypt is AES/ECB, what Java's Cipher.getInstance("AES") means on
// Android. Go has no ECB mode, so it is done block by block.
func ecbEncrypt(block cipher.Block, plain []byte) []byte {
	out := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}
	return out
}

func ecbDecrypt(block cipher.Block, sealed []byte) []byte {
	out := make([]byte, len(sealed))
	for i := 0; i < len(sealed); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], sealed[i:i+aes.BlockSize])
	}
	return out
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	return append(append([]byte{}, b...), bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(b []byte, size int) ([]byte, error) {
	if len(b) == 0 || len(b)%size != 0 {
		return nil, errors.New("bad padding")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > size || n > len(b) || !bytes.Equal(b[len(b)-n:], bytes.Repeat([]byte{byte(n)}, n)) {
		return nil, errors.New("bad padding: wrong local key?")
	}
	return b[:len(b)-n], nil
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
