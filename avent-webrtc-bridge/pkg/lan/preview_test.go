package lan

import (
	"encoding/binary"
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
	msgs, err := startupSequence(LoginToken("pw", "0123456789abcdef"))
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
