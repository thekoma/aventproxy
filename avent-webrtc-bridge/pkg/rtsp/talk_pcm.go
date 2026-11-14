package rtsp

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/pion/rtp"
)

// The Avent monitors play the audio a two-way session sends them as signed
// 16-bit little-endian PCM at 16 kHz, their own audio format, whatever the
// session's SDP agreed (PCMU/8000). Measured on an SCD923/26 (firmware
// 1.1.12, protocol 3.3) and an SCD951/26 (2.4.1, 3.4), 2026-11-07, with test
// audio on the RTSP backchannel: G.711 came out as bursts of noise with
// silence between them, a voice as loud hissing, while the same tone as
// 16 kHz PCM in packets of 160 bytes played as one steady tone. Packets of
// 320 bytes were not played at all. So the talk path turns the clients'
// G.711 into that before it reaches the camera.
const (
	// talkPCMPacketBytes is one packet's payload: 80 samples, 5 ms at 16 kHz.
	talkPCMPacketBytes = 160
	// talkPCMPacketTicks is that packet's length on the session's 8 kHz RTP clock.
	talkPCMPacketTicks = 40
	// talkPCMClockRate is the session's RTP clock, in ticks per second.
	talkPCMClockRate = 8000
	// talkPCMIdleReset is the pause after which the next packet starts a new
	// talk. Clients send a packet every 20 or 30 ms while they talk.
	talkPCMIdleReset = 500 * time.Millisecond
)

// talkPCM converts backchannel packets (G.711 at 8 kHz) to the cameras' PCM.
// It keeps the last sample, so the upsampling runs on across packets, and
// its own sequence numbers and timestamps for the packets it makes; bytes
// short of a whole packet wait for the next one. Safe for concurrent use.
//
// With talkback mode session one bridge serves every talk of a camera
// session, so a talk that starts after a pause, or from another client,
// starts afresh: the bytes still waiting and the last sample belong to the
// one before. The sequence numbers run on, so the camera never sees them
// step back, and the timestamps move on by the pause.
type talkPCM struct {
	mu      sync.Mutex
	now     func() time.Time // nil means time.Now; set by tests
	started bool
	ssrc    uint32
	last    time.Time
	prev    int16
	seq     uint16
	ts      uint32
	pending []byte
}

// convert returns the packets to send for one backchannel packet: PCM for
// G.711 (payload types 0 and 8), and the packet itself for anything else.
func (c *talkPCM) convert(p *rtp.Packet) []*rtp.Packet {
	var decode func(byte) int16
	switch p.PayloadType {
	case 0:
		decode = decodeMuLaw
	case 8:
		decode = decodeALaw
	default:
		return []*rtp.Packet{p}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	at := now()
	switch {
	case !c.started:
		c.started, c.seq, c.ts = true, p.SequenceNumber, p.Timestamp
		c.prev = firstSample(p.Payload, decode)
	case p.SSRC != c.ssrc || at.Sub(c.last) > talkPCMIdleReset:
		c.pending = c.pending[:0]
		c.ts += uint32(at.Sub(c.last) * talkPCMClockRate / time.Second)
		c.prev = firstSample(p.Payload, decode)
	}
	c.ssrc, c.last = p.SSRC, at
	// 8 to 16 kHz: a sample half way between each pair, then the sample.
	buf := c.pending
	for _, b := range p.Payload {
		s := decode(b)
		buf = binary.LittleEndian.AppendUint16(buf, uint16(int16((int32(c.prev)+int32(s))/2)))
		buf = binary.LittleEndian.AppendUint16(buf, uint16(s))
		c.prev = s
	}
	var out []*rtp.Packet
	for len(buf) >= talkPCMPacketBytes {
		out = append(out, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    p.PayloadType,
				SequenceNumber: c.seq,
				Timestamp:      c.ts,
				SSRC:           p.SSRC,
			},
			Payload: append([]byte(nil), buf[:talkPCMPacketBytes]...),
		})
		c.seq++
		c.ts += talkPCMPacketTicks
		buf = buf[talkPCMPacketBytes:]
	}
	c.pending = append(c.pending[:0], buf...)
	return out
}

// firstSample is the first sample of a payload, or silence for an empty one.
// It seeds the upsampling, so a talk does not start half way from the last.
func firstSample(payload []byte, decode func(byte) int16) int16 {
	if len(payload) == 0 {
		return 0
	}
	return decode(payload[0])
}

// decodeMuLaw is G.711 μ-law to a linear 16-bit sample.
func decodeMuLaw(u byte) int16 {
	u = ^u
	t := (int32(u&0x0F) << 3) + 0x84
	t <<= (u & 0x70) >> 4
	if u&0x80 != 0 {
		return int16(0x84 - t)
	}
	return int16(t - 0x84)
}

// decodeALaw is G.711 A-law to a linear 16-bit sample.
func decodeALaw(a byte) int16 {
	a ^= 0x55
	t := int32(a&0x0F) << 4
	switch seg := (a & 0x70) >> 4; seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= seg - 1
	}
	if a&0x80 != 0 {
		return int16(t)
	}
	return int16(-t)
}
