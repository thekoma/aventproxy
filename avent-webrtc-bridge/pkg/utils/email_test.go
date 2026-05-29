package utils

import "testing"

func TestMaskEmail(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"andrea.cervesato@dontouch.ch", "a***@dontouch.ch"},
		{"a@b.co", "a***@b.co"},
		{"", "***"},
		{"noatsign", "***"},
		{"@leading.com", "***"},
		{"trailing@", "***"},
	}
	for _, c := range cases {
		got := MaskEmail(c.in)
		if got != c.want {
			t.Errorf("MaskEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
