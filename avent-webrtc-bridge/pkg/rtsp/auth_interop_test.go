package rtsp

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestDigestWithRealClients runs ffmpeg and go2rtc, the clients Home
// Assistant pulls the stream with, against the server's Digest challenge.
// The URL has no camera path, so a client that passed authentication gets a
// 400 and one that did not a 401; no camera is involved. It needs the tools,
// so it runs only with RTSP_INTEROP=1 (in a Home Assistant image, which has
// both: ffmpeg on PATH, go2rtc at GO2RTC or /bin/go2rtc).
func TestDigestWithRealClients(t *testing.T) {
	if os.Getenv("RTSP_INTEROP") != "1" {
		t.Skip("set RTSP_INTEROP=1 where ffmpeg and go2rtc are installed")
	}
	s := authServer()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handleConnection(c)
		}
	}()
	addr := ln.Addr().String()
	stream := func(password string) string {
		return "rtsp://" + url.UserPassword(RTSPUser, password).String() + "@" + addr + "/"
	}

	ffmpeg := func(password string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-rtsp_transport", "tcp",
			"-i", stream(password), "-f", "null", "-").CombinedOutput()
		return string(out)
	}
	if out := ffmpeg(testRTSPPassword); !strings.Contains(out, "400") || strings.Contains(out, "401") {
		t.Errorf("ffmpeg with the password:\n%s", out)
	}
	if out := ffmpeg("wrong"); !strings.Contains(out, "401") {
		t.Errorf("ffmpeg with a wrong password:\n%s", out)
	}

	bin := os.Getenv("GO2RTC")
	if bin == "" {
		bin = "/bin/go2rtc"
	}
	go2rtc := func(password string) string {
		api := freePort(t)
		cfg := fmt.Sprintf(`{"api":{"listen":"127.0.0.1:%d"},"rtsp":{"listen":""},"webrtc":{"listen":""},"log":{"level":"debug"},"streams":{"cam":"%s"}}`,
			api, stream(password))
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "-config", cfg)
		var log strings.Builder
		cmd.Stdout, cmd.Stderr = &log, &log
		if err := cmd.Start(); err != nil {
			t.Fatalf("start go2rtc: %v", err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		// A frame request makes go2rtc connect to the source.
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", api), time.Second)
			if err == nil {
				fmt.Fprintf(c, "GET /api/frame.jpeg?src=cam HTTP/1.0\r\n\r\n")
				_ = c.SetReadDeadline(time.Now().Add(6 * time.Second))
				buf := make([]byte, 4096)
				n, _ := c.Read(buf)
				c.Close()
				return log.String() + "\n" + string(buf[:n])
			}
			time.Sleep(200 * time.Millisecond)
		}
		return log.String()
	}
	// go2rtc (1.9.15) names no status: past authentication the 400 reads
	// "wrong response on DESCRIBE", a refused password "wrong user/pass".
	if out := go2rtc(testRTSPPassword); !strings.Contains(out, "wrong response on DESCRIBE") || strings.Contains(out, "wrong user/pass") {
		t.Errorf("go2rtc with the password:\n%s", out)
	}
	if out := go2rtc("wrong"); !strings.Contains(out, "wrong user/pass") {
		t.Errorf("go2rtc with a wrong password:\n%s", out)
	}
}
