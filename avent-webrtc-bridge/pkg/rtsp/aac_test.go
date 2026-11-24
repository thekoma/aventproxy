package rtsp

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"

	"github.com/pion/rtp"
)

// adtsFrame builds one ADTS frame (no CRC) around payload.
func adtsFrame(payload []byte) []byte {
	n := len(payload) + 7
	return append([]byte{
		0xFF, 0xF1, // sync, MPEG-4, layer 0, no CRC
		0x60,                    // AAC-LC, 16 kHz, private 0, channel config high bit 0
		0x40 | byte(n>>11)&0x03, // channel config 1, frame length bits 12-11
		byte(n >> 3),
		byte(n&0x07)<<5 | 0x1F,
		0xFC,
	}, payload...)
}

func TestReadADTSSplitsFrames(t *testing.T) {
	stream := append([]byte{0x00, 0x12}, adtsFrame([]byte("first"))...)
	stream = append(stream, adtsFrame(bytes.Repeat([]byte{0xAB}, 300))...)
	var got [][]byte
	readADTS(bytes.NewReader(stream), func(f []byte) { got = append(got, f) })
	if len(got) != 2 || string(got[0]) != "first" || len(got[1]) != 300 {
		t.Fatalf("frames %q", got)
	}
}

func TestAACPacketCarriesOneAUHeader(t *testing.T) {
	frame := bytes.Repeat([]byte{1}, 300)
	p := aacPacket(frame, 7, 1024, aacSSRC)
	if p.PayloadType != 97 || !p.Marker || p.SequenceNumber != 7 || p.Timestamp != 1024 {
		t.Fatalf("header %+v", p.Header)
	}
	if binary.BigEndian.Uint16(p.Payload[0:2]) != 16 {
		t.Fatalf("AU-headers-length %d, want 16 bits", binary.BigEndian.Uint16(p.Payload[0:2]))
	}
	au := binary.BigEndian.Uint16(p.Payload[2:4])
	if au>>3 != 300 || au&0x07 != 0 {
		t.Fatalf("AU header size %d index %d", au>>3, au&0x07)
	}
	if !bytes.Equal(p.Payload[4:], frame) {
		t.Fatal("frame not copied after the header")
	}
}

func TestAACClockCountsFramesAndCatchesUpAfterAPause(t *testing.T) {
	var c aacClock
	start := time.Unix(1000, 0)
	if c.next(start) != 0 {
		t.Fatal("first frame not at 0")
	}
	// Frames arriving faster than real time keep their 1024-tick spacing.
	if c.next(start.Add(10*time.Millisecond)) != 1024 || c.next(start.Add(20*time.Millisecond)) != 2048 {
		t.Fatal("frames not 1024 ticks apart")
	}
	// After 2 s without audio the next frame is stamped at the wall clock.
	if ts := c.next(start.Add(2 * time.Second)); ts != 32000 {
		t.Fatalf("after a pause ts %d, want 32000", ts)
	}
}

func TestSDPPutsAACBeforeG711(t *testing.T) {
	s := &RTSPServer{AACFFmpeg: "/usr/bin/ffmpeg"}
	sdp := s.generateSDP(&storage.CameraInfo{}, "rtsp://h/cam")
	aac := strings.Index(sdp, "MPEG4-GENERIC/16000/1")
	pcmu := strings.Index(sdp, "PCMU/8000")
	if aac < 0 || pcmu < 0 || aac > pcmu {
		t.Fatalf("AAC must come before G.711:\n%s", sdp)
	}
	if !strings.Contains(sdp, "a=control:rtsp://h/cam/aac\r\n") || !strings.Contains(sdp, "config=1408") {
		t.Fatalf("AAC track incomplete:\n%s", sdp)
	}

	s.AACFFmpeg = ""
	if sdp := s.generateSDP(&storage.CameraInfo{}, "rtsp://h/cam"); strings.Contains(sdp, "MPEG4-GENERIC") {
		t.Fatalf("AAC offered without ffmpeg:\n%s", sdp)
	}
}

// The first AAC packet is the silent frame, right after the first video
// packet, before the encoder has produced anything.
func TestSilentAACFrameFollowsTheFirstVideo(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	d := newDrain(peer)

	rf := NewRTPForwarder()
	rf.AACFFmpeg = "/nonexistent/ffmpeg" // never started: no G.711 is sent
	if err := rf.AddTCPClient("s1", conn, 0, 2, 0); err != nil {
		t.Fatal(err)
	}
	if err := rf.SetupAACTrack("s1", 6, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Marker: true}, Payload: []byte{0x41, 0x9a}})
	}
	deadline := time.Now().Add(time.Second)
	for (d.count(0) < 3 || d.count(6) < 1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.packets[6]) != 1 {
		t.Fatalf("%d AAC packets, want exactly one silent frame", len(d.packets[6]))
	}
	if got := d.packets[6][0].Payload[4:]; !bytes.Equal(got, aacSilentFrame) {
		t.Fatalf("first AAC frame %x, want the silent frame", got)
	}
}

func TestAACClockResyncsAfterTheSilentFrame(t *testing.T) {
	var c aacClock
	start := time.Unix(1000, 0)
	c.next(start)
	c.resync = true
	// The encoder's first frame 300 ms later is stamped at the wall clock,
	// not 1024 ticks after the silent frame.
	if ts := c.next(start.Add(300 * time.Millisecond)); ts != 4800 {
		t.Fatalf("ts %d, want 4800", ts)
	}
	if ts := c.next(start.Add(310 * time.Millisecond)); ts != 4800+1024 {
		t.Fatalf("ts %d, want counting on", ts)
	}
}

// Only a URL ending in /aac is the AAC track, so a camera whose name starts
// with "aac" keeps its video and G.711 tracks.
func TestAACTrackIsTheLastSegmentOnly(t *testing.T) {
	for url, want := range map[string]string{
		"rtsp://h:8554/Cam/hd/aac":        "aac",
		"rtsp://h:8554/aacroom/video":     "video",
		"rtsp://h:8554/aacroom/aac/":      "aac",
		"rtsp://h:8554/aacroom":           "aacroom",
		"rtsp://u:p@h:8554/Cam/audio?x=1": "audio",
	} {
		if got := lastPathSegment(url); got != want {
			t.Errorf("%s: %q, want %q", url, got, want)
		}
	}
}

// drain records the interleaved RTP packets arriving on conn, per channel.
type drain struct {
	mu      sync.Mutex
	packets map[byte][]*rtp.Packet
}

func newDrain(conn net.Conn) *drain {
	d := &drain{packets: map[byte][]*rtp.Packet{}}
	go func() {
		head := make([]byte, 4)
		for {
			if _, err := io.ReadFull(conn, head); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint16(head[2:4]))
			if _, err := io.ReadFull(conn, body); err != nil {
				return
			}
			p := &rtp.Packet{}
			if p.Unmarshal(body) == nil {
				d.mu.Lock()
				d.packets[head[1]] = append(d.packets[head[1]], p)
				d.mu.Unlock()
			}
		}
	}()
	return d
}

func (d *drain) count(channel byte) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.packets[channel])
}

func g711Silence(seq uint16) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: seq}, Payload: bytes.Repeat([]byte{0xFF}, 160)}
}

// A client that set up video and AAC but not G.711 has audio channel 0, the
// video's: G.711 must not land there.
func TestG711SkipsAClientWithoutTheTrack(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	d := newDrain(peer)

	rf := NewRTPForwarder()
	if err := rf.AddTCPClientTracks("s1", conn, 0, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		rf.ForwardAudioPacket(g711Silence(uint16(i)))
	}
	time.Sleep(50 * time.Millisecond)
	if n := d.count(0); n != 0 {
		t.Fatalf("%d G.711 packets on the video channel", n)
	}
}

func TestAACReachesOnlyClientsWithTheTrack(t *testing.T) {
	ffmpeg := FindFFmpeg()
	if ffmpeg == "" {
		t.Skip("no ffmpeg with an AAC encoder")
	}
	withConn, withPeer := net.Pipe()
	withoutConn, withoutPeer := net.Pipe()
	defer withConn.Close()
	defer withPeer.Close()
	defer withoutConn.Close()
	defer withoutPeer.Close()
	with, without := newDrain(withPeer), newDrain(withoutPeer)

	rf := NewRTPForwarder()
	rf.AACFFmpeg = ffmpeg
	if err := rf.AddTCPClient("with", withConn, 0, 2, 0); err != nil {
		t.Fatal(err)
	}
	if err := rf.SetupAACTrack("with", 6, 0); err != nil {
		t.Fatal(err)
	}
	if err := rf.AddTCPClient("without", withoutConn, 0, 2, 0); err != nil {
		t.Fatal(err)
	}

	// Two seconds of G.711 silence, in real time. Home Assistant drops an
	// audio track without a packet in its first 20 (about a second of
	// video), so the first AAC must come well within that.
	start := time.Now()
	var first time.Duration
	for i := 0; i < 100; i++ {
		rf.ForwardAudioPacket(g711Silence(uint16(i)))
		time.Sleep(20 * time.Millisecond)
		if first == 0 && with.count(6) > 0 {
			first = time.Since(start)
		}
	}
	if first == 0 || first > 600*time.Millisecond {
		t.Fatalf("first AAC packet after %v, want under 600 ms", first)
	}
	deadline := time.Now().Add(5 * time.Second)
	for with.count(6) < 10 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := with.count(6); n < 10 {
		t.Fatalf("%d AAC packets, want at least 10 for 2 s of audio", n)
	}
	with.mu.Lock()
	pkt := with.packets[6][0]
	with.mu.Unlock()
	if pkt.PayloadType != 97 || len(pkt.Payload) < 5 {
		t.Fatalf("not an AAC packet: %+v", pkt.Header)
	}
	if without.count(6) != 0 {
		t.Fatal("AAC sent to a client without the track")
	}
	if with.count(2) == 0 || without.count(2) == 0 {
		t.Fatal("G.711 no longer reaches its clients")
	}

	// The encoder stops with the last client that has the track.
	rf.RemoveClient("with")
	rf.mutex.RLock()
	running := rf.aac != nil
	rf.mutex.RUnlock()
	if running {
		t.Fatal("encoder still running without an AAC client")
	}
	rf.Stop()
}

// A client that set up only the AAC track, on channel 0, gets no H.264
// there: its video channel is 0 too, unset.
func TestVideoSkipsAClientWithoutTheTrack(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	d := newDrain(peer)

	rf := NewRTPForwarder()
	if err := rf.AddTCPClientTracks("s1", conn, 0, 0, 0, false, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(i), Marker: true}, Payload: []byte{0x65, 0x88, 0x84, 0x00}})
	}
	time.Sleep(50 * time.Millisecond)
	if n := d.count(0); n != 0 {
		t.Fatalf("%d video packets to a client without the video track", n)
	}
}

// AddTCPClient (library callers, older tests) sends both tracks.
func TestTracksDefaultToBoth(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	d := newDrain(peer)
	rf := NewRTPForwarder()
	if err := rf.AddTCPClient("s1", conn, 0, 2, 0); err != nil {
		t.Fatal(err)
	}
	rf.ForwardAudioPacket(g711Silence(1))
	time.Sleep(50 * time.Millisecond)
	if d.count(2) == 0 {
		t.Fatal("G.711 not sent by default")
	}
}

func TestFFmpegArgsFollowTheG711Law(t *testing.T) {
	has := func(args []string, s string) bool {
		for i := range args {
			if args[i] == "-f" && i+1 < len(args) && args[i+1] == s {
				return true
			}
		}
		return false
	}
	if a := ffmpegAACArgs(false); !has(a, "mulaw") || has(a, "alaw") {
		t.Fatalf("mu-law args %v", a)
	}
	if a := ffmpegAACArgs(true); !has(a, "alaw") || has(a, "mulaw") {
		t.Fatalf("A-law args %v", a)
	}
}

func TestAACRetryBacksOff(t *testing.T) {
	for failures, want := range map[int]time.Duration{1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second, 7: 5 * time.Minute, 20: 5 * time.Minute} {
		if got := aacRetryAfter(failures); got != want {
			t.Errorf("aacRetryAfter(%d) = %v, want %v", failures, got, want)
		}
	}
	// An ffmpeg that cannot run: each try waits longer.
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	newDrain(peer)
	rf := NewRTPForwarder()
	rf.AACFFmpeg = "/nonexistent/ffmpeg"
	_ = rf.AddTCPClient("s1", conn, 0, 2, 0)
	_ = rf.SetupAACTrack("s1", 6, 0)
	for try := 1; try <= 2; try++ {
		rf.ForwardAudioPacket(g711Silence(uint16(try)))
		deadline := time.Now().Add(2 * time.Second)
		for {
			rf.mutex.RLock()
			failures, starting, retry := rf.aacFailures, rf.aacStarting, rf.aacRetry
			rf.mutex.RUnlock()
			if failures == try && !starting {
				if left := time.Until(retry); left < aacRetryAfter(try)-time.Second || left > aacRetryAfter(try) {
					t.Fatalf("try %d: next in %v, want about %v", try, left, aacRetryAfter(try))
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("try %d: failures %d", try, failures)
			}
			time.Sleep(10 * time.Millisecond)
		}
		rf.mutex.Lock()
		rf.aacRetry = time.Time{} // let the next packet try again
		rf.mutex.Unlock()
	}
}

func TestControlBaseDropsTheQuery(t *testing.T) {
	for in, want := range map[string]string{
		"rtsp://h:8554/cam?x=1":    "rtsp://h:8554/cam",
		"rtsp://h:8554/cam/":       "rtsp://h:8554/cam",
		"rtsp://u:p@h:8554/cam#f":  "rtsp://u:p@h:8554/cam",
		"rtsp://h:8554/cam/hd?a=b": "rtsp://h:8554/cam/hd",
	} {
		if got := controlBase(in); got != want {
			t.Errorf("controlBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// The forwarder sets the clock's resync after the silent frame, so the
// encoder's first frame goes on the wall clock.
func TestSilentFrameSetsTheResync(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	newDrain(peer)
	rf := NewRTPForwarder()
	rf.AACFFmpeg = "/nonexistent/ffmpeg" // only the silent frame, no encoder needed
	_ = rf.AddTCPClient("s1", conn, 0, 2, 0)
	_ = rf.SetupAACTrack("s1", 6, 0)
	rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1, Marker: true}, Payload: []byte{0x65, 0x88, 0x84, 0x00}})
	rf.mutex.RLock()
	resync, started := rf.aacClock.resync, rf.aacStarted
	rf.mutex.RUnlock()
	if !started || !resync {
		t.Fatalf("after the first video: started %v, resync %v; want both", started, resync)
	}
}

// fakeEncoder is an encoder that no ffmpeg runs, for the generation checks.
func fakeEncoder() *aacEncoder {
	return &aacEncoder{cmd: exec.Command("true"), stdin: nopWriteCloser{}, queue: make(chan []byte, 1), done: make(chan struct{})}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func aacClientForwarder(t *testing.T) (*RTPForwarder, *drain) {
	t.Helper()
	conn, peer := net.Pipe()
	t.Cleanup(func() { conn.Close(); peer.Close() })
	d := newDrain(peer)
	rf := NewRTPForwarder()
	rf.AACFFmpeg = "/nonexistent/ffmpeg"
	_ = rf.AddTCPClient("s1", conn, 0, 2, 0)
	_ = rf.SetupAACTrack("s1", 6, 0)
	return rf, d
}

// A stopped encoder's frames, exit and late start are its own generation's
// only: none of them touches the current encoder.
func TestAACGenerationChecks(t *testing.T) {
	rf, d := aacClientForwarder(t)
	current := fakeEncoder()
	rf.mutex.Lock()
	rf.aacGen = 5
	rf.aac = current
	rf.mutex.Unlock()

	rf.forwardAAC(4, []byte{0x21, 0x10, 0x05})
	time.Sleep(30 * time.Millisecond)
	if n := d.count(6); n != 0 {
		t.Fatalf("a stale encoder's frame reached the client (%d)", n)
	}
	rf.aacStopped(4, "old one")
	rf.mutex.RLock()
	same, failures := rf.aac == current, rf.aacFailures
	rf.mutex.RUnlock()
	if !same || failures != 0 {
		t.Fatalf("a stale exit: encoder replaced %v, failures %d", !same, failures)
	}

	rf.forwardAAC(5, []byte{0x21, 0x10, 0x05})
	time.Sleep(30 * time.Millisecond)
	if n := d.count(6); n != 1 {
		t.Fatalf("the current encoder's frame: %d packets, want 1", n)
	}

	// A start that the forwarder moved on from is closed, not installed.
	rf.mutex.Lock()
	rf.aacStarting = true
	rf.mutex.Unlock()
	rf.AACFFmpeg = "/bin/sleep" // runs, so the start itself succeeds
	rf.startAAC(4, false)
	rf.mutex.RLock()
	same, starting := rf.aac == current, rf.aacStarting
	rf.mutex.RUnlock()
	if !same || starting {
		t.Fatalf("a stale start: encoder replaced %v, still starting %v", !same, starting)
	}
}

// A frame from the current encoder ends a failure streak.
func TestAACFailuresResetOnAFrame(t *testing.T) {
	rf, _ := aacClientForwarder(t)
	rf.mutex.Lock()
	rf.aacGen, rf.aac, rf.aacFailures = 1, fakeEncoder(), 3
	rf.mutex.Unlock()
	rf.forwardAAC(1, []byte{0x21, 0x10, 0x05})
	rf.mutex.RLock()
	failures := rf.aacFailures
	rf.mutex.RUnlock()
	if failures != 0 {
		t.Fatalf("failures %d after a frame, want 0", failures)
	}
}

// While one encoder starts, audio does not start a second.
func TestAACStartingStartsNoSecond(t *testing.T) {
	rf, _ := aacClientForwarder(t)
	rf.mutex.Lock()
	rf.aacStarting, rf.aacGen = true, 7
	rf.mutex.Unlock()
	rf.ForwardAudioPacket(g711Silence(1))
	rf.ForwardAudioPacket(g711Silence(2))
	rf.mutex.RLock()
	gen := rf.aacGen
	rf.mutex.RUnlock()
	if gen != 7 {
		t.Fatalf("gen %d: another encoder was started while one was starting", gen)
	}
}

// An ffmpeg that exits before its start takes the lock is counted as a
// failure and not installed, so the next try comes. The seam lets its exit
// (aacStopped, which then finds no encoder) run before startAAC goes on.
func TestAACEncoderThatExitsAtOnceIsNotInstalled(t *testing.T) {
	rf, _ := aacClientForwarder(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'no such encoder: lab' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rf.AACFFmpeg = script // starts, says why on stderr, exits at once
	old := newAACEncoder
	defer func() { newAACEncoder = old }()
	newAACEncoder = func(ffmpeg string, alaw bool, onFrame func([]byte), onExit func(string)) (*aacEncoder, error) {
		exited := make(chan struct{})
		enc, err := startAACEncoder(ffmpeg, alaw, onFrame, func(s string) { onExit(s); close(exited) })
		if err == nil {
			<-exited
		}
		return enc, err
	}
	rf.mutex.Lock()
	rf.aacGen++
	gen := rf.aacGen
	rf.aacStarting = true
	rf.mutex.Unlock()
	rf.startAAC(gen, false)
	rf.mutex.RLock()
	starting, installed, failures := rf.aacStarting, rf.aac != nil, rf.aacFailures
	rf.mutex.RUnlock()
	if starting || installed || failures != 1 {
		t.Fatalf("starting %v, installed %v, failures %d; want a counted failure and no encoder", starting, installed, failures)
	}
	if len(testLog.lines("no such encoder: lab")) == 0 {
		t.Fatal("ffmpeg's stderr is not in the log")
	}
}

// A second PLAY of the same client takes the tracks it now has.
func TestReplayResetsTheTracks(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	d := newDrain(peer)
	rf := NewRTPForwarder()
	if err := rf.AddTCPClientTracks("s1", conn, 0, 2, 0, true, false); err != nil {
		t.Fatal(err)
	}
	if err := rf.AddTCPClientTracks("s1", conn, 0, 2, 0, false, true); err != nil {
		t.Fatal(err)
	}
	rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1, Marker: true}, Payload: []byte{0x65, 0x88, 0x84, 0x00}})
	rf.ForwardAudioPacket(g711Silence(1))
	time.Sleep(50 * time.Millisecond)
	if d.count(0) != 0 || d.count(2) == 0 {
		t.Fatalf("after the second PLAY: video %d, G.711 %d; want none and some", d.count(0), d.count(2))
	}
}

// fakeFFmpeg writes an ffmpeg stand-in that answers -encoders and -formats
// with the given listings (and sleeps first when slow), first in PATH.
func fakeFFmpeg(t *testing.T, formats string, slow bool) {
	t.Helper()
	dir := t.TempDir()
	sleep := ""
	if slow {
		sleep = "sleep 5\n"
	}
	script := "#!/bin/sh\n" + sleep + `case "$2" in
-encoders) printf ' A....D aac                  AAC\n';;
-formats) printf '` + formats + `';;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func TestFindFFmpegNeedsEveryPart(t *testing.T) {
	all := ` D   alaw  A-law\n D   mulaw  mu-law\n  E  adts  ADTS\n`
	fakeFFmpeg(t, all, false)
	if FindFFmpeg() == "" {
		t.Fatal("an ffmpeg with everything was refused")
	}
	for _, missing := range []string{"alaw", "mulaw", "adts"} {
		listing := strings.ReplaceAll(all, " "+missing+" ", " other ")
		fakeFFmpeg(t, listing, false)
		if got := FindFFmpeg(); got != "" {
			t.Errorf("without %s: %q, want none", missing, got)
		}
	}
}

func TestFindFFmpegGivesUpOnAHangingOne(t *testing.T) {
	fakeFFmpeg(t, "", true)
	old := ffmpegQueryTimeout
	ffmpegQueryTimeout = 200 * time.Millisecond
	defer func() { ffmpegQueryTimeout = old }()
	start := time.Now()
	if got := FindFFmpeg(); got != "" {
		t.Fatalf("a hanging ffmpeg was taken: %q", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("FindFFmpeg took %v", d)
	}
}
