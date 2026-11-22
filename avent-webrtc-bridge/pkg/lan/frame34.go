package lan

// Protocol 3.4 framing, for monitors that speak nothing newer: a Philips Avent
// SCD951 on firmware 2.4.1 refuses 3.5 at the key check. Written after
// tinytuya 1.20 (XenonDevice._encode_message and unpack_message), which the
// tests match byte for byte (testdata/tinytuya34.json).

import (
	"bytes"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	Suffix55AA = 0x0000AA55

	hmacLen    = sha256.Size
	retcodeLen = 4
)

// Pack55AA34 builds a protocol 3.4 frame the way tinytuya does: the payload
// PKCS#7-padded and AES-128-ECB encrypted, then an HMAC-SHA256 over the header
// and ciphertext, then the suffix. The length field counts the ciphertext, the
// HMAC and the suffix.
//
// The payload goes in as given. Whether it carries the "3.4" version header is
// the caller's choice; see versionHeader34For.
func Pack55AA34(key []byte, seq, cmd uint32, payload []byte) ([]byte, error) {
	return pack55AA34(key, seq, cmd, payload, false)
}

// pack55AA34 also writes the frames a monitor sends, which carry a return code
// in clear between the header and the ciphertext; the tests need those.
func pack55AA34(key []byte, seq, cmd uint32, payload []byte, withRetcode bool) ([]byte, error) {
	enc, err := ecbEncrypt(key, payload)
	if err != nil {
		return nil, err
	}
	body := enc
	if withRetcode {
		body = append(make([]byte, retcodeLen), enc...)
	}
	out := make([]byte, header55AALen, header55AALen+len(body)+hmacLen+suffixLen)
	binary.BigEndian.PutUint32(out[0:4], Prefix55AA)
	binary.BigEndian.PutUint32(out[4:8], seq)
	binary.BigEndian.PutUint32(out[8:12], cmd)
	binary.BigEndian.PutUint32(out[12:16], uint32(len(body)+hmacLen+suffixLen))
	out = append(out, body...)
	out = append(out, hmacSHA256(key, out)...)
	return binary.BigEndian.AppendUint32(out, Suffix55AA), nil
}

// Unpack55AA34 checks and decrypts a complete 3.4 frame.
//
// Frames from the monitor put a 4-byte return code in clear before the
// ciphertext and frames from a client do not. The ciphertext is a whole number
// of AES blocks, so the two cannot be confused: whatever is left over after the
// blocks is the return code.
func Unpack55AA34(key, frame []byte) (Header, []byte, error) {
	h, err := ParseHeader(frame)
	if err != nil {
		return Header{}, nil, err
	}
	if h.Prefix != Prefix55AA {
		return h, nil, ErrBadPrefix
	}
	if len(frame) < h.Total {
		return h, nil, ErrShortFrame
	}
	if int(h.Length) < hmacLen+suffixLen {
		return h, nil, ErrShortFrame
	}
	macAt := h.Total - suffixLen - hmacLen
	if !hmac.Equal(hmacSHA256(key, frame[:macAt]), frame[macAt:macAt+hmacLen]) {
		return h, nil, fmt.Errorf("lan: cmd %d: HMAC mismatch", h.Cmd)
	}
	body := frame[header55AALen:macAt]
	switch len(body) % aes.BlockSize {
	case 0:
	case retcodeLen:
		body = body[retcodeLen:]
	default:
		return h, nil, fmt.Errorf("lan: cmd %d carries %d bytes, not whole blocks", h.Cmd, len(body))
	}
	if len(body) == 0 {
		return h, nil, nil
	}
	plain, err := ecbDecrypt(key, body)
	if err != nil {
		return h, nil, err
	}
	// 3.4 monitors put the version header inside the encryption on some
	// replies; it carries nothing the session needs.
	return h, bytes.TrimPrefix(plain, versionHeader34), nil
}

// versionHeader34 is what tinytuya puts in front of most 3.4 payloads: the
// version text and twelve zero bytes.
var versionHeader34 = append([]byte("3.4"), make([]byte, 12)...)

// noVersionHeader34 lists the commands sent without that header. The first
// set is tinytuya's (NO_PROTOCOL_HEADER_CMDS: DP queries, heartbeat, the
// session key negotiation, LAN_EXT_STREAM). Command 32 is added from a
// measurement on an SCD951 (firmware 2.4.1, 01-11-2026): the offer framed
// with the header got no answer at all, and the same offer without it got an
// answer and two candidates straight away.
var noVersionHeader34 = map[uint32]bool{
	10: true, 16: true, 18: true, CmdHeartbeat: true,
	CmdSessKeyNegStart: true, CmdSessKeyNegResp: true, CmdSessKeyNegFinish: true,
	64: true, CmdIPCLan302: true,
}

// versionHeader34For returns what goes in front of a 3.4 payload for cmd.
func versionHeader34For(cmd uint32) []byte {
	if noVersionHeader34[cmd] {
		return nil
	}
	return versionHeader34
}

func ecbEncrypt(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	out := make([]byte, len(plain)+pad)
	copy(out, plain)
	for i := len(plain); i < len(out); i++ {
		out[i] = byte(pad)
	}
	for i := 0; i < len(out); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], out[i:i+aes.BlockSize])
	}
	return out, nil
}

// ecbDecrypt reverses ecbEncrypt. Like tinytuya it strips padding without
// insisting on it: the HMAC has already vouched for the bytes.
func ecbDecrypt(key, data []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("lan: ciphertext is not a whole number of blocks")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	if n := int(out[len(out)-1]); n >= 1 && n <= aes.BlockSize {
		out = out[:len(out)-n]
	}
	return out, nil
}
