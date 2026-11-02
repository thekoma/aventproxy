package rtsp

import (
	"testing"

	"github.com/pion/rtp/codecs"
)

func TestEndsAccessUnit(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"FU-A end bit", []byte{0x7c, 0x45, 0x00}, true},
		{"FU-A no end bit", []byte{0x7c, 0x05, 0x00}, false},
		{"non-IDR slice", []byte{0x41, 0x00}, true},
		{"IDR slice", []byte{0x65, 0x00}, true},
		{"SPS", []byte{0x67, 0x42}, false},
		{"empty", nil, false},
		{"FU-A without header byte", []byte{0x7c}, false},
	}
	for _, c := range cases {
		if got := endsAccessUnit(c.payload); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSplitNALKeepsFragmentsWhole(t *testing.T) {
	p := &codecs.H264Payloader{}

	fua := make([]byte, 2000)
	fua[0] = 0x7c
	if out := splitNAL(fua, p); len(out) != 1 || len(out[0]) != 2000 {
		t.Fatalf("FU-A must come back as one piece, got %d pieces", len(out))
	}

	idr := make([]byte, 2000)
	idr[0] = 0x65
	out := splitNAL(idr, p)
	if len(out) < 2 {
		t.Fatalf("a 2000-byte IDR must be fragmented, got %d pieces", len(out))
	}
	for i, piece := range out {
		if len(piece) > lanRTPMTU {
			t.Errorf("piece %d is %d bytes, over %d", i, len(piece), lanRTPMTU)
		}
		if piece[0]&0x1f != 28 {
			t.Errorf("piece %d is not FU-A", i)
		}
	}

	if out := splitNAL(nil, p); len(out) != 1 {
		t.Errorf("empty payload must pass through, got %d pieces", len(out))
	}
}

func TestCorrectSPSLevelLowersOverstatedLevel(t *testing.T) {
	// The monitor declares 5.1 on a 720p20 picture that fits level 3.1.
	level := minLevel(1280, 720, 20)
	if level != 31 {
		t.Fatalf("minLevel(1280,720,20) = %d, want 31", level)
	}
	sps := []byte{0x67, 0x64, 0x00, 51, 0xac}
	got := correctSPSLevel(sps, level)
	if got[3] != level {
		t.Errorf("level_idc = %d, want %d", got[3], level)
	}
	if sps[3] != 51 {
		t.Error("the caller's slice must not be modified")
	}
}

func TestCorrectSPSLevelNeverRaisesOrTouchesOthers(t *testing.T) {
	// Raising a declaration is the one change that could break a decoder.
	low := []byte{0x67, 0x64, 0x00, 30, 0xac}
	if got := correctSPSLevel(low, minLevel(1920, 1080, 15)); got[3] != 30 {
		t.Errorf("a low declaration must stay, got %d", got[3])
	}
	pps := []byte{0x68, 0x64, 0x00, 51}
	if got := correctSPSLevel(pps, 31); got[3] != 51 {
		t.Error("only an SPS may be rewritten")
	}
	short := []byte{0x67, 0x64}
	if got := correctSPSLevel(short, 31); len(got) != 2 {
		t.Error("a too-short SPS must pass through")
	}
}
