# Multi-Camera Bridge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a new `addon` subcommand to `avent-webrtc-bridge` that reads the HA-written JSON config and serves every camera on the account from a single process, each on its own RTSP path.

**Architecture:** A pure helper extracts the existing path-sanitization rule so it can be shared by `direct.go` (unchanged behavior) and the new `addon.go`. The `addon` subcommand parses the JSON, validates it, deduplicates colliding paths, registers all cameras through the existing `storage.UpdateCamerasForUser` API, and starts the existing multi-camera `RTSPServer`. `run.sh` is reduced to one `exec` line.

**Tech Stack:** Go 1.26 (bridge), Python 3.11+ (integration), Cobra (CLI), `golang:1.26-bookworm` container for local CI mirror.

**Spec reference:** `docs/superpowers/specs/2026-05-29-multi-camera-bridge-design.md`

**Non-negotiable constraint:** The user only interacts with the HA config flow (email + password + MFA). Any change that would require manual user steps is wrong.

---

## File Structure

**New files:**
- `avent-webrtc-bridge/pkg/storage/path.go` — `SanitizeRTSPPath(name, id string) string` helper
- `avent-webrtc-bridge/pkg/storage/path_test.go` — unit tests for the helper
- `avent-webrtc-bridge/cmd/addon/addon.go` — new Cobra subcommand
- `avent-webrtc-bridge/cmd/addon/addon_test.go` — unit tests for JSON parsing, validation, path collisions

**Modified files:**
- `avent-webrtc-bridge/cmd/direct/direct.go` — switch to the shared helper
- `avent-webrtc-bridge/cmd/root.go` — register `NewAddonCmd()`
- `custom_components/philips_avent/const.py` — add `sanitize_rtsp_path()` helper
- `custom_components/philips_avent/camera.py` — use the helper
- `aventproxy-bridge-addon/run.sh` — simplify to one `exec` line
- `README.md` — update "How It Works" to reflect multi-camera

---

## Task 1: Extract `SanitizeRTSPPath` helper

**Files:**
- Create: `avent-webrtc-bridge/pkg/storage/path.go`
- Create: `avent-webrtc-bridge/pkg/storage/path_test.go`

- [ ] **Step 1: Write the failing test**

Create `avent-webrtc-bridge/pkg/storage/path_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go test ./pkg/storage/ -run TestSanitizeRTSPPath -v
```

Expected: `FAIL` with `undefined: SanitizeRTSPPath`.

- [ ] **Step 3: Write minimal implementation**

Create `avent-webrtc-bridge/pkg/storage/path.go`:

```go
package storage

import "strings"

// SanitizeRTSPPath converts a camera display name into an RTSP path component.
// Spaces become underscores. If name is empty, the camera id is used as a fallback.
// The returned string starts with "/".
func SanitizeRTSPPath(name, id string) string {
	if name == "" {
		name = id
	}
	return "/" + strings.ReplaceAll(name, " ", "_")
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go test ./pkg/storage/ -run TestSanitizeRTSPPath -v
```

Expected: `PASS` (4 subtests).

- [ ] **Step 5: Commit**

```bash
git add avent-webrtc-bridge/pkg/storage/path.go avent-webrtc-bridge/pkg/storage/path_test.go
git commit -m "feat(bridge): extract SanitizeRTSPPath helper into pkg/storage"
```

---

## Task 2: Switch `direct.go` to the shared helper

**Files:**
- Modify: `avent-webrtc-bridge/cmd/direct/direct.go:80-82`

- [ ] **Step 1: Replace the inline sanitization**

Edit `avent-webrtc-bridge/cmd/direct/direct.go`. Find:

```go
	if cameraName == "" {
		cameraName = cameraID
	}
	rtspPath := "/" + strings.ReplaceAll(cameraName, " ", "_")
```

Replace with:

```go
	rtspPath := storage.SanitizeRTSPPath(cameraName, cameraID)
```

(The `strings` import may still be needed elsewhere in the file — check before removing it. The `storage` import is already present.)

- [ ] **Step 2: Run build to verify**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go build ./...
```

Expected: no output, exit 0.

- [ ] **Step 3: Run existing tests to verify no regression**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go test ./...
```

Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add avent-webrtc-bridge/cmd/direct/direct.go
git commit -m "refactor(bridge): direct uses shared SanitizeRTSPPath"
```

---

## Task 3: Define `BridgeConfig` struct and `loadConfig()` parser

**Files:**
- Create: `avent-webrtc-bridge/cmd/addon/addon.go`
- Create: `avent-webrtc-bridge/cmd/addon/addon_test.go`

- [ ] **Step 1: Write the failing test**

Create `avent-webrtc-bridge/cmd/addon/addon_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go test ./cmd/addon/ -run TestLoadConfig_ValidTwoCameras -v
```

Expected: build error (`loadConfig` not defined).

- [ ] **Step 3: Write the minimal implementation**

Create `avent-webrtc-bridge/cmd/addon/addon.go`:

```go
package addon

import (
	"encoding/json"
	"fmt"
	"os"
)

// BridgeConfig is the JSON shape written by the HA integration
// in custom_components/philips_avent/__init__.py::_write_bridge_config.
type BridgeConfig struct {
	SigningKey  string   `json:"signing_key"`
	SID         string   `json:"sid"`
	Ecode       string   `json:"ecode"`
	Partner     string   `json:"partner"`
	AppKey      string   `json:"app_key"`
	DeviceID    string   `json:"device_id"`
	PackageName string   `json:"package_name"`
	BridgePort  int      `json:"bridge_port"`
	Cameras     []Camera `json:"cameras"`
}

// Camera is one entry under "cameras" in the JSON.
type Camera struct {
	ID   string `json:"camera_id"`
	Name string `json:"camera_name"`
}

func loadConfig(path string) (BridgeConfig, error) {
	var cfg BridgeConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go test ./cmd/addon/ -run TestLoadConfig_ValidTwoCameras -v
```

Expected: `PASS`.

- [ ] **Step 5: Commit**

```bash
git add avent-webrtc-bridge/cmd/addon/
git commit -m "feat(bridge): add addon subcommand scaffold with JSON loader"
```

---

## Task 4: Add validation + error cases

**Files:**
- Modify: `avent-webrtc-bridge/cmd/addon/addon.go` (add `validateConfig`)
- Modify: `avent-webrtc-bridge/cmd/addon/addon_test.go` (add tests)

- [ ] **Step 1: Write failing tests**

Append to `avent-webrtc-bridge/cmd/addon/addon_test.go`:

```go
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
		Cameras: []Camera{},
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("want error for empty cameras")
	}
}

func TestValidateConfig_AcceptsMinimal(t *testing.T) {
	cfg := BridgeConfig{
		SigningKey: "sk", SID: "S", AppKey: "AK", DeviceID: "D",
		Cameras: []Camera{{ID: "abc", Name: "Erik"}},
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig: unexpected error %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go test ./cmd/addon/ -v
```

Expected: build error (`validateConfig` not defined). `TestLoadConfig_MalformedJSON` and `TestLoadConfig_MissingFile` should compile but the latter is expected to already pass once compile succeeds. Focus on the validate tests failing.

- [ ] **Step 3: Implement `validateConfig`**

Append to `avent-webrtc-bridge/cmd/addon/addon.go`:

```go
func validateConfig(cfg BridgeConfig) error {
	if cfg.SigningKey == "" {
		return fmt.Errorf("signing_key is required")
	}
	if cfg.SID == "" {
		return fmt.Errorf("sid is required")
	}
	if cfg.AppKey == "" {
		return fmt.Errorf("app_key is required")
	}
	if cfg.DeviceID == "" {
		return fmt.Errorf("device_id is required")
	}
	if len(cfg.Cameras) == 0 {
		return fmt.Errorf("cameras list is empty — nothing to serve")
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go test ./cmd/addon/ -v
```

Expected: all subtests `PASS`.

- [ ] **Step 5: Commit**

```bash
git add avent-webrtc-bridge/cmd/addon/
git commit -m "feat(bridge): validate addon JSON config"
```

---

## Task 5: Path assignment with collision handling

**Files:**
- Modify: `avent-webrtc-bridge/cmd/addon/addon.go` (add `assignPaths`)
- Modify: `avent-webrtc-bridge/cmd/addon/addon_test.go` (add tests)

- [ ] **Step 1: Write failing tests**

Append to `avent-webrtc-bridge/cmd/addon/addon_test.go`:

```go
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
		{ID: "ab", Name: "X"},
	}
	out := assignPaths(cams)
	// Second collision uses full id when shorter than 6 chars
	if out[1].Path != "/X_ab" {
		t.Errorf("got %q, want /X_ab", out[1].Path)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go test ./cmd/addon/ -run TestAssignPaths -v
```

Expected: build error (`assignPaths` not defined).

- [ ] **Step 3: Implement `assignPaths`**

Append to `avent-webrtc-bridge/cmd/addon/addon.go`. Add the import for `"avent-webrtc-bridge/pkg/storage"` and `"avent-webrtc-bridge/pkg/core"` at the top of the file.

```go
// CameraWithPath pairs a Camera with the RTSP path it will be served on.
type CameraWithPath struct {
	Camera
	Path string
}

// assignPaths sanitizes each camera's name into an RTSP path and resolves
// collisions by suffixing the second occurrence with up to 6 chars of the camera id.
func assignPaths(cams []Camera) []CameraWithPath {
	out := make([]CameraWithPath, 0, len(cams))
	seen := make(map[string]bool, len(cams))
	for _, cam := range cams {
		path := storage.SanitizeRTSPPath(cam.Name, cam.ID)
		if seen[path] {
			suffix := cam.ID
			if len(suffix) > 6 {
				suffix = suffix[:6]
			}
			collided := path + "_" + suffix
			core.Logger.Warn().Msgf("Path collision on %s, falling back to %s", path, collided)
			path = collided
		}
		seen[path] = true
		out = append(out, CameraWithPath{Camera: cam, Path: path})
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go test ./cmd/addon/ -v
```

Expected: all subtests `PASS`.

- [ ] **Step 5: Commit**

```bash
git add avent-webrtc-bridge/cmd/addon/
git commit -m "feat(bridge): assign collision-free RTSP paths to cameras"
```

---

## Task 6: Implement `NewAddonCmd()` and wire startup

**Files:**
- Modify: `avent-webrtc-bridge/cmd/addon/addon.go` (add `NewAddonCmd`, `runAddon`)

- [ ] **Step 1: Add Cobra command + RunE wiring**

Append to `avent-webrtc-bridge/cmd/addon/addon.go`. The imports section at the top must include:

```go
import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/rtsp"
	"avent-webrtc-bridge/pkg/storage"
	"avent-webrtc-bridge/pkg/tuya"
)
```

Add a package-level storage manager and setter (matches the pattern in `cmd/direct/direct.go`):

```go
var storageManager *storage.StorageManager

func SetStorageManager(sm *storage.StorageManager) {
	storageManager = sm
}

func NewAddonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "addon",
		Short: "Run the multi-camera bridge driven by the HA integration JSON",
		Long: `Read the bridge config JSON written by the Philips Avent HA integration
and serve every camera under it from one RTSP server, each on its own path.

Example:
  avent-webrtc-bridge addon --config /config/philips_avent_bridge_<entry_id>.json`,
		RunE: runAddon,
	}
	cmd.Flags().String("config", "", "Path to the bridge config JSON written by the HA integration")
	cmd.MarkFlagRequired("config")
	return cmd
}

func runAddon(cmd *cobra.Command, args []string) error {
	cfgPath, _ := cmd.Flags().GetString("config")

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if err := validateConfig(cfg); err != nil {
		return fmt.Errorf("invalid config %s: %w", cfgPath, err)
	}

	port := cfg.BridgePort
	if port == 0 {
		port = 38554
	}

	client := tuya.NewMobileSDKClient(cfg.SigningKey, cfg.SID, cfg.AppKey, cfg.DeviceID, "071d81fa")
	client.Ecode = cfg.Ecode
	client.PartnerIdentity = cfg.Partner
	client.PackageName = cfg.PackageName

	core.Logger.Info().Msg("Verifying API access...")
	if _, err := client.Call("smartlife.p.time.get", "1.0", nil); err != nil {
		return fmt.Errorf("API verification failed: %w", err)
	}
	core.Logger.Info().Msg("API access OK")

	userInfo, err := client.GetUserInfo()
	if err != nil {
		return fmt.Errorf("get user info: %w", err)
	}
	core.Logger.Info().Msgf("User: %s (%s)", userInfo.Nickname, userInfo.Email)
	client.UID = userInfo.ID

	userKey := "addon_" + strings.ReplaceAll(strings.ReplaceAll(userInfo.Email, "@", "_at_"), ".", "_")
	user := &storage.UserSession{
		Email:  userInfo.Email,
		Region: "addon",
		SessionData: &tuya.SessionData{
			LoginResult: &tuya.LoginResult{
				Uid:      userInfo.ID,
				Email:    userInfo.Email,
				Nickname: userInfo.Nickname,
				Domain:   userInfo.Domain,
			},
			ServerHost: "a1.tuyaeu.com",
			Region:     "addon",
			UserEmail:  userInfo.Email,
		},
		LastRefresh: time.Now(),
		UserKey:     userKey,
	}
	if err := storageManager.SaveUser("addon", userInfo.Email, user.SessionData); err != nil {
		core.Logger.Warn().Msgf("Could not save user session: %v", err)
	}

	camsWithPath := assignPaths(cfg.Cameras)
	infos := make([]storage.CameraInfo, 0, len(camsWithPath))
	pathLog := make([]string, 0, len(camsWithPath))
	for _, c := range camsWithPath {
		if c.ID == "" {
			core.Logger.Warn().Msgf("Camera config invalid: missing camera_id, skipping name=%q", c.Name)
			continue
		}
		infos = append(infos, storage.CameraInfo{
			DeviceID:   c.ID,
			DeviceName: c.Name,
			Category:   "sp",
			RTSPPath:   c.Path,
			UserKey:    userKey,
		})
		pathLog = append(pathLog, c.Path)
		core.Logger.Info().Msgf("Camera registered: id=%s name=%s path=%s", c.ID, c.Name, c.Path)
	}
	if len(infos) == 0 {
		return fmt.Errorf("no valid cameras after filtering, refusing to start")
	}
	if err := storageManager.UpdateCamerasForUser(userKey, infos); err != nil {
		core.Logger.Warn().Msgf("Could not save cameras: %v", err)
	}

	server := rtsp.NewRTSPServer(port, storageManager)
	server.MobileClient = client
	if err := server.Start(); err != nil {
		return fmt.Errorf("start RTSP server: %w", err)
	}
	core.Logger.Info().Msgf("Serving %d cameras on port %d: %s", len(infos), port, strings.Join(pathLog, " "))

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	core.Logger.Info().Msg("Shutting down...")
	server.Stop()
	return nil
}
```

- [ ] **Step 2: Build to verify**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go build ./...
```

Expected: no output, exit 0.

- [ ] **Step 3: Run all tests**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go test ./...
```

Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add avent-webrtc-bridge/cmd/addon/
git commit -m "feat(bridge): implement addon RunE end-to-end"
```

---

## Task 7: Register the `addon` subcommand in `cmd/root.go`

**Files:**
- Modify: `avent-webrtc-bridge/cmd/root.go` (3 surgical edits)

- [ ] **Step 1: Add the import**

In `avent-webrtc-bridge/cmd/root.go`, find:

```go
	"avent-webrtc-bridge/cmd/auth"
	"avent-webrtc-bridge/cmd/cameras"
	"avent-webrtc-bridge/cmd/direct"
	"avent-webrtc-bridge/cmd/rtsp"
```

Replace with:

```go
	"avent-webrtc-bridge/cmd/addon"
	"avent-webrtc-bridge/cmd/auth"
	"avent-webrtc-bridge/cmd/cameras"
	"avent-webrtc-bridge/cmd/direct"
	"avent-webrtc-bridge/cmd/rtsp"
```

- [ ] **Step 2: Add the subcommand registration**

Find:

```go
	rootCmd.AddCommand(direct.NewDirectCmd())
}
```

Replace with:

```go
	rootCmd.AddCommand(direct.NewDirectCmd())
	rootCmd.AddCommand(addon.NewAddonCmd())
}
```

- [ ] **Step 3: Wire the storage manager**

Find:

```go
	direct.SetStorageManager(storageManager)
}
```

Replace with:

```go
	direct.SetStorageManager(storageManager)
	addon.SetStorageManager(storageManager)
}
```

- [ ] **Step 4: Build and verify the command appears**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  sh -c "go build -o /tmp/bridge . && /tmp/bridge --help"
```

Expected output includes:

```
Available Commands:
  addon       Run the multi-camera bridge driven by the HA integration JSON
  auth        ...
  cameras     ...
  direct      ...
```

Verify `addon` is listed.

- [ ] **Step 5: Verify help text**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  sh -c "go build -o /tmp/bridge . && /tmp/bridge addon --help"
```

Expected output includes the `--config` flag description and the example URL with `--config /config/philips_avent_bridge_<entry_id>.json`.

- [ ] **Step 6: Commit**

```bash
git add avent-webrtc-bridge/cmd/root.go
git commit -m "feat(bridge): register addon subcommand in root"
```

---

## Task 8: Python sanitization helper + camera.py rewire

**Files:**
- Modify: `custom_components/philips_avent/const.py`
- Modify: `custom_components/philips_avent/camera.py:40-41`
- Create: `tests/test_philips_avent/test_sanitize.py`

The Go and Python sides must agree on the RTSP path. Today both do `name.replace(" ", "_")` and there is no fallback to id on the Python side. The helper makes the rule explicit and matches the Go behavior.

- [ ] **Step 1: Write the failing test**

Create `tests/test_philips_avent/test_sanitize.py`:

```python
from custom_components.philips_avent.const import sanitize_rtsp_path


def test_simple_name():
    assert sanitize_rtsp_path("Erik", "abc123") == "Erik"


def test_name_with_spaces():
    assert sanitize_rtsp_path("Baby Room", "abc123") == "Baby_Room"


def test_empty_name_falls_back_to_id():
    assert sanitize_rtsp_path("", "abc123") == "abc123"


def test_multiple_spaces():
    assert sanitize_rtsp_path("Erik  Two", "abc123") == "Erik__Two"
```

Note: the Python helper returns the path component **without** the leading `/`, because `camera.py` builds the URL as `rtsp://host:port/{safe_name}`.

- [ ] **Step 2: Run test to verify it fails**

```bash
PYTHONPATH=. pytest tests/test_philips_avent/test_sanitize.py -v
```

Expected: `ImportError` for `sanitize_rtsp_path`.

- [ ] **Step 3: Implement the helper**

Append to `custom_components/philips_avent/const.py`:

```python
def sanitize_rtsp_path(name: str, cam_id: str) -> str:
    """Convert a camera display name into an RTSP path component.

    Spaces become underscores. Empty name falls back to the camera id.
    Returns the path component WITHOUT a leading slash; the caller composes the URL.
    Mirrors `pkg/storage/path.go::SanitizeRTSPPath` in the Go bridge.
    """
    if not name:
        name = cam_id
    return name.replace(" ", "_")
```

- [ ] **Step 4: Run test to verify it passes**

```bash
PYTHONPATH=. pytest tests/test_philips_avent/test_sanitize.py -v
```

Expected: 4 PASS.

- [ ] **Step 5: Rewire `camera.py`**

Edit `custom_components/philips_avent/camera.py`. Find:

```python
        safe_name = coordinator.camera_name.replace(" ", "_")
        self._stream_url = f"rtsp://localhost:{bridge_port}/{safe_name}"
```

Replace with:

```python
        safe_name = sanitize_rtsp_path(coordinator.camera_name, coordinator.camera_id)
        self._stream_url = f"rtsp://localhost:{bridge_port}/{safe_name}"
```

Add to the imports at the top of `camera.py`:

```python
from .const import sanitize_rtsp_path
```

(Verify `coordinator.camera_id` is exposed by the coordinator; if it isn't, the attribute name may differ — grep the coordinator file for `camera_id` and use the actual name. If `coordinator.camera_id` does not exist as an attribute today, expose it: edit `coordinator.py` to add `self.camera_id = camera_id` in `__init__`.)

- [ ] **Step 6: Run the full Python test suite**

```bash
PYTHONPATH=. pytest tests/test_philips_avent/ -v
```

Expected: all PASS (existing + new).

- [ ] **Step 7: Lint**

```bash
ruff check custom_components/ tests/ --ignore E501
```

Expected: no issues.

- [ ] **Step 8: Commit**

```bash
git add custom_components/philips_avent/const.py custom_components/philips_avent/camera.py tests/test_philips_avent/test_sanitize.py
git commit -m "refactor(integration): share sanitize_rtsp_path between camera.py and Go bridge"
```

---

## Task 9: Simplify `run.sh`

**Files:**
- Modify: `aventproxy-bridge-addon/run.sh`

- [ ] **Step 1: Replace the per-camera extraction and exec line**

Edit `aventproxy-bridge-addon/run.sh`. Remove the block from `NUM_CAMERAS=...` (around line 43) through the end of the file's `exec` block, and replace with the simpler version. The final file should look like this (preserve the file's existing top comments, glob/legacy lookup, and md5 watcher):

```bash
#!/usr/bin/env bash
set -e

BRIDGE_CONFIG_GLOB="/config/philips_avent_bridge_*.json"
BRIDGE_CONFIG_LEGACY="/config/philips_avent_bridge.json"
ADDON_CONFIG="/data/options.json"

find_bridge_config() {
    for f in $BRIDGE_CONFIG_GLOB; do
        [ -f "$f" ] && echo "$f" && return 0
    done
    [ -f "$BRIDGE_CONFIG_LEGACY" ] && echo "$BRIDGE_CONFIG_LEGACY" && return 0
    return 1
}

if [ "${WAIT_FOR_CONFIG:-false}" = "true" ]; then
    echo "Waiting for bridge config from HA integration..."
    while ! find_bridge_config >/dev/null 2>&1 && [ ! -f "$ADDON_CONFIG" ]; do
        sleep 5
    done
    echo "Config found!"
fi

FOUND_CONFIG=$(find_bridge_config 2>/dev/null)
if [ -n "$FOUND_CONFIG" ]; then
    echo "Using bridge config from HA integration: $FOUND_CONFIG"
    CONFIG_PATH="$FOUND_CONFIG"
elif [ -f "$ADDON_CONFIG" ]; then
    echo "Using add-on options config"
    CONFIG_PATH="$ADDON_CONFIG"
else
    echo "ERROR: no bridge config found"
    exit 1
fi

echo "=============================="
echo "Philips Avent WebRTC Bridge"
echo "Config: $CONFIG_PATH"
echo "=============================="

CONFIG_HASH=$(md5sum "$CONFIG_PATH" | cut -d' ' -f1)
(
    while true; do
        sleep 10
        NEW_HASH=$(md5sum "$CONFIG_PATH" 2>/dev/null | cut -d' ' -f1)
        if [ -n "$NEW_HASH" ] && [ "$NEW_HASH" != "$CONFIG_HASH" ]; then
            echo "Config changed, restarting bridge..."
            kill $$
            exit 0
        fi
    done
) &

exec avent-webrtc-bridge addon --config "$CONFIG_PATH"
```

- [ ] **Step 2: Commit**

```bash
git add aventproxy-bridge-addon/run.sh
git commit -m "feat(addon): drive multi-camera bridge via the addon subcommand"
```

---

## Task 10: Local CI mirror — Go tests + addon docker build + smoke

**Files:** none (verification only)

- [ ] **Step 1: Run all Go tests in the container**

```bash
docker run --rm -v "$PWD/avent-webrtc-bridge":/app -w /app golang:1.26-bookworm \
  go test ./... -v
```

Expected: all PASS, including the new `pkg/storage` and `cmd/addon` tests.

- [ ] **Step 2: Run all Python tests**

```bash
PYTHONPATH=. pytest tests/test_philips_avent/ -v
```

Expected: all PASS.

- [ ] **Step 3: Ruff lint**

```bash
ruff check custom_components/ examples/ tools/ --ignore E501
```

Expected: no issues.

- [ ] **Step 4: Build the addon Docker image (mirrors the CI docker-addon job)**

```bash
rm -rf aventproxy-bridge-addon/avent-webrtc-bridge
cp -r avent-webrtc-bridge aventproxy-bridge-addon/avent-webrtc-bridge
docker build --build-arg BUILD_FROM=debian:bookworm-slim \
  -t aventproxy-bridge:test aventproxy-bridge-addon/
rm -rf aventproxy-bridge-addon/avent-webrtc-bridge
```

Expected: `Successfully tagged aventproxy-bridge:test`.

- [ ] **Step 5: Verify the binary has both subcommands**

```bash
docker run --rm aventproxy-bridge:test avent-webrtc-bridge --help
```

Expected: `addon` and `direct` both appear under "Available Commands".

- [ ] **Step 6: Smoke test the addon subcommand with a fake JSON**

```bash
cat > /tmp/fake-bridge.json <<'EOF'
{
  "signing_key": "fake",
  "sid": "fake",
  "ecode": "fake",
  "partner": "fake",
  "app_key": "fake",
  "device_id": "fake",
  "package_name": "fake",
  "bridge_port": 38554,
  "cameras": [
    {"camera_id": "fakeid1", "camera_name": "Erik"},
    {"camera_id": "fakeid2", "camera_name": "Anna"}
  ]
}
EOF

docker run --rm -v /tmp/fake-bridge.json:/cfg.json aventproxy-bridge:test \
  avent-webrtc-bridge addon --config /cfg.json 2>&1 | head -20
```

Expected: log line "Verifying API access..." followed by an authentication error from Tuya. This confirms the binary loads the JSON, validates it, and reaches the API call without crashing on parsing. It is OK and expected that it then exits with non-zero because the credentials are fake.

If you see a panic, a parsing error, or "no cameras configured" — STOP and debug. Otherwise proceed.

- [ ] **Step 7: Verify path collision branch with a colliding JSON**

```bash
cat > /tmp/collision-bridge.json <<'EOF'
{
  "signing_key": "fake",
  "sid": "fake",
  "ecode": "fake",
  "partner": "fake",
  "app_key": "fake",
  "device_id": "fake",
  "package_name": "fake",
  "bridge_port": 38554,
  "cameras": [
    {"camera_id": "aaaaaaaaaa", "camera_name": "Baby"},
    {"camera_id": "bbbbbbbbbb", "camera_name": "Baby"}
  ]
}
EOF

docker run --rm -v /tmp/collision-bridge.json:/cfg.json aventproxy-bridge:test \
  avent-webrtc-bridge addon --config /cfg.json 2>&1 | grep -E "collision|Camera registered" | head -5
```

Expected: a `WARN Path collision on /Baby, falling back to /Baby_bbbbbb` line appears before the API verification step (since path assignment happens before API call — note: in the current `runAddon` ordering, path assignment is AFTER the API call; if it doesn't appear, that's fine — the unit test already covers this branch).

- [ ] **Step 8: Cleanup**

```bash
rm -f /tmp/fake-bridge.json /tmp/collision-bridge.json
```

No commit — this task is verification only.

---

## Task 11: Update `README.md`

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update the architecture diagram**

Edit `README.md`. Find the "How It Works" ASCII diagram (around lines 65-86). Replace the `Camera Entity` block to make it explicit that multiple cameras share the same bridge process and port:

```
│  ┌────────────────────────┐  │
│  │  Camera Entities       │  │
│  │  rtsp://host:38554/N1  │  │
│  │  rtsp://host:38554/N2  │  │
│  │  ...                   │  │
│  └────────────────────────┘  │
```

- [ ] **Step 2: Add a sentence under the features table**

Find the features table (around lines 15-25). Below it, add:

```markdown
Multiple monitors on one Tuya account are supported: the bridge serves each camera from the same port on a distinct RTSP path derived from the camera's display name.
```

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document multi-camera bridge"
```

---

## Wrap-up

After Task 11 the work is done. Push when ready:

```bash
git push git@github.com:thekoma/aventproxy.git main
```

Closing comment on issue #35 (do not auto-close — let the reporter confirm):

```bash
gh issue comment 35 --body "Multi-camera support shipped in the next release. The bridge now serves every monitor on the account from the same port at distinct RTSP paths (one path per camera name). Recreate the integration in HA if your second camera was added after the initial setup. Please reopen if anything is off."
```

The decision to cut a release for this change is out of scope for this plan.
