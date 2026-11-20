package rtsp

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/lan"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// Ported from Leonard Pitzu's fork (pkg/rtsp/lan_bridge.go), without its
// framing probe and L16 audio.

// LANTarget is what the LAN path needs to reach one monitor.
type LANTarget struct {
	LocalKey, Password, IP string
	Protocol               string // Tuya LAN protocol the integration's session negotiated, such as "3.3"
}

// lanStreamer is a media source that feeds a stream's forwarder over the LAN.
type lanStreamer interface {
	Start(ctx context.Context) error
	Stop()
	SetOnError(func(error))
}

// lanClient is the part of lan.Client the source drives.
type lanClient interface {
	Start(ctx context.Context) error
	Close() error
}

var errNoFirstFrame = errors.New("lan: no video within the first-frame deadline")

var _ lanStreamer = (*LANSource)(nil)

// LANSource feeds one monitor's LAN stream into an RTPForwarder, the same
// forwarder the cloud path uses, so RTSP clients cannot tell the two apart.
// It is used once: Start, then Stop.
type LANSource struct {
	deviceID, deviceName, uid string
	target                    LANTarget
	forwarder                 *RTPForwarder
	newClient                 func(lan.ClientConfig) lanClient // test seam
	firstFrame                chan struct{}                    // closed once an SPS or IDR reached the forwarder

	// Set by the imm path (imm_source.go) and nil on the LAN path, which then
	// behaves as it always has. prepare completes the client config in Start,
	// before anything connects, without the lock; its error ends the attempt.
	// frameWait bounds the wait for the first frame once the handshake is
	// done. stall says why that wait failed.
	prepare   func(ctx context.Context, cfg *lan.ClientConfig) error
	frameWait time.Duration
	stall     func() error

	// Messages forwarded, for the imm session's diagnostics.
	videoFrames, audioFrames atomic.Uint64

	// mu also covers forwarding, so once Stop returns nothing more is forwarded.
	mu       sync.Mutex
	client   lanClient
	cancel   context.CancelCauseFunc // ends the client's life
	onError  func(error)
	stopped  bool
	running  bool // Start succeeded; a death now goes to onError
	reported bool // a death was handled

	preparing bool // under mu: prepare is running, so the source is used

	videoSSRC, audioSSRC uint32
	videoSeq, audioSeq   uint16
	gotFrame             bool
	payloader            codecs.H264Payloader
	downsampler          halfRateDownsampler
	audioWarned          bool
}

// NewLANSource prepares a source; nothing touches the network until Start.
func NewLANSource(deviceID, deviceName, uid string, target LANTarget, forwarder *RTPForwarder) *LANSource {
	return &LANSource{
		deviceID:   deviceID,
		deviceName: deviceName,
		uid:        uid,
		target:     target,
		forwarder:  forwarder,
		newClient:  func(cfg lan.ClientConfig) lanClient { return lan.NewClient(cfg) },
		firstFrame: make(chan struct{}),
	}
}

// SetOnError sets the callback for a stream that dies after Start succeeded.
// Set it before Start: a death reported while it is nil is dropped. It is
// called at most once, without the source's lock held, so it may call Stop,
// and never for a death detected after Stop began; one detected just before
// can still arrive while Stop runs, so the callback must check that the
// source is still the one it cares about.
func (s *LANSource) SetOnError(fn func(error)) {
	s.mu.Lock()
	s.onError = fn
	s.mu.Unlock()
}

// Start connects and returns once an H.264 SPS or IDR reached the
// forwarder. ctx bounds only that wait: when it ends first, Start fails with
// errNoFirstFrame. The stream itself lives until Stop.
func (s *LANSource) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped || s.client != nil || s.preparing {
		s.mu.Unlock()
		return errors.New("lan: source already used")
	}
	cfg := lan.ClientConfig{
		DeviceID: s.deviceID,
		LocalKey: s.target.LocalKey,
		Password: s.target.Password,
		UID:      s.uid,
		IP:       s.target.IP,
		OnVideo:  s.onVideo,
		OnAudio:  s.onAudio,
		OnClosed: s.onClosed,
	}
	if s.prepare != nil {
		s.preparing = true
		s.mu.Unlock()
		if err := s.prepare(ctx, &cfg); err != nil {
			s.Stop()
			return err
		}
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return errors.New("lan: source stopped")
		}
	}
	// Not ctx: lan.Client ends the stream, without OnClosed, when its context does.
	life, cancel := context.WithCancelCause(context.Background())
	client := s.newClient(cfg)
	s.client, s.cancel = client, cancel
	// A fresh SSRC per session keeps a reconnect from looking like the old stream.
	s.videoSSRC, s.audioSSRC = rand.Uint32(), rand.Uint32()
	s.mu.Unlock()

	// Closing the client is what aborts a handshake still in progress.
	stopClosing := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClosing()

	if err := client.Start(life); err != nil {
		return s.abort(ctx, life, err)
	}
	wait := ctx
	if s.frameWait > 0 {
		var done context.CancelFunc
		wait, done = context.WithTimeout(ctx, s.frameWait)
		defer done()
	}
	select {
	case <-s.firstFrame:
	case <-wait.Done():
		return s.abort(wait, life, nil)
	case <-life.Done(): // stopped, or died before the first frame
		return s.abort(wait, life, nil)
	}
	if !stopClosing() { // ctx ended just as the frame came and closed the client
		return s.abort(ctx, life, nil)
	}

	s.mu.Lock()
	err := context.Cause(life)
	s.running = err == nil
	s.mu.Unlock()
	if err != nil {
		return s.abort(ctx, life, err)
	}
	return nil
}

// abort tears down a start that did not succeed and says why, keeping the
// client's own error when the deadline ended it: that names the failed step.
func (s *LANSource) abort(ctx, life context.Context, err error) error {
	if cause := context.Cause(life); cause != nil {
		err = cause
	}
	if ctx.Err() != nil {
		if err == nil && s.stall != nil {
			err = s.stall()
		}
		if err == nil {
			err = errors.New("connected, no SPS or IDR")
		}
		err = fmt.Errorf("%w: %w (%w)", errNoFirstFrame, ctx.Err(), err)
	}
	s.Stop()
	return err
}

// Stop tears the stream down. Nothing is forwarded once it returns, and a
// death arriving after it began is not reported.
func (s *LANSource) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	client := s.client
	if s.cancel != nil {
		s.cancel(errors.New("lan: source stopped"))
	}
	s.mu.Unlock()

	if client != nil {
		_ = client.Close()
	}
}

// onClosed hands a dead stream to Start while it waits, or to onError after.
func (s *LANSource) onClosed(err error) {
	err = fmt.Errorf("lan: %s stopped streaming: %w", s.deviceName, err)
	s.mu.Lock()
	if s.stopped || s.reported {
		s.mu.Unlock()
		return
	}
	s.reported = true
	if !s.running {
		s.cancel(err)
		s.mu.Unlock()
		return
	}
	onError := s.onError
	s.mu.Unlock()
	if onError != nil {
		onError(err)
	}
}

// onVideo forwards one of the monitor's RTP payloads.
//
// They are RTP payloads, not raw NALs, so they normally need only a header:
// re-packetising an FU-A fragment would produce a stream that flows but
// decodes to nothing. Only a whole NAL too big for one packet is fragmented.
func (s *LANSource) onVideo(frame *lan.VideoFrame) {
	if frame == nil || len(frame.NAL) < 2 {
		return
	}
	nal := correctSPSLevel(frame.NAL, minLevel(frame.Width, frame.Height, frame.FPS))

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.videoFrames.Add(1)
	// The forwarder sets the timestamp: one per picture, from the marker.
	for _, payload := range splitNAL(nal, &s.payloader) {
		s.videoSeq++
		s.forwarder.ForwardVideoPacket(&rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: s.videoSeq,
				SSRC:           s.videoSSRC,
				Marker:         endsAccessUnit(payload),
			},
			Payload: payload,
		})
		// A stream that flows but cannot decode (HEVC, Annex-B) must not count.
		if !s.gotFrame && carriesSPSOrIDR(payload) {
			s.gotFrame = true
			close(s.firstFrame)
		}
	}
}

// onAudio turns the monitor's 16 kHz PCM into the 8 kHz μ-law the SDP
// announces and forwards it.
func (s *LANSource) onAudio(frame *lan.AudioFrame) {
	if frame == nil || len(frame.Samples) < 2 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	if frame.SampleRate != 16000 || frame.Channels != 1 {
		if !s.audioWarned {
			s.audioWarned = true
			core.Logger.Warn().Msgf("Ignoring LAN audio from %s: %d Hz, %d channel(s), codec %d, not 16 kHz mono",
				s.deviceName, frame.SampleRate, frame.Channels, frame.Codec)
		}
		return
	}
	s.audioFrames.Add(1)
	samples := s.downsampler.process(pcm16LE(frame.Samples))
	if len(samples) == 0 {
		return
	}
	s.audioSeq++
	// The forwarder sets the timestamp by counting these 8 kHz samples.
	s.forwarder.ForwardAudioPacket(&rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    0, // PCMU
			SequenceNumber: s.audioSeq,
			SSRC:           s.audioSSRC,
		},
		Payload: encodeMuLaw(samples),
	})
}
