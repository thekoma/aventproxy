package rtsp

import "github.com/pion/rtp/codecs"

// Ported from Leonard Pitzu's fork (pkg/rtsp/h264_level.go and the
// endsAccessUnit/split parts of lan_bridge.go).

// The monitor declares level 5.1 on a 1280x720 20 fps stream, which is not
// remotely what it sends: that picture fits exactly inside level 3.1. Safari
// sets its decoder up from the declared level and refuses a stream claiming
// more; Chrome ignores the claim. Correcting the declaration changes no coded bit.

// levelLimits is H.264 Annex A table A-1: the frame size and macroblock rate
// each level admits.
var levelLimits = []struct {
	level   byte
	maxFS   int // macroblocks per frame
	maxMBPS int // macroblocks per second
}{
	{10, 99, 1485},
	{11, 396, 3000},
	{12, 396, 6000},
	{13, 396, 11880},
	{20, 396, 11880},
	{21, 792, 19800},
	{22, 1620, 20250},
	{30, 1620, 40500},
	{31, 3600, 108000},
	{32, 5120, 216000},
	{40, 8192, 245760},
	{41, 8192, 245760},
	{42, 8704, 522240},
	{50, 22080, 589824},
	{51, 36864, 983040},
	{52, 36864, 2073600},
}

// minLevel is the lowest level that still admits this picture size and rate.
// Zero means the stream is outside the table and nothing should be rewritten.
func minLevel(width, height, fps int) byte {
	if width <= 0 || height <= 0 {
		return 0
	}
	if fps <= 0 {
		fps = 30
	}

	frame := ((width + 15) / 16) * ((height + 15) / 16)
	rate := frame * fps
	for _, limit := range levelLimits {
		if frame <= limit.maxFS && rate <= limit.maxMBPS {
			return limit.level
		}
	}
	return 0
}

// level_idc sits after the NAL header, profile_idc and the constraint flags.
const spsLevelOffset = 3

// correctSPSLevel lowers an SPS's declared level to what the stream needs. It
// never raises one: a level below the real requirement is the single change
// here that could break a decoder trusting it. It copies before writing, so
// the caller's buffer is never touched.
func correctSPSLevel(nal []byte, level byte) []byte {
	if level == 0 || len(nal) <= spsLevelOffset {
		return nal
	}
	if nal[0]&0x1f != 7 || nal[spsLevelOffset] <= level {
		return nal
	}

	corrected := make([]byte, len(nal))
	copy(corrected, nal)
	corrected[spsLevelOffset] = level
	return corrected
}

// lanRTPMTU is the largest RTP payload the LAN source emits.
const lanRTPMTU = 1200

// endsAccessUnit reports whether a payload completes a picture, which is what
// the RTP marker bit means for H.264.
func endsAccessUnit(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	switch payload[0] & 0x1f {
	case 28, 29: // FU-A / FU-B: the end bit lives in the fragmentation header
		return len(payload) > 1 && payload[1]&0x40 != 0
	case 1, 5: // a whole slice, coded or IDR
		return true
	default: // parameter sets and SEI precede the picture they describe
		return false
	}
}

// splitNAL fragments a payload only when it is a whole NAL that will not fit.
// Anything already fragmented or aggregated is passed through untouched.
func splitNAL(payload []byte, p *codecs.H264Payloader) [][]byte {
	if len(payload) <= lanRTPMTU {
		return [][]byte{payload}
	}
	if payload[0]&0x1f >= 28 {
		return [][]byte{payload}
	}
	if out := p.Payload(lanRTPMTU, payload); len(out) > 0 {
		return out
	}
	return [][]byte{payload}
}

// carriesSPSOrIDR reports whether a payload holds an SPS or the start of an
// IDR, what an H.264 decoder needs before it shows anything. An HEVC header
// byte has even low bits and an Annex-B start code reads as type 0, so
// neither can pass for one.
func carriesSPSOrIDR(payload []byte) bool {
	if len(payload) < 2 {
		return false
	}
	switch payload[0] & 0x1f {
	case 5, 7:
		return true
	case 28: // FU-A: only the start fragment names the NAL inside
		return payload[1]&0x80 != 0 && payload[1]&0x1f == 5
	case 24: // STAP-A: NALs, each after a 2-byte length
		for rest := payload[1:]; len(rest) > 2; {
			n := int(rest[0])<<8 | int(rest[1])
			rest = rest[2:]
			if n == 0 || n > len(rest) {
				return false
			}
			if t := rest[0] & 0x1f; t == 5 || t == 7 {
				return true
			}
			rest = rest[n:]
		}
	}
	return false
}
