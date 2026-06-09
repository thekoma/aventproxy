package rtsp

import (
	"net"
	"strconv"
	"syscall"
	"testing"
)

// listenOnEphemeral binds a TCP listener with reuseAddrControl on an OS-chosen
// port and returns the listener and its port.
func listenOnEphemeral(t *testing.T) (net.Listener, int) {
	t.Helper()
	lc := net.ListenConfig{Control: reuseAddrControl}
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port
}

func TestReuseAddrControlSetsSockopt(t *testing.T) {
	ln, _ := listenOnEphemeral(t)
	defer ln.Close()

	raw, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}

	var opt int
	var ctlErr error
	if err := raw.Control(func(fd uintptr) {
		opt, ctlErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR)
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if ctlErr != nil {
		t.Fatalf("getsockopt: %v", ctlErr)
	}
	if opt == 0 {
		t.Error("SO_REUSEADDR is not set on the listener")
	}
}

// TestReuseAddrAllowsImmediateRebind reproduces the issue #43 scenario: a port
// held by a connection lingering after the listener closed must still be
// bindable by a fresh listener.
func TestReuseAddrAllowsImmediateRebind(t *testing.T) {
	ln, port := listenOnEphemeral(t)

	// Establish a connection so the port has an accepted socket associated with
	// it, then close everything and immediately try to re-bind the same port.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	srvConn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	conn.Close()
	srvConn.Close()
	ln.Close()

	lc := net.ListenConfig{Control: reuseAddrControl}
	ln2, err := lc.Listen(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("re-bind on port %d failed: %v", port, err)
	}
	ln2.Close()
}
