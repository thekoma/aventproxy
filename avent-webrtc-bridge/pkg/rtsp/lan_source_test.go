package rtsp

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/lan"

	"github.com/pion/rtp"
)

// fakeLANClient stands in for the monitor: start runs in place of the
// handshake, and tests drive the callbacks the source handed over in cfg.
type fakeLANClient struct {
	cfg    lan.ClientConfig
	ctx    context.Context
	start  func(f *fakeLANClient) error
	closed atomic.Int32
	done   chan struct{} // closed by the first Close
}

func (f *fakeLANClient) Start(ctx context.Context) error {
	f.ctx = ctx
	if f.start == nil {
		return nil
	}
	return f.start(f)
}

func (f *fakeLANClient) Close() error {
	if f.closed.Add(1) == 1 {
		close(f.done)
	}
	return nil
}

type forwarded struct {
	channel byte // interleaved channel: 0 video, 2 audio
	pkt     *rtp.Packet
}

// watchForwarder attaches a TCP client to rf and hands back what it receives.
func watchForwarder(t *testing.T, rf *RTPForwarder) <-chan forwarded {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	if err := rf.AddTCPClient("watch", server, 0, 2, 4); err != nil {
		t.Fatalf("add client: %v", err)
	}
	out := make(chan forwarded, 16)
	go func() {
		defer close(out)
		header := make([]byte, 4)
		for {
			if _, err := io.ReadFull(client, header); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint16(header[2:4]))
			if _, err := io.ReadFull(client, body); err != nil {
				return
			}
			p := &rtp.Packet{}
			if err := p.Unmarshal(body); err != nil {
				return
			}
			out <- forwarded{header[1], p}
		}
	}()
	return out
}

func nextPacket(t *testing.T, pkts <-chan forwarded) forwarded {
	t.Helper()
	select {
	case f, ok := <-pkts:
		if !ok {
			t.Fatal("forwarder watcher stopped")
		}
		return f
	case <-time.After(time.Second):
		t.Fatal("nothing forwarded")
	}
	return forwarded{}
}

func expectNothing(t *testing.T, pkts <-chan forwarded) {
	t.Helper()
	select {
	case f, ok := <-pkts:
		if !ok {
			t.Fatal("forwarder watcher stopped")
		}
		t.Fatalf("forwarded %+v, want nothing", f.pkt.Header)
	case <-time.After(100 * time.Millisecond):
	}
}

var testIDR = &lan.VideoFrame{Width: 1280, Height: 720, FPS: 20, NAL: []byte{0x65, 0x88, 0x84, 0x00}}

func newTestLANSource(t *testing.T, fake *fakeLANClient) (*LANSource, <-chan forwarded) {
	t.Helper()
	rf := NewRTPForwarder()
	pkts := watchForwarder(t, rf)
	src := NewLANSource("dev1", "Nursery", "uid1", LANTarget{LocalKey: "lk", Password: "pw", IP: "192.0.2.10"}, rf)
	fake.done = make(chan struct{})
	src.newClient = func(cfg lan.ClientConfig) lanClient {
		fake.cfg = cfg
		return fake
	}
	return src, pkts
}

// startedLANSource returns a source past its first frame, that frame drained.
func startedLANSource(t *testing.T) (*LANSource, *fakeLANClient, <-chan forwarded) {
	t.Helper()
	fake := &fakeLANClient{start: func(f *fakeLANClient) error {
		go f.cfg.OnVideo(testIDR)
		return nil
	}}
	src, pkts := newTestLANSource(t, fake)
	t.Cleanup(src.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := src.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextPacket(t, pkts)
	return src, fake, pkts
}

func TestLANSourceStartsOnFirstFrame(t *testing.T) {
	fake := &fakeLANClient{start: func(f *fakeLANClient) error {
		go func() {
			time.Sleep(50 * time.Millisecond)
			f.cfg.OnVideo(testIDR)
		}()
		return nil
	}}
	src, pkts := newTestLANSource(t, fake)
	defer src.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	begun := time.Now()
	if err := src.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if elapsed := time.Since(begun); elapsed < 50*time.Millisecond {
		t.Errorf("Start returned after %v, before the first frame", elapsed)
	}
	got := nextPacket(t, pkts)
	if got.channel != 0 || got.pkt.PayloadType != 96 || !got.pkt.Marker {
		t.Errorf("got channel %d, PT %d, marker %v; want video, PT 96, marker set",
			got.channel, got.pkt.PayloadType, got.pkt.Marker)
	}

	// The attempt's context bounds the start, not the stream.
	cancel()
	time.Sleep(20 * time.Millisecond)
	if fake.ctx.Err() != nil || fake.closed.Load() != 0 {
		t.Error("ending the start context ended the stream")
	}
}

func TestLANSourceFailsWithoutFirstFrame(t *testing.T) {
	for name, start := range map[string]func(*fakeLANClient) error{
		"login ignored": nil,
		"handshake hangs": func(f *fakeLANClient) error {
			select {
			case <-f.done:
			case <-time.After(2 * time.Second):
			}
			return net.ErrClosed
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeLANClient{start: start}
			src, _ := newTestLANSource(t, fake)

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			begun := time.Now()
			err := src.Start(ctx)
			if !errors.Is(err, errNoFirstFrame) {
				t.Fatalf("Start: %v, want errNoFirstFrame", err)
			}
			if elapsed := time.Since(begun); elapsed > 400*time.Millisecond {
				t.Errorf("Start took %v", elapsed)
			}
			if fake.closed.Load() == 0 {
				t.Error("client left open")
			}
		})
	}
}

// Only what lets a decoder start counts: a black stream must not pass.
func TestLANSourceFirstFrameNeedsSPSOrIDR(t *testing.T) {
	for _, tc := range []struct {
		name string
		nal  []byte
		ok   bool
	}{
		{"H.264 SPS", []byte{0x67, 0x42, 0x00, 0x1f}, true},
		{"FU-A start of an IDR", []byte{0x7c, 0x85, 0x88, 0x84}, true},
		{"STAP-A with SPS and PPS", []byte{0x78, 0x00, 0x02, 0x67, 0x42, 0x00, 0x02, 0x68, 0xce}, true},
		{"HEVC IDR_W_RADL", []byte{0x26, 0x01, 0xaf, 0x00}, false},
		{"Annex-B IDR", []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := &lan.VideoFrame{Width: 1280, Height: 720, FPS: 20, NAL: tc.nal}
			fake := &fakeLANClient{start: func(f *fakeLANClient) error {
				go f.cfg.OnVideo(frame)
				return nil
			}}
			src, _ := newTestLANSource(t, fake)
			defer src.Stop()

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			err := src.Start(ctx)
			if tc.ok && err != nil {
				t.Fatalf("Start: %v, want success", err)
			}
			if !tc.ok && !errors.Is(err, errNoFirstFrame) {
				t.Fatalf("Start: %v, want errNoFirstFrame", err)
			}
		})
	}
}

func TestLANSourceForwardsG711(t *testing.T) {
	_, fake, pkts := startedLANSource(t)

	fake.cfg.OnAudio(&lan.AudioFrame{SampleRate: 16000, Channels: 1, Samples: make([]byte, 2*320)})
	got := nextPacket(t, pkts)
	if got.channel != 2 || got.pkt.PayloadType != 0 || len(got.pkt.Payload) != 160 {
		t.Errorf("got channel %d, PT %d, %d bytes; want audio, PT 0, 160 bytes",
			got.channel, got.pkt.PayloadType, len(got.pkt.Payload))
	}
}

func TestLANSourceDropsUnexpectedAudio(t *testing.T) {
	_, fake, pkts := startedLANSource(t)

	fake.cfg.OnAudio(&lan.AudioFrame{SampleRate: 48000, Channels: 1, Samples: make([]byte, 640)})
	fake.cfg.OnAudio(&lan.AudioFrame{SampleRate: 16000, Channels: 2, Samples: make([]byte, 640)})
	expectNothing(t, pkts)
}

func TestLANSourceReportsDeath(t *testing.T) {
	src, fake, _ := startedLANSource(t)
	errs := make(chan error, 2)
	src.SetOnError(func(err error) { errs <- err })

	fake.cfg.OnClosed(io.EOF)
	fake.cfg.OnClosed(io.EOF)
	select {
	case err := <-errs:
		if !errors.Is(err, io.EOF) {
			t.Errorf("reported %v, want it to wrap io.EOF", err)
		}
	default:
		t.Fatal("death not reported")
	}
	select {
	case err := <-errs:
		t.Errorf("reported twice, again with %v", err)
	default:
	}
}

func TestLANSourceStopIsQuiet(t *testing.T) {
	src, fake, pkts := startedLANSource(t)
	errs := make(chan error, 1)
	src.SetOnError(func(err error) { errs <- err })

	src.Stop()
	if fake.closed.Load() == 0 {
		t.Error("Stop left the client open")
	}
	fake.cfg.OnClosed(io.EOF)
	fake.cfg.OnVideo(testIDR)
	select {
	case err := <-errs:
		t.Errorf("error reported after Stop: %v", err)
	default:
	}
	expectNothing(t, pkts)
}

func TestLANSourceStopDuringStart(t *testing.T) {
	fake := &fakeLANClient{start: func(f *fakeLANClient) error {
		select {
		case <-f.ctx.Done():
		case <-f.done:
		case <-time.After(2 * time.Second):
		}
		return net.ErrClosed
	}}
	src, _ := newTestLANSource(t, fake)

	result := make(chan error, 1)
	go func() { result <- src.Start(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	src.Stop()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Start succeeded after Stop")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Start still running after Stop")
	}
}
