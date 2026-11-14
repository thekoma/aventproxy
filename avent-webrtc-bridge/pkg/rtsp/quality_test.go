package rtsp

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"

	"github.com/pion/rtp"
)

// feeder sends video packets on a fake clock and lets the controller decide
// once per simulated second. HD runs at about 180 packets/s; tests that
// simulate hours use a lower rate that still fills minPackets per window, so
// the race-enabled build gate stays fast on the ARM host.
type feeder struct {
	t         *testing.T
	q         *qualityController
	now       time.Time
	seq       uint16
	sent      int
	perSecond int
}

func newFeeder(t *testing.T, q *qualityController) *feeder {
	f := &feeder{t: t, q: q, now: time.Unix(1_700_000_000, 0), perSecond: 180}
	q.StartSession(f.now)
	return f
}

// run feeds seconds of video and drops every lossEvery-th packet (0: none).
// It returns every decision taken.
func (f *feeder) run(seconds int, lossEvery int) []qualityDecision {
	var decisions []qualityDecision
	for s := 0; s < seconds; s++ {
		for i := 0; i < f.perSecond; i++ {
			f.sent++
			f.seq++
			if lossEvery > 0 && f.sent%lossEvery == 0 {
				continue
			}
			f.q.Observe(f.seq, f.now.Add(time.Duration(i)*time.Second/time.Duration(f.perSecond)))
		}
		f.now = f.now.Add(time.Second)
		if d, ok := f.q.Evaluate(f.now); ok {
			decisions = append(decisions, d)
		}
	}
	return decisions
}

func TestParseVideoQuality(t *testing.T) {
	for in, want := range map[string]VideoQuality{"": VideoQualityHD, "hd": VideoQualityHD, " SD ": VideoQualitySD, "Auto": VideoQualityAuto} {
		got, err := ParseVideoQuality(in)
		if err != nil || got != want {
			t.Errorf("ParseVideoQuality(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseVideoQuality("4k"); err == nil {
		t.Error("ParseVideoQuality accepted an unknown quality")
	}
}

func TestQualityCleanStreamStaysHD(t *testing.T) {
	q := newQualityController(defaultQualityPolicy)
	if d := newFeeder(t, q).run(600, 0); len(d) != 0 {
		t.Fatalf("clean stream changed quality: %v", d)
	}
	if q.Current() != VideoQualityHD {
		t.Fatalf("current = %s, want hd", q.Current())
	}
}

func TestQualitySustainedLossStepsDownOnce(t *testing.T) {
	q := newQualityController(defaultQualityPolicy)
	d := newFeeder(t, q).run(30, 5) // 20% loss, as measured on 2026-11-08
	if len(d) != 1 || d[0].To != VideoQualitySD {
		t.Fatalf("decisions = %v, want one step down to sd", d)
	}
	if d[0].Loss < 0.15 || d[0].Packets < defaultQualityPolicy.minPackets {
		t.Fatalf("decision = %v, want about 20%% loss over a full window", d[0])
	}
}

func TestQualitySettlesAfterSessionStart(t *testing.T) {
	q := newQualityController(defaultQualityPolicy)
	if d := newFeeder(t, q).run(4, 5); len(d) != 0 {
		t.Fatalf("decided during settle time: %v", d)
	}
}

func TestQualityIgnoresShortBurstAndReordering(t *testing.T) {
	q := newQualityController(defaultQualityPolicy)
	f := newFeeder(t, q)
	f.run(20, 0)
	// One burst of 10 lost packets.
	f.seq += 10
	// Adjacent packets swapped by Wi-Fi are late, not lost.
	for i := 0; i < 50; i++ {
		f.q.Observe(f.seq+2, f.now)
		f.q.Observe(f.seq+1, f.now)
		f.seq += 2
	}
	if d := f.run(30, 0); len(d) != 0 {
		t.Fatalf("burst or reordering changed quality: %v", d)
	}
}

func TestQualityLateRetransmissionStaysLost(t *testing.T) {
	q := newQualityController(defaultQualityPolicy)
	f := newFeeder(t, q)
	f.run(20, 0)
	// Every fifth packet is lost and arrives again 100 ms later, as a slow
	// NACK retransmission would; the reorder buffer has already skipped it.
	var decisions []qualityDecision
	for s := 0; s < 15; s++ {
		for i := 0; i < 180; i++ {
			at := f.now.Add(time.Duration(i) * time.Second / 180)
			f.seq++
			if f.seq%5 == 0 {
				continue
			}
			q.Observe(f.seq, at)
			if f.seq%5 == 4 {
				q.Observe(f.seq-4, at.Add(100*time.Millisecond))
			}
		}
		f.now = f.now.Add(time.Second)
		if d, ok := q.Evaluate(f.now); ok {
			decisions = append(decisions, d)
		}
	}
	// The window still holds clean seconds when it first crosses the threshold.
	if len(decisions) != 1 || decisions[0].To != VideoQualitySD || decisions[0].Loss < defaultQualityPolicy.downgradeLoss {
		t.Fatalf("decisions = %v, want one step down to sd", decisions)
	}
}

func TestQualityTimelyRetransmissionRecovers(t *testing.T) {
	q := newQualityController(defaultQualityPolicy)
	f := newFeeder(t, q)
	f.run(20, 0)
	for s := 0; s < 15; s++ {
		for i := 0; i < 180; i++ {
			at := f.now.Add(time.Duration(i) * time.Second / 180)
			f.seq++
			if f.seq%5 == 0 {
				continue
			}
			q.Observe(f.seq, at)
			if f.seq%5 == 1 {
				q.Observe(f.seq-1, at.Add(10*time.Millisecond))
			}
		}
		f.now = f.now.Add(time.Second)
		if d, ok := q.Evaluate(f.now); ok {
			t.Fatalf("timely retransmissions changed quality: %v", d)
		}
	}
}

func TestQualityProbesHDWithBackoff(t *testing.T) {
	p := defaultQualityPolicy
	q := newQualityController(p)
	f := newFeeder(t, q)
	f.perSecond = 30

	if d := f.run(20, 5); len(d) != 1 || d[0].To != VideoQualitySD {
		t.Fatalf("first decisions = %v, want sd", d)
	}

	// Clean SD: no probe before the delay, one probe after it.
	d := f.run(int(p.probeDelay/time.Second)+5, 0)
	if len(d) != 1 || d[0].To != VideoQualityHD {
		t.Fatalf("probe decisions = %v, want one hd probe", d)
	}

	// The probe fails: back to SD and the next probe waits twice as long.
	if d := f.run(20, 5); len(d) != 1 || d[0].To != VideoQualitySD {
		t.Fatalf("failed probe decisions = %v, want sd", d)
	}
	if got := q.NextProbeDelay(); got != 2*p.probeDelay {
		t.Fatalf("probe delay = %s, want %s", got, 2*p.probeDelay)
	}
	if d := f.run(int(p.probeDelay/time.Second)+5, 0); len(d) != 0 {
		t.Fatalf("probed again before the backoff: %v", d)
	}
	if d := f.run(int(p.probeDelay/time.Second), 0); len(d) != 1 || d[0].To != VideoQualityHD {
		t.Fatalf("second probe decisions = %v, want hd", d)
	}

	// HD stays clean long enough: the backoff resets.
	f.run(int(p.stableHD/time.Second)+2, 0)
	if got := q.NextProbeDelay(); got != p.probeDelay {
		t.Fatalf("probe delay after stable hd = %s, want %s", got, p.probeDelay)
	}
}

func TestQualityBackoffIsCapped(t *testing.T) {
	p := defaultQualityPolicy
	q := newQualityController(p)
	f := newFeeder(t, q)
	f.perSecond = 30
	f.run(20, 5)
	for i := 0; i < 8; i++ {
		f.run(int(q.NextProbeDelay()/time.Second)+5, 0)
		f.run(20, 5)
	}
	if got := q.NextProbeDelay(); got != p.maxProbeDelay {
		t.Fatalf("probe delay = %s, want cap %s", got, p.maxProbeDelay)
	}
}

func TestQualityLossyStreamDoesNotProbe(t *testing.T) {
	p := defaultQualityPolicy
	q := newQualityController(p)
	f := newFeeder(t, q)
	f.perSecond = 30
	f.run(20, 5)
	// SD still loses 1%: never probe HD.
	if d := f.run(int(p.probeDelay/time.Second)*3, 100); len(d) != 0 {
		t.Fatalf("lossy sd probed hd: %v", d)
	}
}

func TestH264SPS(t *testing.T) {
	sps := []byte{0x67, 0x4d, 0x00, 0x28}
	pps := []byte{0x68, 0xee, 0x3c}
	stap := []byte{0x18, 0, byte(len(sps))}
	stap = append(stap, sps...)
	stap = append(stap, 0, byte(len(pps)))
	stap = append(stap, pps...)

	cases := map[string]struct {
		payload []byte
		want    []byte
	}{
		"single SPS":     {sps, sps},
		"STAP-A":         {stap, sps},
		"fragment":       {[]byte{0x7c, 0x85, 1}, nil},
		"truncated STAP": {[]byte{0x18, 0, 9, 0x67}, nil},
		"empty":          {nil, nil},
	}
	for name, c := range cases {
		if got := h264SPS(c.payload); string(got) != string(c.want) {
			t.Errorf("%s: h264SPS = %x, want %x", name, got, c.want)
		}
	}
}

// qualityBridge returns an auto-mode bridge without network state and a
// function that waits briefly for want errors and returns those it raised.
func qualityBridge() (*WebRTCBridge, func(want int) []error) {
	wb := NewWebRTCBridge(&storage.CameraInfo{DeviceName: "Test camera"}, string(VideoQualityAuto), nil, nil, nil)
	wb.quality = newQualityController(defaultQualityPolicy)
	var mu sync.Mutex
	var errs []error
	wb.SetOnError(func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, err)
	})
	return wb, func(want int) []error {
		deadline := time.Now().Add(time.Second)
		for {
			mu.Lock()
			got := append([]error(nil), errs...)
			mu.Unlock()
			if len(got) >= want || time.Now().After(deadline) {
				return got
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func spsPacket(seq uint16, version byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}, Payload: []byte{0x67, 0x4d, 0x00, version}}
}

func TestQualityRequestWithoutSignalingRenegotiates(t *testing.T) {
	wb, errs := qualityBridge()
	if wb.requestQuality(qualityDecision{From: VideoQualityHD, To: VideoQualitySD}, time.Now()) {
		t.Fatal("request without signaling client reported a live switch")
	}
	if got := errs(1); len(got) != 1 || !strings.Contains(got[0].Error(), "needs a new camera session") {
		t.Fatalf("errors = %v, want one renegotiation", got)
	}
}

func TestQualityRequestSendsClarity(t *testing.T) {
	wb, errs := qualityBridge()
	rec := &controlRecorder{}
	wb.controls = rec
	now := time.Now()
	if !wb.requestQuality(qualityDecision{From: VideoQualityHD, To: VideoQualitySD}, now) {
		t.Fatal("live request reported a renegotiation")
	}
	if !wb.requestQuality(qualityDecision{From: VideoQualitySD, To: VideoQualityHD}, now) {
		t.Fatal("live request reported a renegotiation")
	}
	if got := rec.sent(); got != "resolution 1, resolution 0" {
		t.Fatalf("controls sent %q, want SD then HD clarity", got)
	}
	if got := errs(0); len(got) != 0 {
		t.Fatalf("errors = %v, want none", got)
	}
}

func TestQualityLiveSwitchConfirmedByNewSPS(t *testing.T) {
	wb, errs := qualityBridge()
	now := time.Now()
	wb.observeQuality(spsPacket(1, 0x28), now)

	wb.qualityMutex.Lock()
	wb.pendingSwitch = &pendingQualitySwitch{to: VideoQualitySD, baseSPS: wb.lastSPS, deadline: now.Add(qualitySwitchTimeout)}
	wb.qualityMutex.Unlock()

	wb.observeQuality(spsPacket(2, 0x28), now) // same parameters: not yet
	if wb.pendingSwitch == nil {
		t.Fatal("unchanged SPS confirmed the switch")
	}
	wb.observeQuality(spsPacket(3, 0x1f), now)
	if wb.pendingSwitch != nil {
		t.Fatal("new SPS did not confirm the switch")
	}
	if wb.qualitySwitchExpired(now.Add(2 * qualitySwitchTimeout)) {
		t.Fatal("confirmed switch expired")
	}
	if got := errs(0); len(got) != 0 {
		t.Fatalf("errors = %v, want none", got)
	}
}

func TestQualityIgnoredSwitchRenegotiates(t *testing.T) {
	wb, errs := qualityBridge()
	now := time.Now()
	wb.observeQuality(spsPacket(1, 0x28), now)
	wb.qualityMutex.Lock()
	wb.pendingSwitch = &pendingQualitySwitch{to: VideoQualitySD, baseSPS: wb.lastSPS, deadline: now.Add(qualitySwitchTimeout)}
	wb.qualityMutex.Unlock()

	if wb.qualitySwitchExpired(now.Add(qualitySwitchTimeout / 2)) {
		t.Fatal("switch expired early")
	}
	if !wb.qualitySwitchExpired(now.Add(qualitySwitchTimeout + time.Second)) {
		t.Fatal("ignored switch did not expire")
	}
	got := errs(1)
	if len(got) != 1 || !strings.Contains(got[0].Error(), "video quality sd") {
		t.Fatalf("errors = %v, want one sd renegotiation", got)
	}
	if errors.Unwrap(got[0]) == nil {
		t.Fatal("renegotiation error does not wrap its cause")
	}
}

func TestServerVideoQualityRouting(t *testing.T) {
	if path, res := extractCameraPath("rtsp://h:1/Cam"); path != "/Cam" || res != "" {
		t.Fatalf("bare path = %q %q, want /Cam and no fixed quality", path, res)
	}
	if _, res := extractCameraPath("rtsp://h:1/Cam/sd"); res != "sd" {
		t.Fatalf("/sd path quality = %q", res)
	}

	server := NewRTSPServer(0, nil)
	if got := server.defaultVideoQuality(); got != VideoQualityHD {
		t.Fatalf("zero default = %s, want hd", got)
	}

	camera := &storage.CameraInfo{DeviceID: "d", DeviceName: "n"}
	fixed := NewCameraStream(camera, "hd", nil, nil, server)
	if fixed.quality != nil || fixed.webrtcBridge.quality != nil {
		t.Fatal("fixed hd stream got an adaptive controller")
	}

	first := NewCameraStream(camera, string(VideoQualityAuto), nil, nil, server)
	second := NewCameraStream(camera, string(VideoQualityAuto), nil, nil, server)
	if first.quality == nil || first.quality != second.quality {
		t.Fatal("auto streams of one camera do not share its controller")
	}
	first.mutex.Lock()
	first.replaceBridge()
	first.mutex.Unlock()
	if first.webrtcBridge.quality != first.quality {
		t.Fatal("replacement bridge lost the controller")
	}
}
