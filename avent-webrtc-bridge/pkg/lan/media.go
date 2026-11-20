package lan

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// Application-layer constants for the media channel.
const (
	// mediaMagic prefixes every control message. Media messages omit it and
	// start with their command instead.
	mediaMagic = 0x12345678

	CmdMediaVideo = 0x00010003
	CmdMediaAudio = 0x00010005

	macLen         = sha1.Size // trailing HMAC on every datagram
	mediaHeaderLen = 32        // command + stream description
	// The sub-header comes in two forms. The long one carries a length; the
	// short one does not and starts eight bytes later. Both end with the same
	// four flag bytes immediately before the payload.
	subHeaderLen      = 16
	shortSubHeaderLen = 8
	// subHeaderOverhead is what the length field counts besides the payload:
	// the 8-byte timestamp and the 4-byte flags.
	subHeaderOverhead = 12
	// descOffset is where the 8 stream-description bytes start in the header.
	descOffset = 24
)

// startupMessages are the control messages the app sends straight after the
// login to make the monitor start streaming. They carry no session-specific
// data, so they are replayed verbatim.
var startupMessages = []string{
	"7856341200000000000000000a0000000400000000000100",
	"785634120200000000000000020000000400000000000000",
	"78563412040001000000000009000000080000000000000002000000",
	"78563412030001000000000006000000080000000000000000000000",
	"78563412050001000000000006000400080000000000000004000000",
	"785634120600000000000000050000000400000000000000",
}

// LoginToken is the credential the monitor's P2P layer expects.
//
// md5(devicePassword + "||" + localKey). The password comes from the RTC
// config and the local key from the device object, so a deployment needs both
// cloud lookups once and neither of them again.
func LoginToken(devicePassword, localKey string) string {
	sum := md5.Sum([]byte(devicePassword + "||" + localKey))
	return hex.EncodeToString(sum[:])
}

// buildLogin renders the cmd-1 message: fixed 32-byte username and token
// fields, then 32 reserved bytes the monitor expects but never populates.
func buildLogin(token string) []byte {
	msg := make([]byte, 8+32+32+32)
	binary.LittleEndian.PutUint32(msg[0:4], mediaMagic)
	binary.LittleEndian.PutUint32(msg[4:8], 1)
	copy(msg[8:40], "admin")
	copy(msg[40:72], token)
	return msg
}

// seal encrypts one application message: a random IV followed by AES-CBC.
func seal(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	out := make([]byte, aes.BlockSize+len(padded))
	if _, err := rand.Read(out[:aes.BlockSize]); err != nil {
		return nil, err
	}
	cipher.NewCBCEncrypter(block, out[:aes.BlockSize]).CryptBlocks(out[aes.BlockSize:], padded)
	return out, nil
}

// open reverses seal.
func open(key, data []byte) ([]byte, error) {
	if len(data) < 2*aes.BlockSize || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("lan: media payload is not a whole number of blocks")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data)-aes.BlockSize)
	cipher.NewCBCDecrypter(block, data[:aes.BlockSize]).CryptBlocks(out, data[aes.BlockSize:])
	if n := int(out[len(out)-1]); n >= 1 && n <= aes.BlockSize && n <= len(out) {
		out = out[:len(out)-n]
	}
	return out, nil
}

func mac(key, body []byte) []byte {
	h := hmac.New(sha1.New, key)
	h.Write(body)
	return h.Sum(nil)
}

// sign appends the HMAC-SHA1 trailer the monitor puts on, and expects on,
// every datagram.
func sign(key, segment []byte) []byte {
	return append(append([]byte{}, segment...), mac(key, segment)...)
}

// verify strips the trailer and reports whether it matched.
func verify(key, datagram []byte) ([]byte, bool) {
	if len(datagram) < macLen {
		return nil, false
	}
	n := len(datagram) - macLen
	body := datagram[:n:n]
	return body, hmac.Equal(mac(key, body), datagram[n:])
}

// VideoFrame is one decoded unit of the monitor's video stream.
type VideoFrame struct {
	Width     int
	Height    int
	FPS       int
	Timestamp uint64
	// NAL is an RTP payload, whole as the monitor sent it: parameter sets
	// arrive complete and slices arrive already fragmented as FU-A.
	NAL []byte
}

// AudioFrame is one decoded unit of the monitor's audio stream.
type AudioFrame struct {
	Timestamp uint64
	Codec     int
	// SampleRate is 0 when the header names an index this does not know.
	SampleRate int
	Channels   int
	// Samples is signed 16-bit little-endian linear PCM, not a compressed
	// codec: silence arrives as zero bytes, which no G.711 variant produces.
	Samples []byte
}

// sampleRates maps the description's rate index to Hz. Index 3 is what this
// hardware sends and 16 kHz was confirmed by measuring the stream; the rest
// follow the same enum order and are untested here.
var sampleRates = [...]int{8000, 11025, 12000, 16000, 22050, 24000, 32000, 44100, 48000}

func sampleRate(index uint16) int {
	if int(index) < len(sampleRates) {
		return sampleRates[index]
	}
	return 0
}

// mediaPayload splits a decrypted media message into its command, timestamp
// and data.
//
// Two sub-header forms are in use and the length field is what tells them
// apart: in the long form it accounts for exactly what follows, and in the
// short form those bytes are payload and read as nonsense. Reading only the
// long form leaves every short-form message looking like a gigabyte unit,
// which is most of the picture data on this hardware.
func mediaPayload(msg []byte) (cmd uint32, ts uint64, long bool, payload []byte, ok bool) {
	if len(msg) < mediaHeaderLen+shortSubHeaderLen {
		return 0, 0, false, nil, false
	}
	cmd = binary.LittleEndian.Uint32(msg[:4])
	sub := msg[mediaHeaderLen:]

	if len(sub) >= subHeaderLen {
		length := int(binary.LittleEndian.Uint32(sub[:4]))
		if length >= subHeaderOverhead && length-subHeaderOverhead == len(sub)-subHeaderLen {
			return cmd, binary.LittleEndian.Uint64(sub[4:12]), true, sub[subHeaderLen:], true
		}
	}

	// Short form: a 4-byte clock and the flags, then the payload.
	payload = sub[shortSubHeaderLen:]
	if len(payload) == 0 {
		return 0, 0, false, nil, false
	}
	return cmd, uint64(binary.LittleEndian.Uint32(sub[:4])), false, payload, true
}

// parseVideo turns a decrypted video message into a frame.
func parseVideo(msg []byte, ts uint64, payload []byte) *VideoFrame {
	return &VideoFrame{
		Width:     int(binary.LittleEndian.Uint16(msg[26:28])),
		Height:    int(binary.LittleEndian.Uint16(msg[28:30])),
		FPS:       int(msg[30]),
		Timestamp: ts,
		NAL:       payload,
	}
}

// parseAudio turns a decrypted audio message into a frame. The description
// mirrors the video one: codec first, then the rate index and channel count in
// the fields video uses for width and fps.
func parseAudio(msg []byte, ts uint64, payload []byte) *AudioFrame {
	return &AudioFrame{
		Timestamp:  ts,
		Codec:      int(msg[descOffset]),
		SampleRate: sampleRate(binary.LittleEndian.Uint16(msg[descOffset+2 : descOffset+4])),
		Channels:   int(msg[descOffset+6]),
		Samples:    payload,
	}
}

// startupSequence returns the login followed by the stream-start messages.
func startupSequence(token string) ([][]byte, error) {
	out := [][]byte{buildLogin(token)}
	for _, h := range startupMessages {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("lan: bad startup template: %w", err)
		}
		out = append(out, raw)
	}
	return out, nil
}
