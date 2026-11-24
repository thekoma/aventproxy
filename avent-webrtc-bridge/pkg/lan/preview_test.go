package lan

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// cmdTalk is IOCtrlAudioSpeaker in libThingCameraSDK, the command the app's
// talk button sends. A session that sends it opens the monitor's speaker and
// pauses a playing lullaby (issue #72); the app's preview never does.
const cmdTalk = 8

// The control messages are framed magic | requestId u32 | 0 u32 | cmd u16 |
// sub u16 | len u32 | payload. The login (cmd 1) and the app's StartPreview
// commands are all a session may send.
func TestStartupSequenceNeverTalks(t *testing.T) {
	msgs, err := startupSequence(LoginToken("pw", "0123456789abcdef"), ClarityHD)
	if err != nil {
		t.Fatal(err)
	}
	for i, msg := range msgs {
		if len(msg) < 16 {
			t.Fatalf("message %d too short: % x", i, msg)
		}
		if binary.LittleEndian.Uint32(msg[0:4]) != mediaMagic {
			t.Fatalf("message %d has no magic: % x", i, msg)
		}
		if i == 0 {
			continue // the login is laid out differently: magic, then cmd 1 as a u32
		}
		if cmd := binary.LittleEndian.Uint16(msg[12:14]); cmd == cmdTalk {
			t.Errorf("startup message %d is the talk command: % x", i, msg)
		}
	}
}

// The third startup message asks for the clarity; everything else is the
// app's, byte for byte.
func TestStartupSequenceAsksForTheClarity(t *testing.T) {
	standard, err := startupSequence("token", 0)
	if err != nil {
		t.Fatal(err)
	}
	hd, err := startupSequence("token", ClarityHD)
	if err != nil {
		t.Fatal(err)
	}
	for i := range standard {
		msg := standard[i]
		if i == clarityMessage+1 { // after the login
			if got := binary.LittleEndian.Uint32(msg[len(msg)-4:]); got != ClarityStandard {
				t.Fatalf("default clarity %d, want %d", got, ClarityStandard)
			}
			if got := binary.LittleEndian.Uint32(hd[i][len(hd[i])-4:]); got != ClarityHD {
				t.Fatalf("HD clarity %d, want %d", got, ClarityHD)
			}
			if string(msg[:len(msg)-4]) != string(hd[i][:len(hd[i])-4]) {
				t.Fatal("the clarity changed more than its word")
			}
			continue
		}
		if string(msg) != string(hd[i]) {
			t.Fatalf("message %d differs with HD", i)
		}
	}
	if hex.EncodeToString(standard[clarityMessage+1]) != startupMessages[clarityMessage] {
		t.Fatal("the default is not the app's message")
	}
}
