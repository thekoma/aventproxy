package addon

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

// BridgeConfig is the JSON shape written by the HA integration
// in custom_components/philips_avent/__init__.py::_write_bridge_config.
type BridgeConfig struct {
	SigningKey   string   `json:"signing_key"`
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

// CameraWithPath pairs a Camera with the RTSP path it will be served on.
type CameraWithPath struct {
	Camera
	Path string
}

// assignPaths sanitizes each camera's name into an RTSP path and filters out
// invalid entries. Behavior:
//   - skip entries with empty camera_id (warn);
//   - skip later entries that repeat a previously-seen camera_id (warn) — a single
//     physical device must not be registered twice;
//   - when two distinct cameras sanitize to the same path, the second one gets
//     a suffix derived from up to 6 characters of its camera_id (warn).
func assignPaths(cams []Camera) []CameraWithPath {
	out := make([]CameraWithPath, 0, len(cams))
	seenIDs := make(map[string]bool, len(cams))
	seenPaths := make(map[string]bool, len(cams))
	for _, cam := range cams {
		if cam.ID == "" {
			core.Logger.Warn().Msgf("Camera config invalid: missing camera_id, skipping name=%q", cam.Name)
			continue
		}
		if seenIDs[cam.ID] {
			core.Logger.Warn().Msgf("Duplicate camera_id %q, skipping repeated entry name=%q", cam.ID, cam.Name)
			continue
		}
		seenIDs[cam.ID] = true

		path := storage.SanitizeRTSPPath(cam.Name, cam.ID)
		if seenPaths[path] {
			suffix := cam.ID
			if len(suffix) > 6 {
				suffix = suffix[:6]
			}
			collided := path + "_" + suffix
			core.Logger.Warn().Msgf("Path collision on %s, falling back to %s", path, collided)
			path = collided
		}
		seenPaths[path] = true
		out = append(out, CameraWithPath{Camera: cam, Path: path})
	}
	return out
}

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
	if cfg.Ecode == "" {
		return fmt.Errorf("ecode is required")
	}
	if cfg.Partner == "" {
		return fmt.Errorf("partner is required")
	}
	if len(cfg.Cameras) == 0 {
		return fmt.Errorf("cameras list is empty: nothing to serve")
	}
	return nil
}

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
	if len(camsWithPath) == 0 {
		return fmt.Errorf("no valid cameras after filtering, refusing to start")
	}

	infos := make([]storage.CameraInfo, 0, len(camsWithPath))
	pathLog := make([]string, 0, len(camsWithPath))
	for _, c := range camsWithPath {
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
