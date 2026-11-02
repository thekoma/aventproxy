package rtsp

import (
	"encoding/binary"
	"math"
)

// pcm16LE reads little-endian signed 16-bit samples; a trailing odd byte is dropped.
func pcm16LE(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out
}

// firTaps is a 15-tap Hamming-windowed sinc low-pass at 3.5 kHz for a 16 kHz
// input, normalised to unity gain at DC. It leaves the 8 kHz output's Nyquist
// (4 kHz) mostly clean; a 7 kHz tone comes out about 55 dB down.
var firTaps = func() [15]float64 {
	const cutoff = 3500.0 / 16000.0
	var taps [15]float64
	var sum float64
	for i := range taps {
		x := float64(i - 7)
		sinc := 2 * cutoff
		if x != 0 {
			sinc = math.Sin(2*math.Pi*cutoff*x) / (math.Pi * x)
		}
		taps[i] = sinc * (0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/14))
		sum += taps[i]
	}
	for i := range taps {
		taps[i] /= sum
	}
	return taps
}()

// halfRateDownsampler turns 16 kHz PCM into 8 kHz. It keeps the last samples
// and the decimation phase between calls, so frame boundaries do not click.
type halfRateDownsampler struct {
	hist [len(firTaps) - 1]int16
	skip int // input samples to discard before the next output
}

func (d *halfRateDownsampler) process(in []int16) []int16 {
	n := len(d.hist)
	buf := make([]int16, 0, n+len(in))
	buf = append(buf, d.hist[:]...)
	buf = append(buf, in...)

	out := make([]int16, 0, len(in)/2+1)
	i := d.skip
	for ; i < len(in); i += 2 {
		var acc float64
		for k, c := range firTaps {
			acc += c * float64(buf[i+k])
		}
		out = append(out, int16(math.Max(math.MinInt16, math.Min(math.MaxInt16, math.Round(acc)))))
	}
	d.skip = i - len(in)
	copy(d.hist[:], buf[len(buf)-n:])
	return out
}

// encodeMuLaw encodes samples as G.711 μ-law.
// shortcut: μ-law only, because add-on cameras never carry a Skill so generateSDP always announces PCMU/8000; add A-law if a LAN stream ever runs with a parsed skill.
func encodeMuLaw(samples []int16) []byte {
	const bias, clip = 0x84, 32635
	out := make([]byte, len(samples))
	for i, s := range samples {
		mag := int(s)
		var sign byte
		if mag < 0 {
			mag, sign = -mag, 0x80
		}
		mag = min(mag, clip) + bias
		exp := 7
		for mask := 0x4000; mag&mask == 0 && exp > 0; mask >>= 1 {
			exp--
		}
		out[i] = ^(sign | byte(exp<<4) | byte(mag>>(exp+3))&0x0f)
	}
	return out
}
