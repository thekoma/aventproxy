package rtsp

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"

	"github.com/pion/rtp"
)

// aacServer is a real RTSP server on loopback with one camera, /aaccam, whose
// stream starts through a fake cloud attempt; the test feeds its forwarder.
func aacServer(t *testing.T, ffmpeg string) (*RTSPServer, int, func() *CameraStream) {
	t.Helper()
	return aacServerAt(t, ffmpeg, "/aaccam")
}

// aacServerAt is aacServer with the camera on another path.
func aacServerAt(t *testing.T, ffmpeg, path string) (*RTSPServer, int, func() *CameraStream) {
	t.Helper()
	t.Chdir(t.TempDir())
	sm, err := storage.NewStorageManager()
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.SaveUser("eu", "viewer@example.com", nil); err != nil {
		t.Fatal(err)
	}
	users, _ := sm.ListUsers()
	key := users[0].UserKey
	if err := sm.UpdateCamerasForUser(key, []storage.CameraInfo{{UserKey: key, DeviceID: "aac1", DeviceName: "AAC Cam", RTSPPath: path}}); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	s := NewRTSPServer(port, sm)
	s.BindAddress = "127.0.0.1"
	s.AACFFmpeg = ffmpeg
	var mu sync.Mutex
	var stream *CameraStream
	s.newStream = func(cs *CameraStream) {
		mu.Lock()
		stream = cs
		mu.Unlock()
		cs.cloudStart = func(context.Context, *WebRTCBridge) error { return nil }
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s, port, func() *CameraStream { mu.Lock(); defer mu.Unlock(); return stream }
}

// rtspConn is a minimal RTSP client over one TCP connection.
type rtspConn struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	cseq int
}

func (c *rtspConn) request(method, url string, headers ...string) (map[string]string, string) {
	c.t.Helper()
	c.cseq++
	req := fmt.Sprintf("%s %s RTSP/1.0\r\nCSeq: %d\r\n", method, url, c.cseq)
	for _, h := range headers {
		req += h + "\r\n"
	}
	if _, err := c.conn.Write([]byte(req + "\r\n")); err != nil {
		c.t.Fatal(err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	status, err := c.r.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		c.t.Fatalf("%s %s: %q %v", method, url, status, err)
	}
	got := map[string]string{}
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatal(err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			got[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	body := ""
	if n, _ := strconv.Atoi(got["content-length"]); n > 0 {
		b := make([]byte, n)
		if _, err := io.ReadFull(c.r, b); err != nil {
			c.t.Fatal(err)
		}
		body = string(b)
	}
	return got, body
}

// interleaved reads RTP packets per channel until the deadline.
func (c *rtspConn) interleaved(d time.Duration) map[byte][]rtp.Packet {
	out := map[byte][]rtp.Packet{}
	_ = c.conn.SetReadDeadline(time.Now().Add(d))
	for {
		b, err := c.r.ReadByte()
		if err != nil {
			return out
		}
		if b != '$' {
			continue
		}
		var hdr [3]byte
		if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
			return out
		}
		data := make([]byte, binary.BigEndian.Uint16(hdr[1:]))
		if _, err := io.ReadFull(c.r, data); err != nil {
			return out
		}
		var p rtp.Packet
		if p.Unmarshal(data) == nil {
			out[hdr[0]] = append(out[hdr[0]], p)
		}
	}
}

// A client that takes only the AAC track (on channel 0) of a stream whose
// URL has a query gets AAC there, never H.264 or G.711, for μ-law and A-law
// cameras; the encoder stops with the stream.
func TestAACTrackEndToEnd(t *testing.T) {
	ffmpeg := FindFFmpeg()
	if ffmpeg == "" {
		t.Skip("no ffmpeg with an AAC encoder")
	}
	for _, law := range []struct {
		name string
		pt   uint8
	}{{"mulaw", 0}, {"alaw", 8}} {
		t.Run(law.name, func(t *testing.T) {
			_, port, stream := aacServer(t, ffmpeg)
			conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			c := &rtspConn{t: t, conn: conn, r: bufio.NewReader(conn)}
			base := fmt.Sprintf("rtsp://127.0.0.1:%d/aaccam?client=test", port)
			_, sdp := c.request("DESCRIBE", base, "Accept: application/sdp")
			m := regexp.MustCompile(`a=control:(\S*/aac)\r\n`).FindStringSubmatch(sdp)
			if m == nil || strings.Contains(m[1], "?") {
				t.Fatalf("AAC control in %q", sdp)
			}
			if aacAt, g711At := strings.Index(sdp, "MPEG4-GENERIC"), strings.Index(sdp, "PCMU"); aacAt < 0 || g711At >= 0 && aacAt > g711At {
				t.Fatalf("AAC not the first audio track: %q", sdp)
			}
			h, _ := c.request("SETUP", m[1], "Transport: RTP/AVP/TCP;unicast;interleaved=0-1")
			session, _, _ := strings.Cut(h["session"], ";")
			c.request("PLAY", base, "Session: "+session)

			cs := stream()
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				payload := make([]byte, 160)
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					case <-time.After(20 * time.Millisecond):
					}
					cs.forwarder.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(i), Marker: true}, Payload: []byte{0x65, 0x88, 0x84, 0x00}})
					cs.forwarder.ForwardAudioPacket(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: law.pt, SequenceNumber: uint16(i)}, Payload: payload})
				}
			}()
			got := c.interleaved(2 * time.Second)
			close(stop)
			<-done

			var aac, other int
			for _, p := range got[0] {
				if p.PayloadType == 97 {
					aac++
				} else {
					other++
				}
			}
			if aac < 5 || other != 0 {
				t.Fatalf("channel 0: %d AAC packets, %d others; want AAC only", aac, other)
			}
			for ch, ps := range got {
				if ch != 0 && len(ps) > 0 {
					t.Fatalf("%d packets on channel %d, which the client did not set up", len(ps), ch)
				}
			}
			cs.forwarder.mutex.RLock()
			alaw := cs.forwarder.aac != nil && cs.forwarder.aac.cmd != nil && strings.Contains(strings.Join(cs.forwarder.aac.cmd.Args, " "), "-f alaw")
			cs.forwarder.mutex.RUnlock()
			if alaw != (law.pt == 8) {
				t.Fatalf("encoder reads A-law: %v, want %v", alaw, law.pt == 8)
			}
			cs.forwarder.Stop()
			cs.forwarder.mutex.RLock()
			running := cs.forwarder.aac != nil
			cs.forwarder.mutex.RUnlock()
			if running {
				t.Fatal("the encoder outlived the forwarder's Stop")
			}
		})
	}
}

// Without an ffmpeg the server offers no AAC track and a SETUP of /aac takes
// nothing.
func TestNoAACTrackWithoutFFmpeg(t *testing.T) {
	_, port, _ := aacServer(t, "")
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &rtspConn{t: t, conn: conn, r: bufio.NewReader(conn)}
	_, sdp := c.request("DESCRIBE", fmt.Sprintf("rtsp://127.0.0.1:%d/aaccam", port), "Accept: application/sdp")
	if strings.Contains(sdp, "MPEG4-GENERIC") || strings.Contains(sdp, "/aac\r\n") {
		t.Fatalf("AAC offered without ffmpeg: %q", sdp)
	}
}

// dial connects a raw RTSP client to the test server.
func dial(t *testing.T, port int) *rtspConn {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &rtspConn{t: t, conn: conn, r: bufio.NewReader(conn)}
}

// feed sends video and G.711 into a stream's forwarder until stop closes.
func feed(cs *CameraStream, pt uint8, stop chan struct{}) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		payload := make([]byte, 160)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			cs.forwarder.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(i), Marker: true}, Payload: []byte{0x65, 0x88, 0x84, 0x00}})
			cs.forwarder.ForwardAudioPacket(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: uint16(i)}, Payload: payload})
		}
	}()
	return done
}

// A camera whose path holds "/video" still gets its tracks by the last
// segment: a SETUP of its G.711 track takes G.711, not the video.
func TestSetupRoutesByTheLastSegment(t *testing.T) {
	_, port, stream := aacServerAt(t, "", "/videoroom")
	c := dial(t, port)
	base := fmt.Sprintf("rtsp://127.0.0.1:%d/videoroom", port)
	_, sdp := c.request("DESCRIBE", base, "Accept: application/sdp")
	if !strings.Contains(sdp, base+"/audio\r\n") {
		t.Fatalf("no audio control in %q", sdp)
	}
	h, _ := c.request("SETUP", base+"/audio", "Transport: RTP/AVP/TCP;unicast;interleaved=2-3")
	session, _, _ := strings.Cut(h["session"], ";")
	c.request("PLAY", base, "Session: "+session)
	stop := make(chan struct{})
	done := feed(stream(), 0, stop)
	got := c.interleaved(time.Second)
	close(stop)
	<-done
	var g711, other int
	for ch, ps := range got {
		for _, p := range ps {
			if ch == 2 && p.PayloadType == 0 {
				g711++
			} else {
				other++
			}
		}
	}
	if g711 < 5 || other != 0 {
		t.Fatalf("%d G.711 packets on channel 2, %d others; want G.711 only", g711, other)
	}
}

// A UDP client that sets up only the AAC track gets AAC at its port.
func TestAACTrackOverUDP(t *testing.T) {
	ffmpeg := FindFFmpeg()
	if ffmpeg == "" {
		t.Skip("no ffmpeg with an AAC encoder")
	}
	_, port, stream := aacServer(t, ffmpeg)
	c := dial(t, port)
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	rtpPort := udp.LocalAddr().(*net.UDPAddr).Port
	base := fmt.Sprintf("rtsp://127.0.0.1:%d/aaccam", port)
	c.request("DESCRIBE", base, "Accept: application/sdp")
	h, _ := c.request("SETUP", base+"/aac", fmt.Sprintf("Transport: RTP/AVP;unicast;client_port=%d-%d", rtpPort, rtpPort+1))
	session, _, _ := strings.Cut(h["session"], ";")
	c.request("PLAY", base, "Session: "+session)
	stop := make(chan struct{})
	done := feed(stream(), 0, stop)
	defer func() { close(stop); <-done }()
	aac := 0
	buf := make([]byte, 2048)
	_ = udp.SetReadDeadline(time.Now().Add(2 * time.Second))
	for aac < 5 {
		n, err := udp.Read(buf)
		if err != nil {
			break
		}
		var p rtp.Packet
		if p.Unmarshal(buf[:n]) == nil && p.PayloadType == 97 {
			aac++
		} else {
			t.Fatalf("a non-AAC packet on the AAC port: %+v", p.Header)
		}
	}
	if aac < 5 {
		t.Fatalf("%d AAC packets over UDP, want at least 5", aac)
	}
}

// A client dropped as inactive takes the encoder with it.
func TestCleanupInactiveClientsStopsTheEncoder(t *testing.T) {
	ffmpeg := FindFFmpeg()
	if ffmpeg == "" {
		t.Skip("no ffmpeg with an AAC encoder")
	}
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	newDrain(peer)
	rf := NewRTPForwarder()
	rf.AACFFmpeg = ffmpeg
	_ = rf.AddTCPClient("s1", conn, 0, 2, 0)
	_ = rf.SetupAACTrack("s1", 6, 0)
	deadline := time.Now().Add(3 * time.Second)
	for {
		rf.ForwardAudioPacket(g711Silence(1))
		rf.mutex.RLock()
		running := rf.aac != nil
		rf.mutex.RUnlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("encoder never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	rf.CleanupInactiveClients(-time.Second)
	rf.mutex.RLock()
	running := rf.aac != nil
	rf.mutex.RUnlock()
	if running {
		t.Fatal("the encoder outlived its last client")
	}
}

// A camera whose path holds "aac" keeps its tracks: a SETUP of its G.711
// track takes G.711, not the AAC copy.
func TestCameraNamedAACKeepsItsTracks(t *testing.T) {
	ffmpeg := FindFFmpeg()
	if ffmpeg == "" {
		t.Skip("no ffmpeg with an AAC encoder")
	}
	_, port, stream := aacServerAt(t, ffmpeg, "/aacroom")
	c := dial(t, port)
	base := fmt.Sprintf("rtsp://127.0.0.1:%d/aacroom", port)
	c.request("DESCRIBE", base, "Accept: application/sdp")
	h, _ := c.request("SETUP", base+"/audio", "Transport: RTP/AVP/TCP;unicast;interleaved=2-3")
	session, _, _ := strings.Cut(h["session"], ";")
	c.request("PLAY", base, "Session: "+session)
	stop := make(chan struct{})
	done := feed(stream(), 0, stop)
	got := c.interleaved(time.Second)
	close(stop)
	<-done
	var g711, other int
	for ch, ps := range got {
		for _, p := range ps {
			if ch == 2 && p.PayloadType == 0 {
				g711++
			} else {
				other++
			}
		}
	}
	if g711 < 5 || other != 0 {
		t.Fatalf("%d G.711 packets on channel 2, %d others; want G.711 only", g711, other)
	}
}
