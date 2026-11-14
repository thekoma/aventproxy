package rtsp

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// VideoQuality selects which camera stream an RTSP path receives.
//
// "hd" and "sd" are fixed. "auto" starts in HD and steps down to SD while the
// camera's video arrives with sustained packet loss, then probes HD again
// with exponential backoff. A lost H.264 packet corrupts the picture until the
// next keyframe, and on a congested Wi-Fi link retransmission (NACK) cannot
// keep up, so a lower bitrate is the only repair.
type VideoQuality string

const (
	VideoQualityHD   VideoQuality = "hd"
	VideoQualitySD   VideoQuality = "sd"
	VideoQualityAuto VideoQuality = "auto"
)

// ParseVideoQuality accepts hd, sd or auto; empty means hd, the upstream default.
func ParseVideoQuality(value string) (VideoQuality, error) {
	switch q := VideoQuality(strings.ToLower(strings.TrimSpace(value))); q {
	case "":
		return VideoQualityHD, nil
	case VideoQualityHD, VideoQualitySD, VideoQualityAuto:
		return q, nil
	default:
		return "", fmt.Errorf("invalid video quality %q: use hd, sd or auto", value)
	}
}

// qualityPolicy holds the thresholds of the adaptive controller.
type qualityPolicy struct {
	bucket          time.Duration // loss accounting granularity
	downgradeWindow time.Duration // loss window that triggers SD
	downgradeLoss   float64       // loss ratio in downgradeWindow that triggers SD
	upgradeWindow   time.Duration // clean window required before an HD probe
	upgradeLoss     float64       // loss ratio below which SD counts as clean
	minPackets      int           // packets a window needs before it is judged
	settle          time.Duration // ignore loss right after a switch
	probeDelay      time.Duration // first HD probe after a downgrade
	maxProbeDelay   time.Duration // backoff cap for repeated failed probes
	stableHD        time.Duration // HD this long resets the backoff
}

// defaultQualityPolicy: any lost packet is visible until the next keyframe
// (2-4 s on the Avent), so 2% over 10 s is already a damaged picture, while a
// single short burst does not flip the stream. HD costs about 2 Mbit/s and SD
// (1280x720) about 0.75 Mbit/s.
var defaultQualityPolicy = qualityPolicy{
	bucket:          time.Second,
	downgradeWindow: 10 * time.Second,
	downgradeLoss:   0.02,
	upgradeWindow:   60 * time.Second,
	upgradeLoss:     0.002,
	minPackets:      250,
	settle:          5 * time.Second,
	probeDelay:      2 * time.Minute,
	maxProbeDelay:   30 * time.Minute,
	stableHD:        10 * time.Minute,
}

type lossBucket struct {
	start    time.Time
	received int
	missing  int
}

// qualityDecision is a requested change of the camera stream.
type qualityDecision struct {
	From, To VideoQuality
	Loss     float64 // loss ratio in the window that decided
	Packets  int     // packets in that window
}

func (d qualityDecision) String() string {
	return fmt.Sprintf("%s -> %s (loss %.1f%% of %d packets)", d.From, d.To, d.Loss*100, d.Packets)
}

// qualityController measures video packet loss by RTP sequence number and
// decides between HD and SD. One controller lives per camera in the server,
// so its state survives a camera session restart.
type qualityController struct {
	mu     sync.Mutex
	policy qualityPolicy

	current VideoQuality
	buckets []lossBucket

	seqStarted bool
	highestSeq uint16
	// missingSince records when each gap was seen. A packet that fills it
	// within reorderMaxWait reaches consumers through the reorder buffer; a
	// later one (for example a slow NACK retransmission) does not, so it
	// stays counted as lost.
	missingSince map[uint16]time.Time

	settleUntil  time.Time
	sdSince      time.Time
	probeDelay   time.Duration
	lastUpgrade  time.Time
	upgradeValid bool
}

func newQualityController(policy qualityPolicy) *qualityController {
	return &qualityController{
		policy:     policy,
		current:    VideoQualityHD,
		probeDelay: policy.probeDelay,
	}
}

// Current returns the stream the camera should send now.
func (q *qualityController) Current() VideoQuality {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.current
}

// StartSession forgets sequence state and loss history for a new camera
// session; its sequence numbers are unrelated to the previous one.
func (q *qualityController) StartSession(now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seqStarted = false
	q.missingSince = nil
	q.buckets = nil
	q.settleUntil = now.Add(q.policy.settle)
}

// Observe records one received video RTP packet, before reordering.
func (q *qualityController) Observe(seq uint16, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()

	b := q.bucketAt(now)
	b.received++
	if !q.seqStarted {
		q.seqStarted, q.highestSeq = true, seq
		return
	}
	distance := int16(seq - q.highestSeq)
	switch {
	case distance > 0 && int(distance) < reorderResetDistance:
		b.missing += int(distance) - 1
		for gap := q.highestSeq + 1; gap != seq; gap++ {
			if len(q.missingSince) >= maxTrackedGaps {
				break // The rest stays lost; the buffer skips that much anyway.
			}
			if q.missingSince == nil {
				q.missingSince = make(map[uint16]time.Time)
			}
			q.missingSince[gap] = now
		}
		q.highestSeq = seq
	case distance <= 0 && int(-distance) < reorderResetDistance:
		// A packet that fills its gap in time is not lost; a duplicate or a
		// packet later than the reorder buffer waits changes nothing.
		if since, ok := q.missingSince[seq]; ok {
			delete(q.missingSince, seq)
			if now.Sub(since) <= reorderMaxWait {
				q.recoverOne()
			}
		}
	default:
		// The sender restarted its sequence.
		q.highestSeq = seq
		q.missingSince = nil
	}
}

// maxTrackedGaps bounds gap bookkeeping, like reorderMaxPackets bounds the buffer.
const maxTrackedGaps = reorderMaxPackets

// forgetExpiredGaps drops gaps that can no longer be filled in time.
func (q *qualityController) forgetExpiredGaps(now time.Time) {
	for seq, since := range q.missingSince {
		if now.Sub(since) > reorderMaxWait {
			delete(q.missingSince, seq)
		}
	}
}

// recoverOne removes one missing packet from the newest bucket that has one.
func (q *qualityController) recoverOne() {
	for i := len(q.buckets) - 1; i >= 0; i-- {
		if q.buckets[i].missing > 0 {
			q.buckets[i].missing--
			return
		}
	}
}

// bucketAt returns the bucket for now and drops buckets older than any window.
func (q *qualityController) bucketAt(now time.Time) *lossBucket {
	if n := len(q.buckets); n == 0 || now.Sub(q.buckets[n-1].start) >= q.policy.bucket {
		q.buckets = append(q.buckets, lossBucket{start: now})
		q.forgetExpiredGaps(now)
	}
	keep := q.policy.upgradeWindow
	if q.policy.downgradeWindow > keep {
		keep = q.policy.downgradeWindow
	}
	drop := 0
	for drop < len(q.buckets)-1 && now.Sub(q.buckets[drop].start) > keep {
		drop++
	}
	q.buckets = q.buckets[drop:]
	return &q.buckets[len(q.buckets)-1]
}

// loss returns the loss ratio and packet count over the last window. It
// reports covered=false until history spans the whole window.
func (q *qualityController) loss(window time.Duration, now time.Time) (ratio float64, packets int, covered bool) {
	if len(q.buckets) == 0 {
		return 0, 0, false
	}
	received, missing := 0, 0
	for _, b := range q.buckets {
		if now.Sub(b.start) <= window {
			received += b.received
			missing += b.missing
		}
	}
	packets = received + missing
	covered = now.Sub(q.buckets[0].start) >= window-q.policy.bucket
	if packets == 0 {
		return 0, 0, covered
	}
	return float64(missing) / float64(packets), packets, covered
}

// Evaluate returns a decision when the stream should change. Call it about
// once per bucket.
func (q *qualityController) Evaluate(now time.Time) (qualityDecision, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if now.Before(q.settleUntil) {
		return qualityDecision{}, false
	}
	p := q.policy

	switch q.current {
	case VideoQualityHD:
		if q.upgradeValid && now.Sub(q.lastUpgrade) >= p.stableHD {
			q.probeDelay, q.upgradeValid = p.probeDelay, false
		}
		ratio, packets, _ := q.loss(p.downgradeWindow, now)
		if packets < p.minPackets || ratio < p.downgradeLoss {
			return qualityDecision{}, false
		}
		if q.upgradeValid {
			// The HD probe failed: wait longer before the next one.
			q.probeDelay *= 2
			if q.probeDelay > p.maxProbeDelay {
				q.probeDelay = p.maxProbeDelay
			}
			q.upgradeValid = false
		}
		q.sdSince = now
		return q.switchTo(VideoQualitySD, ratio, packets, now), true

	case VideoQualitySD:
		if now.Sub(q.sdSince) < q.probeDelay {
			return qualityDecision{}, false
		}
		ratio, packets, covered := q.loss(p.upgradeWindow, now)
		if !covered || packets < p.minPackets || ratio >= p.upgradeLoss {
			return qualityDecision{}, false
		}
		q.lastUpgrade, q.upgradeValid = now, true
		return q.switchTo(VideoQualityHD, ratio, packets, now), true
	}
	return qualityDecision{}, false
}

func (q *qualityController) switchTo(to VideoQuality, ratio float64, packets int, now time.Time) qualityDecision {
	d := qualityDecision{From: q.current, To: to, Loss: ratio, Packets: packets}
	q.current = to
	q.buckets = nil
	q.settleUntil = now.Add(q.policy.settle)
	return d
}

// NextProbeDelay reports the current HD probe backoff, for logging and tests.
func (q *qualityController) NextProbeDelay() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.probeDelay
}
