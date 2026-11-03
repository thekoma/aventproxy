package addon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	"avent-webrtc-bridge/pkg/utils"
)

// BridgeConfig is the JSON shape written by the HA integration
// in custom_components/philips_avent/__init__.py::_write_bridge_config.
type BridgeConfig struct {
	SigningKey  string `json:"signing_key"`
	SID         string `json:"sid"`
	Ecode       string `json:"ecode"`
	Partner     string `json:"partner"`
	AppKey      string `json:"app_key"`
	DeviceID    string `json:"device_id"`
	PackageName string `json:"package_name"`
	APIHost     string `json:"api_host"`
	Talkback    bool   `json:"talkback"`
	// UID is the Tuya account id, needed for LAN sessions and cloud-free starts.
	UID string `json:"uid"`
	// LanStreaming is a pointer so an absent field means enabled.
	LanStreaming *bool `json:"lan_streaming"`
	BridgePort   int   `json:"bridge_port"`
	// RTSPBind is optional: the --bind flag wins when it is given.
	RTSPBind string   `json:"rtsp_bind_address"`
	Cameras  []Camera `json:"cameras"`
}

// Camera is one entry under "cameras" in the JSON.
type Camera struct {
	ID        string `json:"camera_id"`
	Name      string `json:"camera_name"`
	ProductID string `json:"product_id"`
	// Secrets for the LAN path; they stay in memory (RTSPServer.LANTargets).
	LocalKey    string `json:"local_key"`
	P2PPassword string `json:"p2p_password"`
	LanIP       string `json:"lan_ip"`
	LanProtocol string `json:"lan_protocol"`
}

func lanStreamingEnabled(cfg BridgeConfig) bool {
	return cfg.LanStreaming == nil || *cfg.LanStreaming
}

// lanTargets keeps the cameras that have both a local key and a P2P password,
// keyed by camera id.
func lanTargets(cams []Camera) map[string]rtsp.LANTarget {
	out := make(map[string]rtsp.LANTarget)
	for _, c := range cams {
		if c.LocalKey != "" && c.P2PPassword != "" {
			out[c.ID] = rtsp.LANTarget{LocalKey: c.LocalKey, Password: c.P2PPassword, IP: c.LanIP, Protocol: c.LanProtocol}
		}
	}
	return out
}

func storedUID(u storage.UserSession) string {
	if u.SessionData == nil || u.SessionData.LoginResult == nil {
		return ""
	}
	return u.SessionData.LoginResult.Uid
}

// cloudFallbackUser decides what a failed cloud check means: with LAN targets
// and this account's stored add-on user the bridge can still serve LAN
// streams, otherwise the original error stands. The data dir holds every
// account's user, so only this account's is taken: the one saved with the
// configured uid, or else the one that registered a camera this config
// streams over the LAN. The user is saved with the cloud's user id, which is
// expected to match the integration's uid; the cameras still name the
// account if it ever does not. A config without uid predates LAN streaming
// and has no LAN targets anyway.
func cloudFallbackUser(cloudErr error, cfgUID string, targets map[string]rtsp.LANTarget, stored []storage.UserSession, registry []storage.CameraInfo) (*storage.UserSession, error) {
	if len(targets) == 0 || cfgUID == "" {
		return nil, cloudErr
	}
	for i := range stored {
		if stored[i].Region == "addon" && storedUID(stored[i]) == cfgUID {
			return &stored[i], nil
		}
	}
	for i := range stored {
		if stored[i].Region == "addon" && ownsAny(registry, stored[i].UserKey, targets) {
			return &stored[i], nil
		}
	}
	return nil, cloudErr
}

// ownsAny reports whether userKey registered any of the cameras in targets.
func ownsAny(registry []storage.CameraInfo, userKey string, targets map[string]rtsp.LANTarget) bool {
	for _, c := range registry {
		if _, ok := targets[c.DeviceID]; ok && c.UserKey == userKey {
			return true
		}
	}
	return false
}

// cloudCheckWarning says what a failed cloud check means for the log. Only a
// network error means the cloud is unreachable, and the cloud path comes back
// with it; an API error, such as an expired sid, means it answered and refused.
func cloudCheckWarning(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return fmt.Sprintf("Cloud unreachable, serving streams with LAN signalling until it returns: %v", err)
	}
	return fmt.Sprintf("Cloud check failed, serving streams with LAN signalling: %v", err)
}

// lanUID is the account id LAN offers carry: the integration's uid, or the
// cloud's when the config has none. The cloud and MQTT always use the cloud's
// user id, the value tested there (WHITEPAPER); differ says the two disagree.
func lanUID(cfgUID, cloudUID string) (uid string, differ bool) {
	if cfgUID == "" {
		return cloudUID, false
	}
	return cfgUID, cloudUID != "" && cloudUID != cfgUID
}

// verifyCloud checks API access and fetches the user.
func verifyCloud(client *tuya.MobileSDKClient) (*tuya.UserInfoResult, error) {
	core.Logger.Info().Msg("Verifying API access...")
	if _, err := client.Call("smartlife.p.time.get", "1.0", nil); err != nil {
		return nil, fmt.Errorf("API verification failed: %w", err)
	}
	core.Logger.Info().Msg("API access OK")
	userInfo, err := client.GetUserInfo()
	if err != nil {
		return nil, fmt.Errorf("get user info: %w", err)
	}
	return userInfo, nil
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

		basePath := storage.SanitizeRTSPPath(cam.Name, cam.ID)
		path := basePath
		if seenPaths[path] {
			suffix := cam.ID
			if len(suffix) > 6 {
				suffix = suffix[:6]
			}
			path = basePath + "_" + suffix
			i := 2
			for seenPaths[path] {
				path = fmt.Sprintf("%s_%s_%d", basePath, suffix, i)
				i++
			}
			core.Logger.Warn().Msgf("Path collision on %s, falling back to %s", basePath, path)
		}
		seenPaths[path] = true
		out = append(out, CameraWithPath{Camera: cam, Path: path})
	}
	return out
}

// DefaultBind keeps the unauthenticated RTSP server off the LAN. Home Assistant
// runs on the host network next to the add-on, so loopback is enough for it.
const DefaultBind = "localhost"

// resolveBind picks the bind address: an explicit --bind flag, then the
// rtsp_bind_address field of the config JSON, then DefaultBind.
func resolveBind(flagValue string, flagSet bool, cfg BridgeConfig) string {
	if flagSet {
		return flagValue
	}
	if b := strings.TrimSpace(cfg.RTSPBind); b != "" {
		return b
	}
	return DefaultBind
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
	cmd.Flags().String("bind", DefaultBind, "Interfaces the RTSP server listens on: localhost (127.0.0.1 and ::1), "+
		"an address or comma-separated list, or 0.0.0.0 for every interface. The server has no authentication, "+
		"so open it to the network only when another machine must reach it")
	// Issue #72 experiment knobs, set from the Dev card's options by run.sh.
	// Speaker-off stays off elsewhere until a tester confirms it.
	cmd.Flags().Bool("speaker-off", false, "Ask the monitor to keep its speaker closed once a cloud session "+
		"without talkback is up (issue #72, experimental)")
	cmd.Flags().String("webrtc-audio", rtsp.WebRTCAudioRecvonly, "Audio a cloud offer asks the camera for: "+
		"recvonly, or none to leave it out (issue #72 experiment; RTSP clients see the same description)")
	cmd.Flags().String("cloud-transport", rtsp.CloudTransportWebRTC, "Cloud session for streams that do not take "+
		"the LAN: webrtc, or imm to try the app's own transport over cloud signalling first and fall back to "+
		"webrtc (experimental)")
	cmd.MarkFlagRequired("config")
	return cmd
}

// cloudTransportFlag reads and checks --cloud-transport.
func cloudTransportFlag(cmd *cobra.Command) (string, error) {
	v, err := cmd.Flags().GetString("cloud-transport")
	if err != nil {
		return "", err
	}
	return rtsp.ParseCloudTransport(v)
}

// sessionFlags reads and checks the issue #72 knobs.
func sessionFlags(cmd *cobra.Command) (speakerOff bool, webrtcAudio string, err error) {
	speakerOff, err = cmd.Flags().GetBool("speaker-off")
	if err != nil {
		return false, "", err
	}
	mode, err := cmd.Flags().GetString("webrtc-audio")
	if err != nil {
		return false, "", err
	}
	webrtcAudio, err = rtsp.ParseWebRTCAudio(mode)
	return speakerOff, webrtcAudio, err
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
	speakerOff, webrtcAudio, err := sessionFlags(cmd)
	if err != nil {
		return err
	}
	cloudTransport, err := cloudTransportFlag(cmd)
	if err != nil {
		return err
	}

	port := cfg.BridgePort
	if port == 0 {
		port = 38554
	}

	apiHost := tuya.NormalizeAPIHost(cfg.APIHost)
	client := tuya.NewMobileSDKClient(cfg.SigningKey, cfg.SID, cfg.AppKey, cfg.DeviceID, "071d81fa")
	client.BaseURL = tuya.APIBaseURL(apiHost)
	client.Ecode = cfg.Ecode
	client.PartnerIdentity = cfg.Partner
	client.PackageName = cfg.PackageName

	core.Logger.Info().Msgf("Tuya API host: %s", apiHost)
	targets := lanTargets(cfg.Cameras)

	var userKey string
	userInfo, err := verifyCloud(client)
	if err != nil {
		stored, lerr := storageManager.ListUsers()
		if lerr != nil {
			core.Logger.Warn().Msgf("Could not read the stored users: %v", lerr)
		}
		registry, rerr := storageManager.GetAllCameras()
		if rerr != nil {
			core.Logger.Warn().Msgf("Could not read the camera registry: %v", rerr)
		}
		fallback, ferr := cloudFallbackUser(err, cfg.UID, targets, stored, registry)
		if ferr != nil {
			return ferr
		}
		core.Logger.Warn().Msg(cloudCheckWarning(err))
		userKey = fallback.UserKey
		client.UID = cfg.UID // MQTTManager takes the cloud's user id once the cloud answers
	} else {
		core.Logger.Info().Msgf("User: %s (%s)", userInfo.Nickname, utils.MaskEmail(userInfo.Email))
		client.UID = userInfo.ID
		userKey = "addon_" + strings.ReplaceAll(strings.ReplaceAll(userInfo.Email, "@", "_at_"), ".", "_")
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
				ServerHost: apiHost,
				Region:     "addon",
				UserEmail:  userInfo.Email,
			},
			LastRefresh: time.Now(),
			UserKey:     userKey,
		}
		if err := storageManager.SaveUser("addon", userInfo.Email, user.SessionData); err != nil {
			core.Logger.Warn().Msgf("Could not save user session: %v", err)
		}
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
			ProductID:  c.ProductID,
			RTSPPath:   c.Path,
			UserKey:    userKey,
		})
		pathLog = append(pathLog, c.Path)
		core.Logger.Info().Msgf("Camera registered: id=%s name=%s path=%s", c.ID, c.Name, c.Path)
	}
	if err := storageManager.UpdateCamerasForUser(userKey, infos); err != nil {
		core.Logger.Warn().Msgf("Could not save cameras: %v", err)
	}

	bindFlag, _ := cmd.Flags().GetString("bind")
	bind := resolveBind(bindFlag, cmd.Flags().Changed("bind"), cfg)

	server := rtsp.NewRTSPServer(port, storageManager)
	server.BindAddress = bind
	server.MobileClient = client
	server.Talkback = cfg.Talkback
	server.SpeakerOff = speakerOff
	server.WebRTCAudio = webrtcAudio
	server.CloudTransport = cloudTransport
	server.LanStreaming = lanStreamingEnabled(cfg)
	uid, differ := lanUID(cfg.UID, client.UID)
	if differ {
		core.Logger.Warn().Msg("The integration's uid differs from the cloud's user id: the cloud uses the latter, LAN offers the former")
	}
	server.UID = uid
	server.LANTargets = targets
	core.Logger.Info().Msgf("LAN streaming %t, %d cameras with LAN targets", server.LanStreaming, len(targets))
	if cfg.Talkback {
		core.Logger.Info().Msg("Two-way audio enabled: streams will ask the camera for talkback")
		if webrtcAudio == rtsp.WebRTCAudioNone {
			core.Logger.Warn().Msg("webrtc_audio none is ignored: two-way audio needs the audio section")
		}
		core.Logger.Info().Msg("Cloud sessions: audio sendrecv, speaker left to talkback")
	} else {
		core.Logger.Info().Msgf("Cloud sessions: audio %s, speaker off %t", webrtcAudio, speakerOff)
	}
	if cloudTransport == rtsp.CloudTransportIMM {
		core.Logger.Info().Msg("Cloud transport imm (experimental): streams that do not take the LAN try imm " +
			"over cloud signalling first, and fall back to webrtc")
	}
	if err := server.Start(); err != nil {
		return fmt.Errorf("start RTSP server: %w", err)
	}
	core.Logger.Info().Msgf("Serving %d cameras on port %d (bind %s): %s", len(infos), port, bind, strings.Join(pathLog, " "))

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	core.Logger.Info().Msg("Shutting down...")
	server.Stop()
	return nil
}
