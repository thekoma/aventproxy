package rtsp

import (
	"net"
	"reflect"
	"strconv"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"
)

func TestBindHosts(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{""}},
		{"   ", []string{""}},
		{"localhost", []string{"127.0.0.1", "::1"}},
		{"LOCALHOST", []string{"127.0.0.1", "::1"}},
		{"127.0.0.1", []string{"127.0.0.1"}},
		{"0.0.0.0", []string{"0.0.0.0"}},
		{"[::1]", []string{"::1"}},
		{"127.0.0.1, 192.168.1.10", []string{"127.0.0.1", "192.168.1.10"}},
		{"localhost,127.0.0.1", []string{"127.0.0.1", "::1"}},
		{",,", []string{""}},
	}
	for _, c := range cases {
		if got := bindHosts(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("bindHosts(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// freePort returns a TCP port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	ln, port := listenOnEphemeral(t)
	ln.Close()
	return port
}

// newTestServer builds a server with a throwaway storage directory.
func newTestServer(t *testing.T, bind string) (*RTSPServer, int) {
	t.Helper()
	t.Chdir(t.TempDir())
	sm, err := storage.NewStorageManager()
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	port := freePort(t)
	s := NewRTSPServer(port, sm)
	s.BindAddress = bind
	return s, port
}

// firstNonLoopbackIPv4 returns an address of this host that is not loopback,
// or nil when the host has none (then the LAN half of the test is skipped).
func firstNonLoopbackIPv4() net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP
		}
	}
	return nil
}

func TestStartOnLoopbackIsNotReachableFromLAN(t *testing.T) {
	s, port := newTestServer(t, "127.0.0.1")
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	if len(s.listeners) != 1 {
		t.Fatalf("listeners = %d, want 1", len(s.listeners))
	}
	if ip := s.listeners[0].Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Errorf("listening on %s, want loopback", ip)
	}

	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatalf("loopback dial: %v", err)
	}
	c.Close()

	lan := firstNonLoopbackIPv4()
	if lan == nil {
		t.Skip("no non-loopback IPv4 address on this host")
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(lan.String(), strconv.Itoa(port)), time.Second); err == nil {
		c.Close()
		t.Errorf("RTSP port answered on %s although bound to 127.0.0.1", lan)
	}
}

func TestStartLocalhostBindsOnlyLoopback(t *testing.T) {
	// ::1 may be missing (IPv6 off); the server must still start on 127.0.0.1.
	s, _ := newTestServer(t, "localhost")
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	if len(s.listeners) == 0 {
		t.Fatal("no listeners")
	}
	for _, ln := range s.listeners {
		if ip := ln.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
			t.Errorf("listening on %s, want loopback only", ip)
		}
	}
}

func TestStartFailsWhenNoAddressCanBeBound(t *testing.T) {
	// 192.0.2.1 is TEST-NET-1 and never assigned to a local interface.
	s, _ := newTestServer(t, "192.0.2.1")
	if err := s.Start(); err == nil {
		s.Stop()
		t.Fatal("start succeeded on an address this host does not own")
	}
}

func TestLocalIP(t *testing.T) {
	ln, _ := listenOnEphemeral(t)
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if ip := localIP(c); ip == nil || !ip.IsLoopback() {
		t.Errorf("localIP = %v, want loopback", ip)
	}
	if ip := localIP(nil); ip != nil {
		t.Errorf("localIP(nil) = %v, want nil", ip)
	}
}
