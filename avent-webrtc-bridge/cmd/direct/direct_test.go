package direct

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/rtsp"
)

func TestConfigureServerFromTheFlags(t *testing.T) {
	cmd := NewDirectCmd()
	for flag, value := range map[string]string{"talkback": "true", "webrtc-lan-only": "true", "lan-ip": "192.168.1.30"} {
		if err := cmd.Flags().Set(flag, value); err != nil {
			t.Fatal(err)
		}
	}
	server := rtsp.NewRTSPServer(0, nil)
	configureServer(server, cmd, "cam1")
	if !server.Talkback || !server.WebRTCLANOnly || server.CameraLANHosts["cam1"] != "192.168.1.30" {
		t.Fatalf("talkback %v, LAN only %v, hosts %v", server.Talkback, server.WebRTCLANOnly, server.CameraLANHosts)
	}

	plain := rtsp.NewRTSPServer(0, nil)
	configureServer(plain, NewDirectCmd(), "cam1")
	if plain.Talkback || plain.WebRTCLANOnly || plain.CameraLANHosts != nil {
		t.Fatalf("defaults: talkback %v, LAN only %v, hosts %v", plain.Talkback, plain.WebRTCLANOnly, plain.CameraLANHosts)
	}
}

// With --webrtc-lan-only, a --lan-ip the rewrite cannot use gets one warning
// at start, without the address; a usable one and one without the option none.
func TestConfigureServerWarnsOfAnUnusableLANIP(t *testing.T) {
	saved := core.Logger
	t.Cleanup(func() { core.Logger = saved })
	for _, tc := range []struct {
		lanOnly, lanIP, want string
	}{
		{"true", "203.0.113.9", "LAN-only WebRTC: --lan-ip is not a LAN address"},
		{"true", "fe80::1%eth0", "LAN-only WebRTC: --lan-ip is an IPv6 address with a zone"},
		{"true", "camera.local", "LAN-only WebRTC: --lan-ip is not an IP address"},
		{"true", "192.168.1.30", ""},
		{"false", "203.0.113.9", ""},
	} {
		var buf bytes.Buffer
		core.Logger = zerolog.New(&buf)
		cmd := NewDirectCmd()
		if cmd.Flags().Set("webrtc-lan-only", tc.lanOnly) != nil || cmd.Flags().Set("lan-ip", tc.lanIP) != nil {
			t.Fatal("flags")
		}
		configureServer(rtsp.NewRTSPServer(0, nil), cmd, "cam1")
		out := buf.String()
		if tc.want == "" {
			if out != "" {
				t.Errorf("%s %s: logged %q", tc.lanOnly, tc.lanIP, out)
			}
			continue
		}
		if !strings.Contains(out, tc.want) || strings.Count(out, `"level":"warn"`) != 1 || strings.Contains(out, tc.lanIP) {
			t.Errorf("%s: want one warning %q without the address, got %q", tc.lanIP, tc.want, out)
		}
	}
}
