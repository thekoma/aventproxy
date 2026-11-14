package rtsp

import (
	"time"

	"github.com/pion/rtp"
)

const (
	// reorderMaxWait bounds the latency added while waiting for a missing packet.
	// The camera's Wi-Fi link swaps adjacent packets within about 1 ms.
	reorderMaxWait = 40 * time.Millisecond
	// reorderMaxPackets bounds memory and latency during a burst of loss.
	reorderMaxPackets = 256
	// reorderResetDistance treats a larger sequence jump as a new stream.
	reorderResetDistance = 3000
)

type bufferedPacket struct {
	packet  *rtp.Packet
	arrived time.Time
}

// rtpReorderBuffer releases RTP packets in sequence order.
// WebRTC delivers packets in arrival order, but RTSP consumers depacketize
// H.264 in sequence order and drop late fragments, which corrupts the frame.
type rtpReorderBuffer struct {
	started bool
	ssrc    uint32
	next    uint16
	pending map[uint16]bufferedPacket
}

func newRTPReorderBuffer() *rtpReorderBuffer {
	return &rtpReorderBuffer{pending: make(map[uint16]bufferedPacket)}
}

// Push adds one packet and returns the packets that are now ready, in order.
// A missing packet is skipped after reorderMaxWait or when the buffer is full,
// so a real loss still reaches the consumer as a sequence gap.
func (b *rtpReorderBuffer) Push(packet *rtp.Packet, now time.Time) []*rtp.Packet {
	seq := packet.SequenceNumber
	if !b.started {
		b.started, b.ssrc, b.next = true, packet.SSRC, seq
	}

	// A new SSRC is a new sender, wherever its sequence starts: a cloud
	// session that replaced the last one, or a camera that restarted.
	newSender := packet.SSRC != b.ssrc
	distance := int(int16(seq - b.next))
	switch {
	case !newSender && distance < 0 && -distance < reorderResetDistance:
		// Duplicate or too late: the consumer already passed this position.
		return nil
	case newSender || distance < 0 || distance >= reorderResetDistance:
		// The sender restarted its sequence; release what is held and resync.
		ready := b.drain()
		b.ssrc, b.next = packet.SSRC, seq
		b.pending[seq] = bufferedPacket{packet: packet, arrived: now}
		return append(ready, b.release(now)...)
	}

	if _, exists := b.pending[seq]; !exists {
		b.pending[seq] = bufferedPacket{packet: packet, arrived: now}
	}
	return b.release(now)
}

// Release returns the packets a gap held once its wait has passed at now. The
// forwarder calls it when the deadline passes with no packet arriving.
func (b *rtpReorderBuffer) Release(now time.Time) []*rtp.Packet {
	return b.release(now)
}

// Deadline is when the gap now waited on is given up, and whether there is one.
func (b *rtpReorderBuffer) Deadline() (time.Time, bool) {
	if len(b.pending) == 0 {
		return time.Time{}, false
	}
	return b.waitingSince().Add(reorderMaxWait), true
}

// release returns contiguous packets and skips a gap that waited too long.
func (b *rtpReorderBuffer) release(now time.Time) []*rtp.Packet {
	var ready []*rtp.Packet
	for len(b.pending) > 0 {
		if item, ok := b.pending[b.next]; ok {
			delete(b.pending, b.next)
			ready = append(ready, item.packet)
			b.next++
			continue
		}
		if len(b.pending) < reorderMaxPackets && now.Sub(b.waitingSince()) < reorderMaxWait {
			break
		}
		_, b.next = b.oldest()
	}
	return ready
}

// waitingSince is when the first packet still held arrived: the wait for the
// gap in front of it began then, whatever arrived behind it later.
func (b *rtpReorderBuffer) waitingSince() time.Time {
	var first time.Time
	for _, item := range b.pending {
		if first.IsZero() || item.arrived.Before(first) {
			first = item.arrived
		}
	}
	return first
}

// oldest returns the held packet with the lowest sequence distance from next.
func (b *rtpReorderBuffer) oldest() (bufferedPacket, uint16) {
	var best bufferedPacket
	var bestSeq uint16
	bestDistance := -1
	for seq, item := range b.pending {
		distance := int(uint16(seq - b.next))
		if bestDistance < 0 || distance < bestDistance {
			best, bestSeq, bestDistance = item, seq, distance
		}
	}
	return best, bestSeq
}

// drain releases every held packet in sequence order.
func (b *rtpReorderBuffer) drain() []*rtp.Packet {
	var ready []*rtp.Packet
	for len(b.pending) > 0 {
		_, seq := b.oldest()
		ready = append(ready, b.pending[seq].packet)
		delete(b.pending, seq)
		b.next = seq + 1
	}
	return ready
}

// Reset forgets held packets, for example when the last consumer leaves.
func (b *rtpReorderBuffer) Reset() {
	b.started = false
	b.pending = make(map[uint16]bufferedPacket)
}
