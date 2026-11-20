package rtsp

import (
	"math"
	"strings"
	"testing"

	"avent-webrtc-bridge/pkg/storage"
)

func TestEncodeG711KnownValues(t *testing.T) {
	got := encodeMuLaw([]int16{0, 32767, -32768})
	want := []byte{0xFF, 0x80, 0x00}
	if string(got) != string(want) {
		t.Errorf("got %x, want %x", got, want)
	}
}

func TestPCM16LE(t *testing.T) {
	got := pcm16LE([]byte{0x01, 0x00, 0xff, 0xff, 0x00, 0x80, 0x07})
	want := []int16{1, -1, -32768}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d: got %d, want %d", i, got[i], want[i])
		}
	}
}

func sine(freq float64, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(10000 * math.Sin(2*math.Pi*freq*float64(i)/16000))
	}
	return out
}

func rms(s []int16) float64 {
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func TestDownsamplerHalvesAndPassesLowTone(t *testing.T) {
	in := sine(1000, 320)
	out := (&halfRateDownsampler{}).process(in)
	if len(out) != 160 {
		t.Fatalf("got %d samples, want 160", len(out))
	}
	if r := rms(out) / rms(in); r < 0.9 || r > 1.1 {
		t.Errorf("1 kHz RMS ratio %.3f, want within 10%%", r)
	}

	hi := sine(7000, 3200)
	outHi := (&halfRateDownsampler{}).process(hi)
	// skip the filter warm-up
	db := 20 * math.Log10(rms(outHi[20:])/rms(hi))
	t.Logf("7 kHz attenuation %.1f dB", db)
	if db > -20 {
		t.Errorf("7 kHz only %.1f dB down, want at most -20", db)
	}
}

func TestDownsamplerKeepsStateAcrossCalls(t *testing.T) {
	in := sine(1000, 640)
	whole := (&halfRateDownsampler{}).process(in)

	d := &halfRateDownsampler{}
	var split []int16
	for _, n := range []int{101, 200, 7, 332} { // odd sizes too
		split = append(split, d.process(in[:n])...)
		in = in[n:]
	}
	if len(split) != len(whole) {
		t.Fatalf("got %d samples, want %d", len(split), len(whole))
	}
	for i := range whole {
		if split[i] != whole[i] {
			t.Fatalf("sample %d differs: %d vs %d", i, split[i], whole[i])
		}
	}
}

func TestAddonSDPAnnouncesPCMU(t *testing.T) {
	// encodeMuLaw's shortcut relies on this: add-on cameras carry no Skill.
	sdp := NewRTSPServer(0, nil).generateSDP(&storage.CameraInfo{DeviceID: "d", DeviceName: "n"}, "rtsp://h/n")
	for _, want := range []string{"PCMU/8000", "H264/90000"} {
		if !strings.Contains(sdp, want) {
			t.Errorf("SDP lacks %s:\n%s", want, sdp)
		}
	}
}
