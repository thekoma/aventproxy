package rtsp

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
)

// The camera's audio is G.711 at 8 kHz, which Home Assistant's HLS player
// drops: its stream component keeps only AAC and MP3 audio, so every
// dashboard card on HLS was silent. The RTSP description therefore carries
// the same audio a second time as AAC, before the G.711 track because Home
// Assistant takes the first audio track. G.711 stays for clients that want it
// (WebRTC through go2rtc passes it to the browser as it is). ffmpeg does the
// encoding: one process per stream while a client has the AAC track set up.

const (
	aacPayloadType = 97
	aacSSRC        = 2
	aacSampleRate  = 16000 // 8 kHz AAC is valid but not every decoder takes it
	aacFrameTicks  = 1024  // samples per AAC-LC frame
	// aacMaxLag is how far the frame count may fall behind the wall clock
	// before the timestamps jump ahead: after a pause in the camera's audio
	// the next frame is stamped at the time it was heard, not right after
	// the frame before the pause.
	aacMaxLag = 250 * time.Millisecond
	// aacWriteQueue holds G.711 payloads for ffmpeg; a full queue drops,
	// so a stuck encoder can never hold up the video.
	aacWriteQueue = 64
)

// aacSilentFrame is one AAC-LC frame of silence (16 kHz mono), as ffmpeg's
// encoder makes it for digital silence; it decodes to zeros. The bridge sends
// it right after the first video packet, before the encoder has anything:
// the encoder holds back its first frame until it has 256 ms of audio, and
// on a cold start, with the video arriving in a burst, the first AAC packet
// came up to 26 packets into the stream. Home Assistant stops looking for
// audio after 20 ("Audio stream not found") and then skips its check for
// ADTS-framed AAC, which these RTP frames do not need, but the warning stays.
var aacSilentFrame = []byte{0x01, 0x18, 0x20, 0x07}

// aacSDP is the media description of the AAC track: AAC-LC, 16 kHz, mono,
// RFC 3640 AAC-hbr. config is the AudioSpecificConfig (object type 2,
// frequency index 8, one channel).
func aacSDP(control string) string {
	return "m=audio 0 RTP/AVP 97\r\n" +
		"a=rtpmap:97 MPEG4-GENERIC/16000/1\r\n" +
		"a=fmtp:97 streamtype=5;profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1408\r\n" +
		"a=control:" + control + "\r\n" +
		"a=recvonly\r\n"
}

// ffmpegQueryTimeout bounds each of FindFFmpeg's questions to ffmpeg.
var ffmpegQueryTimeout = 5 * time.Second

// FindFFmpeg returns the path of ffmpeg when it has what the AAC copy uses
// (the AAC encoder, the mulaw and alaw demuxers, the ADTS muxer), or "".
// Each check is bounded, so a hanging ffmpeg cannot hold up the start.
func FindFFmpeg() string {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ""
	}
	query := func(what string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), ffmpegQueryTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, "-hide_banner", what)
		// A killed ffmpeg's children could keep its output open; do not wait for them.
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		if err != nil {
			return nil
		}
		return out
	}
	if !bytes.Contains(query("-encoders"), []byte(" aac ")) {
		return ""
	}
	formats := query("-formats")
	for _, f := range []string{"mulaw", "alaw", "adts"} {
		if !hasFormat(formats, f) {
			return ""
		}
	}
	return path
}

// hasFormat reports whether ffmpeg's -formats listing has the format: a line
// of flags (D, E, d) and then the name.
func hasFormat(listing []byte, name string) bool {
	for _, line := range bytes.Split(listing, []byte("\n")) {
		fields := bytes.Fields(line)
		if len(fields) >= 2 && len(bytes.Trim(fields[0], "DEd")) == 0 && string(fields[1]) == name {
			return true
		}
	}
	return false
}

// aacEncoder runs one ffmpeg that turns G.711 into ADTS-framed AAC.
type aacEncoder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	queue chan []byte

	closeOnce sync.Once
	done      chan struct{}
	// stderr is what ffmpeg wrote there; complete once done is closed by
	// ffmpeg's exit (Close comes after cmd.Wait).
	stderr *limitedBuffer
}

// ffmpegAACArgs are the arguments for an encoder fed G.711 at 8 kHz on stdin.
func ffmpegAACArgs(alaw bool) []string {
	in := "mulaw"
	if alaw {
		in = "alaw"
	}
	return []string{
		"-hide_banner", "-loglevel", "error",
		// Without the probe limits ffmpeg analyses its input first: the first
		// AAC frame came 2.0 s after the first G.711 packet, against 0.14 s
		// with them, and Home Assistant drops an audio track that sends
		// nothing in its first 20 packets (about a second of video). No
		// -fflags nobuffer: with it ffmpeg 5.1 reads nothing from a pipe.
		"-probesize", "32", "-analyzeduration", "0",
		"-f", in, "-ar", "8000", "-ac", "1", "-i", "pipe:0",
		"-c:a", "aac", "-b:a", "32k", "-ar", "16000", "-ac", "1",
		"-f", "adts", "-flush_packets", "1", "pipe:1",
	}
}

// newAACEncoder starts an encoder; a test seam over startAACEncoder.
var newAACEncoder = startAACEncoder

// startAACEncoder starts ffmpeg and calls onFrame with every raw AAC frame
// (ADTS header removed), from its own goroutine, until the encoder stops.
// onExit runs once when ffmpeg is gone, with what it wrote to stderr.
func startAACEncoder(ffmpeg string, alaw bool, onFrame func([]byte), onExit func(stderr string)) (*aacEncoder, error) {
	cmd := exec.Command(ffmpeg, ffmpegAACArgs(alaw)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	e := &aacEncoder{cmd: cmd, stdin: stdin, queue: make(chan []byte, aacWriteQueue), done: make(chan struct{}), stderr: &stderr}
	go e.writeLoop()
	go func() {
		readADTS(stdout, onFrame)
		_ = cmd.Wait()
		e.Close()
		if onExit != nil {
			onExit(strings.TrimSpace(stderr.String()))
		}
	}()
	return e, nil
}

func (e *aacEncoder) writeLoop() {
	for {
		select {
		case <-e.done:
			return
		case b := <-e.queue:
			if _, err := e.stdin.Write(b); err != nil {
				e.Close()
				return
			}
		}
	}
}

// Write queues one G.711 payload; it never blocks.
func (e *aacEncoder) Write(g711 []byte) {
	select {
	case <-e.done:
	case e.queue <- append([]byte(nil), g711...):
	default:
	}
}

// Close stops ffmpeg. It does not wait for it: the reader goroutine calls
// back into the forwarder, which may hold its lock while closing.
func (e *aacEncoder) Close() {
	e.closeOnce.Do(func() {
		close(e.done)
		_ = e.stdin.Close()
		if e.cmd.Process != nil {
			_ = e.cmd.Process.Kill()
		}
	})
}

// readADTS splits an ADTS stream into raw AAC frames until EOF or an error.
// Bytes before a sync word are skipped.
func readADTS(r io.Reader, onFrame func([]byte)) {
	br := bufio.NewReader(r)
	header := make([]byte, 7)
	for {
		if _, err := io.ReadFull(br, header[:2]); err != nil {
			return
		}
		for header[0] != 0xFF || header[1]&0xF0 != 0xF0 {
			header[0] = header[1]
			b, err := br.ReadByte()
			if err != nil {
				return
			}
			header[1] = b
		}
		if _, err := io.ReadFull(br, header[2:]); err != nil {
			return
		}
		frameLen := int(header[3]&0x03)<<11 | int(header[4])<<3 | int(header[5])>>5
		headerLen := 7
		if header[1]&0x01 == 0 { // protection_absent 0: a CRC follows
			headerLen = 9
		}
		if frameLen < headerLen {
			continue
		}
		frame := make([]byte, frameLen-7)
		if _, err := io.ReadFull(br, frame); err != nil {
			return
		}
		if payload := frame[headerLen-7:]; len(payload) > 0 {
			onFrame(payload)
		}
	}
}

// aacPacket wraps one AAC frame in RFC 3640 AAC-hbr: a 16-bit AU-headers
// length (one header of 16 bits), the header (13-bit size, 3-bit index 0),
// then the frame.
func aacPacket(frame []byte, seq uint16, ts uint32, ssrc uint32) *rtp.Packet {
	payload := make([]byte, 4+len(frame))
	payload[0], payload[1] = 0x00, 0x10
	payload[2] = byte(len(frame) >> 5)
	payload[3] = byte(len(frame)&0x1F) << 3
	copy(payload[4:], frame)
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         true,
			PayloadType:    aacPayloadType,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: payload,
	}
}

// aacClock stamps AAC frames: 1024 ticks per frame, but never more than
// aacMaxLag behind the wall clock since the first frame. After resync (the
// silent frame) the next frame is stamped at the wall clock.
type aacClock struct {
	started bool
	start   time.Time
	ts      uint32
	resync  bool
}

func (c *aacClock) next(now time.Time) uint32 {
	if !c.started {
		c.started = true
		c.start = now
		c.ts = 0
		return 0
	}
	c.ts += aacFrameTicks
	wall := rtpTicks(now.Sub(c.start), aacSampleRate)
	lag := int32(wall - c.ts)
	if lag > int32(rtpTicks(aacMaxLag, aacSampleRate)) || (c.resync && lag > 0) {
		c.ts = wall
	}
	c.resync = false
	return c.ts
}

// limitedBuffer keeps the first 4 KiB written to it: enough for ffmpeg's
// error line, never a growing buffer.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := 4096 - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
