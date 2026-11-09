package rtsp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"avent-webrtc-bridge/pkg/core"

	"github.com/pion/rtp"
)

// qualitySwitchTimeout bounds how long a live clarity request may take. The
// camera sends a keyframe with fresh parameter sets every 2-4 s, so a switch
// that took effect shows a new SPS well within this time.
const qualitySwitchTimeout = 15 * time.Second

// pendingQualitySwitch is a live clarity request that has not shown a new SPS yet.
type pendingQualitySwitch struct {
	to       VideoQuality
	baseSPS  []byte
	deadline time.Time
}

// clarityValue maps a quality to the Tuya clarity value (protocol 312).
func clarityValue(q VideoQuality) int {
	if q == VideoQualitySD {
		return 1
	}
	return 0
}

// observeQuality feeds one received video packet to the quality controller
// and notices when the camera's SPS changes after a clarity request.
func (wb *WebRTCBridge) observeQuality(packet *rtp.Packet, now time.Time) {
	wb.quality.Observe(packet.SequenceNumber, now)

	sps := h264SPS(packet.Payload)
	if sps == nil {
		return
	}
	wb.qualityMutex.Lock()
	defer wb.qualityMutex.Unlock()
	if bytes.Equal(sps, wb.lastSPS) {
		return
	}
	wb.lastSPS = append([]byte(nil), sps...)
	switch p := wb.pendingSwitch; {
	case p == nil:
	case p.baseSPS == nil:
		// No SPS was seen before the request; this one is the baseline.
		p.baseSPS = wb.lastSPS
	default:
		core.Logger.Info().Msgf("Camera %s now sends %s video", wb.camera.DeviceName, p.to)
		wb.pendingSwitch = nil
	}
}

// runQualityLoop applies the controller's decisions while the session lives.
func (wb *WebRTCBridge) runQualityLoop() {
	ticker := time.NewTicker(wb.quality.policy.bucket)
	defer ticker.Stop()
	for {
		select {
		case <-wb.ctx.Done():
			return
		case now := <-ticker.C:
			if wb.qualitySwitchExpired(now) {
				return
			}
			if decision, ok := wb.quality.Evaluate(now); ok {
				if !wb.requestQuality(decision, now) {
					return
				}
			}
		}
	}
}

// requestQuality asks the running session for another stream. It returns
// false when it had to fall back to a new camera session.
func (wb *WebRTCBridge) requestQuality(d qualityDecision, now time.Time) bool {
	core.Logger.Warn().Msgf("Video quality for camera %s: %s", wb.camera.DeviceName, d)
	if d.To == VideoQualitySD {
		core.Logger.Info().Msgf("Next HD probe for camera %s in %s", wb.camera.DeviceName, wb.quality.NextProbeDelay())
	}

	wb.qualityMutex.Lock()
	wb.pendingSwitch = &pendingQualitySwitch{to: d.To, baseSPS: wb.lastSPS, deadline: now.Add(qualitySwitchTimeout)}
	wb.qualityMutex.Unlock()

	if wb.controls == nil {
		wb.renegotiateQuality(d.To, fmt.Errorf("no camera signaling client"))
		return false
	}
	if err := wb.controls.SendResolution(clarityValue(d.To)); err != nil {
		wb.renegotiateQuality(d.To, err)
		return false
	}
	return true
}

// qualitySwitchExpired falls back to a new session when the camera ignored a
// live clarity request. It returns true after starting that fallback.
func (wb *WebRTCBridge) qualitySwitchExpired(now time.Time) bool {
	wb.qualityMutex.Lock()
	p := wb.pendingSwitch
	expired := p != nil && now.After(p.deadline)
	if expired {
		wb.pendingSwitch = nil
	}
	wb.qualityMutex.Unlock()
	if !expired {
		return false
	}
	wb.renegotiateQuality(p.to, fmt.Errorf("camera sent no new parameter sets within %s", qualitySwitchTimeout))
	return true
}

// renegotiateQuality ends this session through the normal error path. RTSP
// clients reconnect and the next session offers the controller's current
// stream type directly.
func (wb *WebRTCBridge) renegotiateQuality(to VideoQuality, cause error) {
	core.Logger.Warn().Err(cause).Msgf("Live switch to %s failed for camera %s; starting a new %s session", to, wb.camera.DeviceName, to)
	wb.handleError(fmt.Errorf("video quality %s needs a new camera session: %w", to, cause))
}

// h264SPS returns the SPS NAL unit carried by an RTP payload (single NAL or
// STAP-A), or nil.
func h264SPS(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	switch payload[0] & 0x1f {
	case 7:
		return payload
	case 24:
		for rest := payload[1:]; len(rest) >= 2; {
			size := int(binary.BigEndian.Uint16(rest))
			rest = rest[2:]
			if size == 0 || size > len(rest) {
				return nil
			}
			if rest[0]&0x1f == 7 {
				return rest[:size]
			}
			rest = rest[size:]
		}
	}
	return nil
}
