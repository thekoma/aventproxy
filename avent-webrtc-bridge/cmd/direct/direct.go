package direct

import (
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
	"avent-webrtc-bridge/pkg/utils"
)

var storageManager *storage.StorageManager

func SetStorageManager(sm *storage.StorageManager) {
	storageManager = sm
}

func NewDirectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "direct",
		Short: "Start RTSP server using Tuya Mobile SDK API",
		Long: `Connect to a Tuya camera using the mobile SDK API with pre-configured credentials.
Bypasses the Smart Life web portal authentication entirely.

Example:
  avent-webrtc-bridge direct \
    --signing-key "pkg_cert_emb_secret" \
    --sid "eu..." \
    --app-key "wx..." \
    --device-id "4ea..." \
    --camera-id "bf3..." \
    --camera-name "Erik"`,
		RunE: runDirect,
	}

	cmd.Flags().String("signing-key", "", "HMAC-SHA256 signing key")
	cmd.Flags().String("sid", "", "Tuya session ID")
	cmd.Flags().String("ecode", "", "Tuya ecode (from login response)")
	cmd.Flags().String("partner", "", "Partner identity (from login response)")
	cmd.Flags().String("app-key", "", "Tuya app key (clientId)")
	cmd.Flags().String("device-id", "", "Phone device ID")
	cmd.Flags().String("ch-key", "071d81fa", "Channel key")
	cmd.Flags().String("package", "", "App package name (for MQTT client ID)")
	cmd.Flags().String("camera-id", "", "Camera device ID")
	cmd.Flags().String("camera-name", "", "Camera display name (used in RTSP path)")
	cmd.Flags().Int("port", 8554, "RTSP server port")

	cmd.MarkFlagRequired("signing-key")
	cmd.MarkFlagRequired("sid")
	cmd.MarkFlagRequired("ecode")
	cmd.MarkFlagRequired("partner")
	cmd.MarkFlagRequired("app-key")
	cmd.MarkFlagRequired("device-id")
	cmd.MarkFlagRequired("camera-id")

	return cmd
}

func runDirect(cmd *cobra.Command, args []string) error {
	signingKey, _ := cmd.Flags().GetString("signing-key")
	sid, _ := cmd.Flags().GetString("sid")
	ecode, _ := cmd.Flags().GetString("ecode")
	partner, _ := cmd.Flags().GetString("partner")
	appKey, _ := cmd.Flags().GetString("app-key")
	deviceID, _ := cmd.Flags().GetString("device-id")
	chKey, _ := cmd.Flags().GetString("ch-key")
	packageName, _ := cmd.Flags().GetString("package")
	cameraID, _ := cmd.Flags().GetString("camera-id")
	cameraName, _ := cmd.Flags().GetString("camera-name")
	port, _ := cmd.Flags().GetInt("port")

	rtspPath := storage.SanitizeRTSPPath(cameraName, cameraID)

	client := tuya.NewMobileSDKClient(signingKey, sid, appKey, deviceID, chKey)
	client.Ecode = ecode
	client.PartnerIdentity = partner
	client.PackageName = packageName

	core.Logger.Info().Msg("Verifying API access...")
	_, err := client.Call("smartlife.p.time.get", "1.0", nil)
	if err != nil {
		return fmt.Errorf("API verification failed: %v", err)
	}
	core.Logger.Info().Msg("API access OK")

	userInfo, err := client.GetUserInfo()
	if err != nil {
		return fmt.Errorf("failed to get user info: %v", err)
	}
	core.Logger.Info().Msgf("User: %s (%s)", userInfo.Nickname, utils.MaskEmail(userInfo.Email))
	core.Logger.Info().Msgf("MQTT domain: %s", userInfo.Domain.MobileMqttsUrl)
	client.UID = userInfo.ID

	userKey := "direct_" + strings.ReplaceAll(strings.ReplaceAll(userInfo.Email, "@", "_at_"), ".", "_")
	user := &storage.UserSession{
		Email:  userInfo.Email,
		Region: "direct",
		SessionData: &tuya.SessionData{
			LoginResult: &tuya.LoginResult{
				Uid:      userInfo.ID,
				Email:    userInfo.Email,
				Nickname: userInfo.Nickname,
				Domain:   userInfo.Domain,
			},
			ServerHost: "a1.tuyaeu.com",
			Region:     "direct",
			UserEmail:  userInfo.Email,
		},
		LastRefresh: time.Now(),
		UserKey:     userKey,
	}

	camera := storage.CameraInfo{
		DeviceID:   cameraID,
		DeviceName: cameraName,
		Category:   "sp",
		RTSPPath:   rtspPath,
		UserKey:    userKey,
	}

	if err := storageManager.SaveUser("direct", userInfo.Email, user.SessionData); err != nil {
		core.Logger.Warn().Msgf("Could not save user session: %v", err)
	}
	if err := storageManager.UpdateCamerasForUser(userKey, []storage.CameraInfo{camera}); err != nil {
		core.Logger.Warn().Msgf("Could not save camera: %v", err)
	}

	server := rtsp.NewRTSPServer(port, storageManager)
	server.MobileClient = client

	if err := server.Start(); err != nil {
		return fmt.Errorf("failed to start RTSP server: %v", err)
	}

	core.Logger.Info().Msgf("RTSP endpoints:")
	core.Logger.Info().Msgf("  HD: rtsp://localhost:%d%s", port, rtspPath)
	core.Logger.Info().Msgf("  SD: rtsp://localhost:%d%s/sd", port, rtspPath)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	core.Logger.Info().Msg("Shutting down...")
	server.Stop()
	return nil
}
