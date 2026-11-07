package rtsp

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/pion/rtp"
)

func g711Packet(pt uint8, seq uint16, ts uint32, payload []byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: seq, Timestamp: ts, SSRC: 7}, Payload: payload}
}

func TestG711DecodersRoundTrip(t *testing.T) {
	for _, s := range []int16{0, 100, -100, 1000, -1000, 8000, -8000, 30000, -30000} {
		got := decodeMuLaw(encodeMuLaw([]int16{s})[0])
		if d := math.Abs(float64(got) - float64(s)); d > math.Max(40, math.Abs(float64(s))/16) {
			t.Errorf("mu-law %d -> %d", s, got)
		}
	}
	if decodeMuLaw(0xFF) != 0 || decodeMuLaw(0x7F) != 0 {
		t.Errorf("mu-law silence: %d %d", decodeMuLaw(0xFF), decodeMuLaw(0x7F))
	}
	// A-law: 0xD5 and 0x55 are the smallest positive and negative values.
	if decodeALaw(0xD5) != 8 || decodeALaw(0x55) != -8 || decodeALaw(0xAA) != 32256 || decodeALaw(0x2A) != -32256 {
		t.Errorf("a-law: %d %d %d %d", decodeALaw(0xD5), decodeALaw(0x55), decodeALaw(0xAA), decodeALaw(0x2A))
	}
}

func TestTalkPCMPacketsAndTiming(t *testing.T) {
	var c talkPCM
	// A 20 ms G.711 packet (160 bytes) is 320 samples at 16 kHz: four 5 ms packets of 160 bytes.
	out := c.convert(g711Packet(0, 500, 9000, make([]byte, 160)))
	if len(out) != 4 {
		t.Fatalf("20 ms: %d packets, want 4", len(out))
	}
	for i, p := range out {
		if len(p.Payload) != talkPCMPacketBytes || p.SequenceNumber != uint16(500+i) || p.Timestamp != uint32(9000+40*i) ||
			p.PayloadType != 0 || p.SSRC != 7 {
			t.Errorf("packet %d: %d bytes, seq %d, ts %d, pt %d", i, len(p.Payload), p.SequenceNumber, p.Timestamp, p.PayloadType)
		}
	}
	// A 30 ms packet (240 bytes, as a browser sends) gives six; the numbers run on.
	out = c.convert(g711Packet(0, 501, 9160, make([]byte, 240)))
	if len(out) != 6 || out[0].SequenceNumber != 504 || out[0].Timestamp != 9160 {
		t.Fatalf("30 ms: %d packets, first seq %d ts %d", len(out), out[0].SequenceNumber, out[0].Timestamp)
	}
	// An odd size leaves bytes for the next packet: 50 bytes are 200 bytes of PCM, one packet and 40 waiting.
	if n := len(c.convert(g711Packet(0, 502, 0, make([]byte, 50)))); n != 1 {
		t.Fatalf("50 bytes: %d packets, want 1", n)
	}
	if n := len(c.convert(g711Packet(0, 503, 0, make([]byte, 30)))); n != 1 { // 40 + 120 = 160
		t.Fatalf("the rest: %d packets, want 1", n)
	}
}

func TestTalkPCMSamples(t *testing.T) {
	var c talkPCM
	// A 1 kHz tone at 8 kHz comes out as the same tone at 16 kHz: every second
	// sample is the original, the ones between lie between their neighbours.
	in := make([]int16, 160)
	for i := range in {
		in[i] = int16(8000 * math.Sin(2*math.Pi*1000*float64(i)/8000))
	}
	var pcm []byte
	for _, p := range c.convert(g711Packet(0, 1, 0, encodeMuLaw(in))) {
		pcm = append(pcm, p.Payload...)
	}
	if len(pcm) != 640 {
		t.Fatalf("%d bytes of PCM, want 640", len(pcm))
	}
	samples := make([]int16, len(pcm)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcm[2*i:]))
	}
	for i, s := range in {
		if d := math.Abs(float64(samples[2*i+1]) - float64(s)); d > 300 {
			t.Fatalf("sample %d: %d, want about %d", i, samples[2*i+1], s)
		}
		if i > 0 {
			lo, hi := min(samples[2*i-1], samples[2*i+1]), max(samples[2*i-1], samples[2*i+1])
			if mid := samples[2*i]; mid < lo || mid > hi {
				t.Fatalf("between %d and %d: %d", lo, hi, mid)
			}
		}
	}
}

func TestTalkPCMLeavesOtherCodecsAlone(t *testing.T) {
	var c talkPCM
	p := g711Packet(111, 1, 0, []byte{1, 2, 3})
	if out := c.convert(p); len(out) != 1 || out[0] != p {
		t.Errorf("payload type 111 was converted")
	}
}
