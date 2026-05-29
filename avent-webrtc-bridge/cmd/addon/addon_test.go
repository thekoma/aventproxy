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
