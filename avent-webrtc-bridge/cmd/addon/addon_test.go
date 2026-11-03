package addon

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"avent-webrtc-bridge/pkg/rtsp"
	"avent-webrtc-bridge/pkg/storage"
	"avent-webrtc-bridge/pkg/tuya"
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
	    {"camera_id": "abc123", "camera_name": "Erik", "product_id": "selj2idknqhjnids"},
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
	if cfg.Cameras[0].ProductID != "selj2idknqhjnids" {
		t.Errorf("camera[0].ProductID = %q, want selj2idknqhjnids", cfg.Cameras[0].ProductID)
	}
	if cfg.Cameras[1].ID != "def456" || cfg.Cameras[1].Name != "Anna" {
		t.Errorf("camera[1] = %+v", cfg.Cameras[1])
	}
	// Cameras without product_id should parse as empty string (no regression for SCD973).
	if cfg.Cameras[1].ProductID != "" {
		t.Errorf("camera[1].ProductID = %q, want empty string", cfg.Cameras[1].ProductID)
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
		Ecode:   "E",
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

func TestAssignPaths_PreservesProductID(t *testing.T) {
	cams := []Camera{
		{ID: "abc123", Name: "Erik", ProductID: "selj2idknqhjnids"},
		{ID: "def456", Name: "Anna", ProductID: ""},
	}
	out := assignPaths(cams)
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	if out[0].ProductID != "selj2idknqhjnids" {
		t.Errorf("out[0].ProductID = %q, want selj2idknqhjnids", out[0].ProductID)
	}
	if out[1].ProductID != "" {
		t.Errorf("out[1].ProductID = %q, want empty string (SCD973 no-regression)", out[1].ProductID)
	}
}

func TestLoadConfig_APIHost(t *testing.T) {
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
	  "api_host": "a1.tuyaus.com",
	  "bridge_port": 38554,
	  "cameras": [{"camera_id": "abc123", "camera_name": "Erik"}]
	}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.APIHost != "a1.tuyaus.com" {
		t.Errorf("APIHost = %q, want a1.tuyaus.com", cfg.APIHost)
	}
	if got := tuya.APIBaseURL(cfg.APIHost); got != "https://a1.tuyaus.com/api.json" {
		t.Errorf("base URL = %q", got)
	}
}

func TestLoadConfig_APIHostMissingFallsBackToEU(t *testing.T) {
	// Config files written by older integration versions have no api_host;
	// those installs were all on the Central Europe data center.
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
	  "cameras": [{"camera_id": "abc123", "camera_name": "Erik"}]
	}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.APIHost != "" {
		t.Errorf("APIHost = %q, want empty", cfg.APIHost)
	}
	if err := validateConfig(cfg); err != nil {
		t.Errorf("config without api_host must stay valid: %v", err)
	}
	if got := tuya.NormalizeAPIHost(cfg.APIHost); got != "a1.tuyaeu.com" {
		t.Errorf("fallback host = %q, want a1.tuyaeu.com", got)
	}
}

func TestLoadConfig_TalkbackDefaultsToOff(t *testing.T) {
	// Issue #72: asking the camera for two-way audio stops a playing lullaby, so
	// a config written without the field must not enable it.
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.json")
	body := `{
	  "signing_key": "sk", "sid": "S", "ecode": "E", "partner": "P",
	  "app_key": "AK", "device_id": "D", "package_name": "pkg",
	  "api_host": "a1.tuyaeu.com", "bridge_port": 38554,
	  "cameras": [{"camera_id": "abc123", "camera_name": "Erik"}]
	}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Talkback {
		t.Error("talkback must default to false when the key is absent")
	}
}

func TestLoadConfig_TalkbackHonoured(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.json")
	body := `{
	  "signing_key": "sk", "sid": "S", "ecode": "E", "partner": "P",
	  "app_key": "AK", "device_id": "D", "package_name": "pkg",
	  "api_host": "a1.tuyaeu.com", "talkback": true, "bridge_port": 38554,
	  "cameras": [{"camera_id": "abc123", "camera_name": "Erik"}]
	}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.Talkback {
		t.Error("talkback true in the config must be honoured")
	}
}

func TestResolveBind(t *testing.T) {
	none := BridgeConfig{}
	fromJSON := BridgeConfig{RTSPBind: "192.168.1.10"}

	if got := resolveBind("localhost", false, none); got != DefaultBind {
		t.Errorf("no flag, no JSON: got %q, want %q", got, DefaultBind)
	}
	if DefaultBind != "localhost" {
		t.Errorf("DefaultBind = %q: the unauthenticated RTSP server must default to loopback", DefaultBind)
	}
	if got := resolveBind("localhost", false, fromJSON); got != "192.168.1.10" {
		t.Errorf("JSON value ignored: got %q", got)
	}
	if got := resolveBind("0.0.0.0", true, fromJSON); got != "0.0.0.0" {
		t.Errorf("explicit flag must win over JSON: got %q", got)
	}
	if got := resolveBind("localhost", false, BridgeConfig{RTSPBind: "  "}); got != DefaultBind {
		t.Errorf("blank JSON value: got %q, want %q", got, DefaultBind)
	}
}

func TestLoadConfig_RTSPBind(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.json")
	body := `{
	  "signing_key": "sk", "sid": "S", "ecode": "E", "partner": "P",
	  "app_key": "AK", "device_id": "D", "rtsp_bind_address": "0.0.0.0",
	  "cameras": [{"camera_id": "abc123", "camera_name": "Erik"}]
	}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.RTSPBind != "0.0.0.0" {
		t.Errorf("RTSPBind = %q, want 0.0.0.0", cfg.RTSPBind)
	}
}

func writeCfg(t *testing.T, body string) BridgeConfig {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bridge.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return cfg
}

func TestLoadConfigWithLANFields(t *testing.T) {
	cfg := writeCfg(t, `{
	  "signing_key": "sk", "sid": "S", "ecode": "E", "partner": "P",
	  "app_key": "AK", "device_id": "D", "uid": "u1", "lan_streaming": false,
	  "cameras": [{"camera_id": "abc123", "camera_name": "Erik",
	    "local_key": "lk", "p2p_password": "pw", "lan_ip": "192.0.2.7", "lan_protocol": "3.3"}]
	}`)
	if cfg.UID != "u1" {
		t.Errorf("UID = %q", cfg.UID)
	}
	if lanStreamingEnabled(cfg) {
		t.Error("lan_streaming false must disable LAN streaming")
	}
	got, ok := lanTargets(cfg.Cameras)["abc123"]
	want := rtsp.LANTarget{LocalKey: "lk", Password: "pw", IP: "192.0.2.7", Protocol: "3.3"}
	if !ok || got != want {
		t.Errorf("target = %+v (present %v), want %+v", got, ok, want)
	}
}

func TestLoadConfigWithoutLANFields(t *testing.T) {
	cfg := writeCfg(t, `{
	  "signing_key": "sk", "sid": "S", "ecode": "E", "partner": "P",
	  "app_key": "AK", "device_id": "D",
	  "cameras": [{"camera_id": "abc123", "camera_name": "Erik"}]
	}`)
	if !lanStreamingEnabled(cfg) {
		t.Error("an absent lan_streaming must mean enabled")
	}
	if n := len(lanTargets(cfg.Cameras)); n != 0 {
		t.Errorf("targets = %d, want 0", n)
	}
}

func TestLanTargetsNeedKeyAndPassword(t *testing.T) {
	got := lanTargets([]Camera{
		{ID: "a", LocalKey: "lk"},
		{ID: "b", P2PPassword: "pw"},
		{ID: "c", LocalKey: "lk", P2PPassword: "pw"},
	})
	if len(got) != 1 {
		t.Fatalf("targets = %v, want only c", got)
	}
	if _, ok := got["c"]; !ok {
		t.Errorf("c missing: %v", got)
	}
}

func addonUser(key, uid string) storage.UserSession {
	return storage.UserSession{
		Region: "addon", UserKey: key,
		SessionData: &tuya.SessionData{LoginResult: &tuya.LoginResult{Uid: uid}},
	}
}

func TestCloudFallbackUser(t *testing.T) {
	errCloud := errors.New("API verification failed: down")
	targets := map[string]rtsp.LANTarget{"a": {LocalKey: "lk", Password: "pw"}}
	a, b := addonUser("addon_a", "ua"), addonUser("addon_b", "ub")
	other := storage.UserSession{Region: "other", UserKey: "other_x"}

	// The registry from the last cloud start: addon_b registered camera "a".
	registry := []storage.CameraInfo{{UserKey: "addon_b", DeviceID: "a"}, {UserKey: "addon_a", DeviceID: "other-cam"}}

	cases := []struct {
		name     string
		uid      string
		targets  map[string]rtsp.LANTarget
		stored   []storage.UserSession
		registry []storage.CameraInfo
		wantKey  string // empty: the original error
	}{
		{"configured uid picks the matching user", "ub", targets, []storage.UserSession{a, b}, nil, "addon_b"},
		{"configured uid skips other regions", "ua", targets, []storage.UserSession{other, a}, nil, "addon_a"},
		{"configured uid without a match fails", "uz", targets, []storage.UserSession{a, b}, nil, ""},
		{"no uid fails, even with a sole add-on user", "", targets, []storage.UserSession{a}, registry, ""},
		{"no LAN targets fails", "ua", nil, []storage.UserSession{a}, nil, ""},
		{"no stored add-on user fails", "ua", targets, []storage.UserSession{other}, nil, ""},
		// The user is saved under the cloud's user id. Should it ever differ
		// from the integration's uid, the cameras still name the account.
		{"a uid that differs falls back to the user owning a LAN camera", "uz", targets, []storage.UserSession{a, b}, registry, "addon_b"},
		{"the configured uid wins over camera ownership", "ua", targets, []storage.UserSession{a, b}, registry, "addon_a"},
		{"owning only other cameras is not enough", "uz", targets, []storage.UserSession{a}, registry, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := cloudFallbackUser(errCloud, c.uid, c.targets, c.stored, c.registry)
			if c.wantKey == "" {
				if u != nil || !errors.Is(err, errCloud) {
					t.Errorf("got %+v, %v; want the original error", u, err)
				}
				return
			}
			if err != nil || u == nil || u.UserKey != c.wantKey {
				t.Errorf("got %+v, %v", u, err)
			}
		})
	}
}

func TestLanUID(t *testing.T) {
	cases := []struct {
		cfgUID, cloudUID, want string
		differ                 bool
	}{
		{"u1", "u1", "u1", false},
		{"", "u1", "u1", false},  // a config without uid takes the cloud's
		{"u1", "", "u1", false},  // the cloud returned no id
		{"u1", "u2", "u1", true}, // LAN offers keep the integration's uid
	}
	for _, c := range cases {
		got, differ := lanUID(c.cfgUID, c.cloudUID)
		if got != c.want || differ != c.differ {
			t.Errorf("lanUID(%q, %q) = %q, %v; want %q, %v", c.cfgUID, c.cloudUID, got, differ, c.want, c.differ)
		}
	}
}

func TestCloudCheckWarning(t *testing.T) {
	unreachable := fmt.Errorf("API verification failed: %w", &url.Error{Op: "Post", URL: "https://a1.tuyaeu.com/api.json", Err: &net.OpError{Op: "dial", Err: errors.New("no route to host")}})
	if got := cloudCheckWarning(unreachable); !strings.HasPrefix(got, "Cloud unreachable,") {
		t.Errorf("a network error reads %q", got)
	}
	expired := fmt.Errorf("API verification failed: %w", errors.New("API error: session expired (code: USER_SESSION_INVALID)"))
	if got := cloudCheckWarning(expired); strings.Contains(got, "unreachable") {
		t.Errorf("an API error such as an expired sid reads %q; the cloud answered", got)
	}
}

// The issue #72 knobs come from the Dev card's options through run.sh. A card
// that does not define them passes no flag, and the defaults apply.
func TestSessionFlags(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		speakerOff bool
		audio      string
		wantErr    bool
	}{
		{nil, true, rtsp.WebRTCAudioRecvonly, false},
		{[]string{"--speaker-off=false"}, false, rtsp.WebRTCAudioRecvonly, false},
		{[]string{"--speaker-off=true", "--webrtc-audio", "none"}, true, rtsp.WebRTCAudioNone, false},
		{[]string{"--webrtc-audio", "sendrecv"}, false, "", true},
	} {
		cmd := NewAddonCmd()
		if err := cmd.ParseFlags(tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		speakerOff, audio, err := sessionFlags(cmd)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%v: want an error", tc.args)
			}
			continue
		}
		if err != nil || speakerOff != tc.speakerOff || audio != tc.audio {
			t.Errorf("%v: got %v, %q, %v; want %v, %q", tc.args, speakerOff, audio, err, tc.speakerOff, tc.audio)
		}
	}
}
