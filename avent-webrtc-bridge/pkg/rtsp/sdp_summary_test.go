package rtsp

import (
	"strings"
	"testing"
)

// An offer as the bridge sends it with talkback off, with fake secrets.
const testOfferSDP = "v=0\r\n" +
	"o=- 1 2 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"a=group:BUNDLE 0 1\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 101 0 8\r\n" +
	"a=ice-ufrag:FAKEUFRAG\r\n" +
	"a=ice-pwd:SECRETPWD\r\n" +
	"a=fingerprint:sha-256 AA:BB:CC\r\n" +
	"a=mid:0\r\n" +
	"a=recvonly\r\n" +
	"a=rtpmap:101 opus/48000/2\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=rtpmap:8 PCMA/8000\r\n" +
	"a=candidate:1 1 udp 2130706431 192.0.2.7 50000 typ host\r\n" +
	"a=ssrc:1234 cname:fake\r\n" +
	"a=aes-key:00112233445566778899aabbccddeeff\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
	"a=mid:1\r\n" +
	"a=recvonly\r\n" +
	"a=rtpmap:96 H264/90000\r\n"

func TestSummarizeSDPOmitsSecrets(t *testing.T) {
	got := summarizeSDP(testOfferSDP)
	for _, secret := range []string{"FAKEUFRAG", "SECRETPWD", "fingerprint", "AA:BB", "candidate", "192.0.2.7", "ssrc", "1234", "aes-key", "0011223344"} {
		if strings.Contains(got, secret) {
			t.Errorf("summary leaks %q: %s", secret, got)
		}
	}
	want := "audio(mid=0 recvonly opus/48000/2,PCMU/8000,PCMA/8000) video(mid=1 recvonly H264/90000)"
	if got != want {
		t.Errorf("summary\n got %s\nwant %s", got, want)
	}
}

func TestSummarizeSDPDefaultsToSendrecv(t *testing.T) {
	got := summarizeSDP("v=0\nm=audio 9 RTP/AVP 0\na=mid:a\na=rtpmap:0 PCMU/8000\n")
	if got != "audio(mid=a sendrecv PCMU/8000)" {
		t.Errorf("got %s", got)
	}
}

func TestSummarizeSDPWithoutMedia(t *testing.T) {
	if got := summarizeSDP("v=0\r\ns=-\r\n"); got != "no media sections (10 bytes)" {
		t.Errorf("got %s", got)
	}
}

// The imm description, as pkg/lan offers it and the monitor answers, with
// fake secrets.
func TestSummarizeIMMSDPOmitsSecrets(t *testing.T) {
	sdp := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\na=group:BUNDLE imm0\r\n" +
		"a=msid-semantic: WMS devfakesession\r\nm=application 9 imm 6001\r\nc=IN IP4 0.0.0.0\r\n" +
		"a=ice-ufrag:FAKEUFRAG\r\na=ice-pwd:SECRETPWD\r\na=ice-options:trickle\r\n" +
		"a=aes-key:00112233445566778899aabbccddeeff\r\na=mid:imm0\r\na=rtpmap:6001 AES/KCP 330\r\n" +
		"a=ssrc:0 cname:fakeuid\r\n"
	got := summarizeSDP(sdp)
	for _, secret := range []string{"FAKEUFRAG", "SECRETPWD", "aes-key", "0011223344", "fakeuid", "devfakesession"} {
		if strings.Contains(got, secret) {
			t.Errorf("summary leaks %q: %s", secret, got)
		}
	}
	if want := "application(mid=imm0 sendrecv AES/KCP)"; got != want {
		t.Errorf("summary\n got %s\nwant %s", got, want)
	}
}
