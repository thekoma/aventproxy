package rtsp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestFrameClockSharesTimestampWithinFrame(t *testing.T) {
	var c frameClock

	first := c.timestamp(false, 90000)
	if c.timestamp(false, 90000) != first || c.timestamp(true, 90000) != first {
		t.Fatal("packets of one frame got different timestamps")
	}

	time.Sleep(20 * time.Millisecond)
	next := c.timestamp(true, 90000)
	if gap := int32(next - first); gap < 90000*15/1000 || gap > 90000*200/1000 {
		t.Fatalf("next frame %d ticks later, want about 20 ms (1800)", gap)
	}
}

func TestFrameClockClosesFrameWithoutMarker(t *testing.T) {
	var c frameClock

	first := c.timestamp(false, 90000)
	time.Sleep(maxFrameSpan + 10*time.Millisecond)
	if c.timestamp(false, 90000) == first {
		t.Fatal("frame whose marker was lost never closed")
	}
}

func TestFrameClockStaysIncreasingAcrossWraparound(t *testing.T) {
	c := frameClock{started: true, wallStart: time.Now(), frameTs: 0xFFFF_FFF0}

	if got := c.timestamp(true, 90000); int32(got-0xFFFF_FFF0) <= 0 {
		t.Fatalf("timestamp %d does not follow %d", got, uint32(0xFFFF_FFF0))
	}
}

func TestRtpTicksWrapsPast32Bits(t *testing.T) {
	// 14 h at 90 kHz is 4_536_000_000 ticks, past 2^32.
	if got, want := rtpTicks(14*time.Hour, 90000), uint32(4_536_000_000-(1<<32)); got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
	if got := rtpTicks(1500*time.Millisecond, 8000); got != 12000 {
		t.Fatalf("got %d, want 12000", got)
	}
}

func TestFrameClockKeepsAdvancingAfter32Bits(t *testing.T) {
	start := time.Now().Add(-14 * time.Hour)
	c := frameClock{started: true, wallStart: start, frameTs: rtpTicks(14*time.Hour-50*time.Millisecond, 90000)}

	got := c.timestamp(true, 90000)
	if gap := int32(got - rtpTicks(14*time.Hour-50*time.Millisecond, 90000)); gap < 90000*40/1000 || gap > 90000*500/1000 {
		t.Fatalf("frame after 14 h advanced %d ticks, want about 50 ms (4500)", gap)
	}
}

// readInterleaved collects the RTP packets a TCP client receives.
func readInterleaved(t *testing.T, conn net.Conn, n int) []*rtp.Packet {
	t.Helper()
	packets, err := readInterleavedErr(conn, n)
	if err != nil {
		t.Fatal(err)
	}
	return packets
}

// readInterleavedErr is readInterleaved for a goroutine other than the test's.
func readInterleavedErr(conn net.Conn, n int) ([]*rtp.Packet, error) {
	var packets []*rtp.Packet
	header := make([]byte, 4)
	for len(packets) < n {
		if _, err := io.ReadFull(conn, header); err != nil {
			return packets, fmt.Errorf("read header after %d packets: %w", len(packets), err)
		}
		body := make([]byte, binary.BigEndian.Uint16(header[2:4]))
		if _, err := io.ReadFull(conn, body); err != nil {
			return packets, fmt.Errorf("read body: %w", err)
		}
		p := &rtp.Packet{}
		if err := p.Unmarshal(body); err != nil {
			return packets, fmt.Errorf("unmarshal: %w", err)
		}
		packets = append(packets, p)
	}
	return packets, nil
}

// collect reads n interleaved packets in the background; wait returns them.
func collect(t *testing.T, conn net.Conn, n int) (wait func() []*rtp.Packet) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	type result struct {
		packets []*rtp.Packet
		err     error
	}
	done := make(chan result, 1)
	go func() {
		packets, err := readInterleavedErr(conn, n)
		done <- result{packets, err}
	}()
	return func() []*rtp.Packet {
		t.Helper()
		r := <-done
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.packets
	}
}

func TestForwardVideoInjectsParameterSetsInOrder(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	rf := NewRTPForwarder()
	if err := rf.AddTCPClient("s1", server, 0, 2, 4); err != nil {
		t.Fatalf("add client: %v", err)
	}

	send := []*rtp.Packet{
		{Header: rtp.Header{SequenceNumber: 10}, Payload: []byte{0x67, 0x42}},               // SPS
		{Header: rtp.Header{SequenceNumber: 11}, Payload: []byte{0x68, 0xce}},               // PPS
		{Header: rtp.Header{SequenceNumber: 12, Marker: true}, Payload: []byte{0x65, 0x88}}, // IDR
		{Header: rtp.Header{SequenceNumber: 13}, Payload: []byte{0x7c, 0x81}},               // FU-A start of a P-frame
		{Header: rtp.Header{SequenceNumber: 14, Marker: true}, Payload: []byte{0x7c, 0x45}},
	}

	wait := collect(t, client, 7)
	for i, p := range send {
		if i == 3 {
			time.Sleep(20 * time.Millisecond)
		}
		rf.ForwardVideoPacket(p)
	}
	got := wait()

	wantSeq := []uint16{10, 11, 12, 13, 14, 15, 16}
	for i, p := range got {
		if p.SequenceNumber != wantSeq[i] {
			t.Errorf("packet %d: seq %d, want %d", i, p.SequenceNumber, wantSeq[i])
		}
	}
	// Packets 0-4 are one access unit (SPS, PPS, injected SPS/PPS, IDR), 5-6 the next frame.
	for i := 1; i <= 4; i++ {
		if got[i].Timestamp != got[0].Timestamp {
			t.Errorf("packet %d: ts %d, want %d like the rest of its frame", i, got[i].Timestamp, got[0].Timestamp)
		}
	}
	if got[5].Timestamp != got[6].Timestamp || int32(got[5].Timestamp-got[0].Timestamp) <= 0 {
		t.Errorf("second frame ts %d/%d, want equal and after %d", got[5].Timestamp, got[6].Timestamp, got[0].Timestamp)
	}
	for _, i := range []int{2, 3} {
		if got[i].Marker {
			t.Errorf("injected packet %d has the marker bit set", i)
		}
	}
}

// UDP media goes where the client's RTSP connection comes from. It used to go
// to localhost, so any client on another host received nothing (fork c30da50).
// controlConn returns both ends of an RTSP control connection whose client
// end is bound to ip.
func controlConn(t *testing.T, ip net.IP) (server, client net.Conn) {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: ip})
	if err != nil {
		t.Skipf("cannot listen on %v: %v", ip, err)
	}
	defer ln.Close()
	client, err = (&net.Dialer{LocalAddr: &net.TCPAddr{IP: ip}}).Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err = ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return server, client
}

func TestUDPMediaGoesToTheClientAddress(t *testing.T) {
	ip := firstNonLoopbackIPv4()
	if ip == nil {
		t.Skip("no non-loopback IPv4 address to stand in for a remote client")
	}
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		t.Skipf("cannot listen on %v: %v", ip, err)
	}
	defer sink.Close()
	control, _ := controlConn(t, ip)

	rf := NewRTPForwarder()
	if err := rf.AddUDPClient("s1", control, sink.LocalAddr().(*net.UDPAddr).Port, 0); err != nil {
		t.Fatalf("add client: %v", err)
	}
	rf.Play("s1")
	defer rf.RemoveClient("s1")
	rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: 1, Marker: true}, Payload: []byte{0x65, 0x88}})

	_ = sink.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := sink.ReadFromUDP(make([]byte, 1500)); err != nil {
		t.Fatalf("the client at %v received nothing: %v", ip, err)
	}
}

func TestUDPClientNeedsAnAddress(t *testing.T) {
	a, b := net.Pipe() // no IP address behind it
	defer a.Close()
	defer b.Close()
	if err := NewRTPForwarder().AddUDPClient("s1", a, 5000, 5002); err == nil {
		t.Fatal("a UDP client with no address must be refused, not sent to localhost")
	}
}

func TestRemoteIP(t *testing.T) {
	ln, _ := listenOnEphemeral(t)
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if ip := remoteIP(c); ip == nil || !ip.IsLoopback() {
		t.Errorf("remoteIP = %v, want loopback", ip)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if ip := remoteIP(a); ip != nil {
		t.Errorf("remoteIP(pipe) = %v, want nil", ip)
	}
}

// refusedUDPPort is a loopback port nobody listens on any more.
func refusedUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	return port
}

func waitClientCount(t *testing.T, rf *RTPForwarder, want int, send func()) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for rf.GetClientCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("forwarder still has %d client(s), want %d", rf.GetClientCount(), want)
		}
		send()
		time.Sleep(5 * time.Millisecond)
	}
}

// A UDP client that closed its socket answers every packet with ICMP port
// unreachable, which a connected socket reports as ECONNREFUSED: the client
// is gone, on the audio track as much as on the video one (fork 7314de5).
// Its RTSP connection is closed too, as for a dead TCP client, so the stream
// does not stay up for a session nobody watches and a live client reconnects.
func TestRefusedUDPClientIsDropped(t *testing.T) {
	loopback := net.IPv4(127, 0, 0, 1)
	for _, track := range []string{"video", "audio"} {
		t.Run(track, func(t *testing.T) {
			control, peer := controlConn(t, loopback)
			rf := NewRTPForwarder()
			video, audio := refusedUDPPort(t), 0
			var seq uint16 // a repeated sequence number would be dropped as a duplicate
			send := func() {
				seq++
				rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Marker: true}, Payload: []byte{0x41, 0x9a}})
			}
			if track == "audio" {
				video, audio = 0, refusedUDPPort(t)
				send = func() { rf.ForwardAudioPacket(&rtp.Packet{Payload: make([]byte, 160)}) }
			}
			if err := rf.AddUDPClient("s1", control, video, audio); err != nil {
				t.Fatal(err)
			}
			rf.Play("s1")
			waitClientCount(t, rf, 0, send)
			expectClosed(t, peer)
		})
	}
}

// A TCP client that stops reading must not stall every other viewer: the
// write gives up, the client is dropped and its connection closed.
func TestStalledTCPClientIsDropped(t *testing.T) {
	old := tcpWriteTimeout
	tcpWriteTimeout = 50 * time.Millisecond
	defer func() { tcpWriteTimeout = old }()

	server, client := net.Pipe() // nobody reads client: every write blocks
	defer client.Close()
	rf := NewRTPForwarder()
	if err := rf.AddTCPClient("s1", server, 0, 2, 4); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{Marker: true}, Payload: []byte{0x41, 0x9a}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("forwarding still blocked on a client that does not read")
	}
	if n := rf.GetClientCount(); n != 0 {
		t.Fatalf("stalled client still attached (%d clients)", n)
	}
	if _, err := server.Write([]byte{0}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("the stalled client's connection was left open: %v", err)
	}
}

// Audio timestamps count samples: they say when the audio was recorded, not
// when its packet happened to arrive (fork 00d7e2f). Stamped from the wall
// clock, a burst of packets got nearly one timestamp and a consumer that
// honours timestamps played them over each other.
func TestForwardAudioStampsBySamples(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	rf := NewRTPForwarder()
	if err := rf.AddTCPClient("s1", server, 0, 2, 4); err != nil {
		t.Fatal(err)
	}

	wait := collect(t, client, 3)
	for range 3 { // a burst: 60 ms of audio delivered at once
		rf.ForwardAudioPacket(&rtp.Packet{Header: rtp.Header{PayloadType: 0}, Payload: make([]byte, 160)})
	}
	got := wait()
	for i := 1; i < len(got); i++ {
		if d := got[i].Timestamp - got[i-1].Timestamp; d != 160 {
			t.Errorf("packet %d is %d ticks after the previous one, want 160 (its samples)", i, d)
		}
	}
}

func TestAudioClockCatchesUpAfterSilence(t *testing.T) {
	c := audioClock{started: true, wallStart: time.Now().Add(-3 * time.Second), next: 160}
	got := c.timestamp(160, 8000)
	if got < 8000*29/10 || got > 8000*35/10 {
		t.Fatalf("after 3 s of silence the clock reads %d, want about 24000", got)
	}
	if next := c.timestamp(160, 8000); next != got+160 {
		t.Fatalf("next packet at %d, want %d", next, got+160)
	}
}

// Parameter sets that arrive before anyone watches are kept, so the first
// viewer still gets them ahead of the next keyframe.
func TestParameterSetsCachedWithoutClients(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	rf := NewRTPForwarder()
	rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: 1}, Payload: []byte{0x67, 0x42}}) // SPS
	rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: 2}, Payload: []byte{0x68, 0xce}}) // PPS

	if err := rf.AddTCPClient("s1", server, 0, 2, 4); err != nil {
		t.Fatal(err)
	}
	wait := collect(t, client, 3)
	rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: 3, Marker: true}, Payload: []byte{0x65, 0x88}}) // IDR
	got := wait()
	for i, want := range []byte{7, 8, 5} {
		if got[i].Payload[0]&0x1f != want {
			t.Errorf("packet %d is NAL type %d, want %d", i, got[i].Payload[0]&0x1f, want)
		}
	}
}

// flakyConn fails its first `fail` writes with an error that is not a dead
// client's, like EHOSTUNREACH from a phone that left the Wi-Fi for a moment.
type flakyConn struct {
	net.Conn
	fail int
}

func (c *flakyConn) Write(b []byte) (int, error) {
	if c.fail > 0 {
		c.fail--
		return 0, syscall.EHOSTUNREACH
	}
	return len(b), nil
}

func (c *flakyConn) SetWriteDeadline(time.Time) error { return nil }

// A client that cannot be reached for a while is not dropped, but it must not
// log an ERROR for every packet either (100-150 a second): once when it
// starts failing, once when it recovers.
func TestUnreachableClientLogsOnce(t *testing.T) {
	session := fmt.Sprintf("flaky-%d", time.Now().UnixNano())
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	rf := NewRTPForwarder()
	if err := rf.AddTCPClient(session, &flakyConn{Conn: a, fail: 50}, 0, 2, 4); err != nil {
		t.Fatal(err)
	}
	for range 60 {
		rf.ForwardAudioPacket(&rtp.Packet{Header: rtp.Header{PayloadType: 0}, Payload: make([]byte, 160)})
	}
	if n := rf.GetClientCount(); n != 1 {
		t.Fatalf("an unreachable client was dropped (%d clients)", n)
	}
	if lines := testLog.lines(session); len(lines) != 2 {
		t.Fatalf("want one line when %s starts failing and one when it recovers, got %d: %q", session, len(lines), lines)
	}
}

// A monitor whose sample clock runs fast (crystal drift, here 500 ppm, ten
// times a typical crystal) would put audio ahead of the wall-clock video for
// good on an always-on consumer, since audio that flows never pauses for a
// re-anchor. The clock steers back one tick per packet once it is off by more
// than audioSlewFrom, so the offset stays bounded and timestamps never go back.
func TestAudioClockStaysNearTheWallClock(t *testing.T) {
	for _, tc := range []struct {
		name string
		ppm  float64 // how much faster than the host the monitor samples
	}{{"fast monitor", 500}, {"slow monitor", -500}} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			c := audioClock{}
			period := time.Duration(float64(20*time.Millisecond) / (1 + tc.ppm/1e6)) // 160 samples
			var last uint32
			limit := int32(rtpTicks(audioSlewFrom, 8000)) + 2*160
			for i := range 200_000 { // a bit over an hour of audio
				now := start.Add(time.Duration(i) * period)
				ts := c.stamp(now, 160, 8000)
				if i > 0 && int32(ts-last) <= 0 {
					t.Fatalf("packet %d: timestamp %d does not follow %d", i, ts, last)
				}
				last = ts
				if off := int32(ts - rtpTicks(now.Sub(start), 8000)); off > limit || off < -limit {
					t.Fatalf("packet %d: audio %d ticks off the wall clock, want within %d", i, off, limit)
				}
			}
		})
	}
}

// A UDP client that set up only video has no audio socket. Its audio
// packets are not sends, so they must not count as the client recovering:
// an unreachable video-only client alternated a WARN and an INFO per packet.
func TestFailingClientRecoversOnlyOnARealSend(t *testing.T) {
	session := fmt.Sprintf("video-only-%d", time.Now().UnixNano())
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	video, err := net.DialUDP("udp", nil, sink.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	rf := NewRTPForwarder()
	rf.clients[session] = &RTPClient{
		sessionID: session, transportMode: TransportUDP, playing: true,
		videoConn: video,
		failing:   true, // its last video send failed
	}
	defer rf.RemoveClient(session)

	for range 20 {
		rf.ForwardAudioPacket(&rtp.Packet{Header: rtp.Header{PayloadType: 0}, Payload: make([]byte, 160)})
	}
	if lines := testLog.lines("Sending to client " + session + " again"); len(lines) != 0 {
		t.Fatalf("audio it has no socket for counted as a recovery %d times", len(lines))
	}

	rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{Marker: true}, Payload: []byte{0x41, 0x9a}})
	if lines := testLog.lines("Sending to client " + session + " again"); len(lines) != 1 {
		t.Fatalf("a video send that worked logged %d recoveries, want 1", len(lines))
	}
}
