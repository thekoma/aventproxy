package storage

import "strings"

// SanitizeRTSPPath converts a camera display name into an RTSP path component.
// It replaces spaces, forward slashes, and backslashes with underscores.
// If the result is empty or just "_", falls back to the camera id.
// The returned string starts with "/".
func SanitizeRTSPPath(name, id string) string {
	sanitized := strings.ReplaceAll(name, " ", "_")
	sanitized = strings.ReplaceAll(sanitized, "/", "_")
	sanitized = strings.ReplaceAll(sanitized, "\\", "_")

	// If name is empty or too generic, use device ID
	if sanitized == "" || sanitized == "_" {
		sanitized = id
	}

	return "/" + sanitized
}
