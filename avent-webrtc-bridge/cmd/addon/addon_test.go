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
