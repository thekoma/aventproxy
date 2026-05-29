package addon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_ValidTwoCameras(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.json")
	body := `{
	  "signing_key": "sk",
	  "sid": "S",
	  "ecode": "E",
	  "partner": "P",
	  "app_key": "AK",
	  "device_id": "D",
	  "package_name": "pkg",
	  "bridge_port": 38554,
	  "cameras": [
	    {"camera_id": "abc123", "camera_name": "Erik"},
	    {"camera_id": "def456", "camera_name": "Anna"}
	  ]
	}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SigningKey != "sk" || cfg.SID != "S" || cfg.AppKey != "AK" {
		t.Errorf("creds parsed wrong: %+v", cfg)
	}
	if cfg.BridgePort != 38554 {
		t.Errorf("port = %d, want 38554", cfg.BridgePort)
	}
	if len(cfg.Cameras) != 2 {
		t.Fatalf("cameras = %d, want 2", len(cfg.Cameras))
	}
	if cfg.Cameras[0].ID != "abc123" || cfg.Cameras[0].Name != "Erik" {
		t.Errorf("camera[0] = %+v", cfg.Cameras[0])
	}
	if cfg.Cameras[1].ID != "def456" || cfg.Cameras[1].Name != "Anna" {
		t.Errorf("camera[1] = %+v", cfg.Cameras[1])
	}
}

func TestLoadConfig_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(p); err == nil {
		t.Fatal("want error for malformed JSON, got nil")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	if _, err := loadConfig("/nonexistent/path.json"); err == nil {
		t.Fatal("want error for missing file, got nil")
	}
}

func TestValidateConfig_RejectsMissingCreds(t *testing.T) {
	cfg := BridgeConfig{Cameras: []Camera{{ID: "abc", Name: "Erik"}}}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("want error for missing signing_key")
	}
}

func TestValidateConfig_RejectsEmptyCameras(t *testing.T) {
	cfg := BridgeConfig{
		SigningKey: "sk", SID: "S", AppKey: "AK", DeviceID: "D",
		Ecode: "E", Partner: "P",
		Cameras: []Camera{},
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("want error for empty cameras")
	}
}

func TestValidateConfig_AcceptsMinimal(t *testing.T) {
	cfg := BridgeConfig{
		SigningKey: "sk", SID: "S", AppKey: "AK", DeviceID: "D",
		Ecode: "E", Partner: "P",
		Cameras: []Camera{{ID: "abc", Name: "Erik"}},
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig: unexpected error %v", err)
	}
}

func TestValidateConfig_RejectsMissingEcode(t *testing.T) {
	cfg := BridgeConfig{
		SigningKey: "sk", SID: "S", AppKey: "AK", DeviceID: "D",
		Partner: "P",
		Cameras: []Camera{{ID: "abc", Name: "Erik"}},
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("want error for missing ecode")
	}
}

func TestValidateConfig_RejectsMissingPartner(t *testing.T) {
	cfg := BridgeConfig{
		SigningKey: "sk", SID: "S", AppKey: "AK", DeviceID: "D",
		Ecode: "E",
		Cameras: []Camera{{ID: "abc", Name: "Erik"}},
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("want error for missing partner")
	}
}

func TestAssignPaths_NoCollision(t *testing.T) {
	cams := []Camera{
		{ID: "abc123", Name: "Erik"},
		{ID: "def456", Name: "Anna"},
	}
	out := assignPaths(cams)
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	if out[0].Path != "/Erik" {
		t.Errorf("out[0].Path = %q, want /Erik", out[0].Path)
	}
	if out[1].Path != "/Anna" {
		t.Errorf("out[1].Path = %q, want /Anna", out[1].Path)
	}
}

func TestAssignPaths_CollidingNames(t *testing.T) {
	cams := []Camera{
		{ID: "abc123", Name: "Baby"},
		{ID: "def456", Name: "Baby"},
	}
	out := assignPaths(cams)
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	if out[0].Path != "/Baby" {
		t.Errorf("first kept as-is: got %q", out[0].Path)
	}
	if out[1].Path != "/Baby_def456" {
		t.Errorf("second got suffix: got %q, want /Baby_def456", out[1].Path)
	}
}

func TestAssignPaths_ShortIDCollision(t *testing.T) {
	cams := []Camera{
		{ID: "ab", Name: "X"},
		{ID: "cd", Name: "X"},
	}
	out := assignPaths(cams)
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	if out[1].Path != "/X_cd" {
		t.Errorf("got %q, want /X_cd (short id used in full)", out[1].Path)
	}
}

func TestAssignPaths_CascadingFallback(t *testing.T) {
	// Three cameras sharing the same name AND the same first 6 chars of camera_id.
	// The naive single-step fallback would produce three identical "/Baby_abcdef"
	// paths; the loop must extend the suffix with a counter to keep them unique.
	cams := []Camera{
		{ID: "abcdef01", Name: "Baby"},
		{ID: "abcdef02", Name: "Baby"},
		{ID: "abcdef03", Name: "Baby"},
	}
	out := assignPaths(cams)
	if len(out) != 3 {
		t.Fatalf("got %d, want 3", len(out))
	}
	paths := map[string]bool{}
	for _, c := range out {
		if paths[c.Path] {
			t.Fatalf("duplicate fallback path %q in %+v", c.Path, out)
		}
		paths[c.Path] = true
	}
	if out[0].Path != "/Baby" {
		t.Errorf("first kept as base: got %q", out[0].Path)
	}
	if out[1].Path != "/Baby_abcdef" {
		t.Errorf("second got single-suffix: got %q, want /Baby_abcdef", out[1].Path)
	}
	if out[2].Path != "/Baby_abcdef_2" {
		t.Errorf("third got counter suffix: got %q, want /Baby_abcdef_2", out[2].Path)
	}
}

func TestAssignPaths_DuplicateCameraID(t *testing.T) {
	// Different names per duplicate to make the "first entry wins" contract explicit.
	cams := []Camera{
		{ID: "abc123", Name: "Erik"},
		{ID: "abc123", Name: "Renamed Erik"},
		{ID: "def456", Name: "Anna"},
	}
	out := assignPaths(cams)
	if len(out) != 2 {
		t.Fatalf("got %d, want 2 (the second 'abc123' should be skipped)", len(out))
	}
	if out[0].ID != "abc123" || out[0].Name != "Erik" {
		t.Errorf("first occurrence should win, got %+v", out[0])
	}
	if out[1].ID != "def456" {
		t.Errorf("third entry should follow: %+v", out[1])
	}
}

func TestAssignPaths_SkipsMissingID(t *testing.T) {
	cams := []Camera{
		{ID: "", Name: "Ghost"},
		{ID: "abc123", Name: "Erik"},
	}
	out := assignPaths(cams)
	if len(out) != 1 {
		t.Fatalf("got %d, want 1 (entry with empty ID should be skipped)", len(out))
	}
	if out[0].ID != "abc123" {
		t.Errorf("kept the wrong entry: %+v", out)
	}
}
