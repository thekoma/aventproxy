package storage

import "testing"

func TestSanitizeRTSPPath(t *testing.T) {
	cases := []struct {
		name string
		in   struct{ name, id string }
		want string
	}{
		{"simple name", struct{ name, id string }{"Erik", "abc123"}, "/Erik"},
		{"name with spaces", struct{ name, id string }{"Baby Room", "abc123"}, "/Baby_Room"},
		{"empty name falls back to id", struct{ name, id string }{"", "abc123"}, "/abc123"},
		{"multiple spaces", struct{ name, id string }{"Erik  Two", "abc123"}, "/Erik__Two"},
		{"slash in name", struct{ name, id string }{"Room/Camera", "abc123"}, "/Room_Camera"},
		{"backslash in name", struct{ name, id string }{"Room\\Camera", "abc123"}, "/Room_Camera"},
		{"name sanitizes to underscore", struct{ name, id string }{"/", "abc123"}, "/abc123"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SanitizeRTSPPath(c.in.name, c.in.id)
			if got != c.want {
				t.Errorf("SanitizeRTSPPath(%q, %q) = %q, want %q", c.in.name, c.in.id, got, c.want)
			}
		})
	}
}

func TestGenerateRTSPPath_delegatesToSanitize(t *testing.T) {
	sm := &StorageManager{}
	got := sm.GenerateRTSPPath("Baby Room", "dev456")
	want := SanitizeRTSPPath("Baby Room", "dev456")
	if got != want {
		t.Errorf("GenerateRTSPPath(%q, %q) = %q, want %q", "Baby Room", "dev456", got, want)
	}
}
