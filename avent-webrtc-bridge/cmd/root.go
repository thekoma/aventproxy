package cmd

import (
	"fmt"
	"os"

	"avent-webrtc-bridge/cmd/auth"
	"avent-webrtc-bridge/cmd/cameras"
	"avent-webrtc-bridge/cmd/direct"
	"avent-webrtc-bridge/cmd/rtsp"
	"avent-webrtc-bridge/pkg/storage"

	"github.com/spf13/cobra"
)

var (
	storageManager *storage.StorageManager
)

var rootCmd = &cobra.Command{
	Use:   "avent-webrtc-bridge",
	Short: "Tuya Smart Camera RTSP Bridge",
	Long: `A CLI tool to connect Tuya Smart Cameras to RTSP clients.

This tool allows you to:
- Authenticate with Tuya Smart accounts
- Discover cameras
- Provide RTSP endpoints for your cameras

Examples:
  avent-webrtc-bridge auth list
  avent-webrtc-bridge auth add eu-central user@example.com
  avent-webrtc-bridge cameras refresh
  avent-webrtc-bridge rtsp start --port 8554`,
}

func Execute(version string) error {
	rootCmd.Version = version
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)

	// Add subcommands
	rootCmd.AddCommand(auth.NewAuthCmd())
	rootCmd.AddCommand(cameras.NewCamerasCmd())
	rootCmd.AddCommand(rtsp.NewRTSPCmd())
	rootCmd.AddCommand(direct.NewDirectCmd())
}

func initConfig() {
	var err error
	storageManager, err = storage.NewStorageManager()
	if err != nil {
		fmt.Println("Failed to initialize storage")
		os.Exit(1)
	}

	// Make storage manager available to subcommands
	auth.SetStorageManager(storageManager)
	cameras.SetStorageManager(storageManager)
	rtsp.SetStorageManager(storageManager)
	direct.SetStorageManager(storageManager)
}

func GetStorageManager() *storage.StorageManager {
	return storageManager
}
