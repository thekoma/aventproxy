package rtsp

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func seqs(packets []*rtp.Packet) []uint16 {
	out := make([]uint16, 0, len(packets))
	for _, p := range packets {
		out = append(out, p.SequenceNumber)
	}
	return out
}

func pkt(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts}, Payload: []byte{0x41, 0}}
}

// TestReorderBufferRestoresSwappedPackets covers the camera's Wi-Fi reordering.
func TestReorderBufferRestoresSwappedPackets(t *testing.T) {
	b := newRTPReorderBuffer()
	now := time.Now()
	var got []*rtp.Packet
	for _, seq := range []uint16{10, 12, 11, 14, 13, 15} {
		got = append(got, b.Push(pkt(seq, 0), now)...)
	}
	want := []uint16{10, 11, 12, 13, 14, 15}
	if g := seqs(got); len(g) != len(want) {
		t.Fatalf("got %v, want %v", g, want)
	} else {
		for i := range want {
			if g[i] != want[i] {
				t.Fatalf("got %v, want %v", g, want)
			}
		}
	}
}

// TestReorderBufferSkipsRealLossAfterWait keeps latency bounded and exposes the gap.
func TestReorderBufferSkipsRealLossAfterWait(t *testing.T) {
	b := newRTPReorderBuffer()
	now := time.Now()
	b.Push(pkt(1, 0), now)
	if got := b.Push(pkt(3, 0), now); len(got) != 0 {
		t.Fatalf("released %v before the wait expired", seqs(got))
	}
	got := b.Push(pkt(4, 0), now.Add(reorderMaxWait))
	if g := seqs(got); len(g) != 2 || g[0] != 3 || g[1] != 4 {
		t.Fatalf("got %v, want [3 4] after skipping lost packet 2", g)
	}
	if late := b.Push(pkt(2, 0), now.Add(reorderMaxWait)); len(late) != 0 {
		t.Fatalf("forwarded packet %v after the consumer passed it", seqs(late))
	}
}

// TestReorderBufferWrapsAndResyncs covers 16-bit wraparound and a sender restart.
func TestReorderBufferWrapsAndResyncs(t *testing.T) {
	b := newRTPReorderBuffer()
	now := time.Now()
	var got []*rtp.Packet
	for _, seq := range []uint16{65534, 0, 65535, 1} {
		got = append(got, b.Push(pkt(seq, 0), now)...)
	}
	if g := seqs(got); len(g) != 4 || g[0] != 65534 || g[1] != 65535 || g[2] != 0 || g[3] != 1 {
		t.Fatalf("wraparound order %v", g)
	}
	if g := seqs(b.Push(pkt(30000, 0), now)); len(g) != 1 || g[0] != 30000 {
		t.Fatalf("restart resync %v", g)
	}
}

// TestReorderBufferResyncsOnNewSSRC takes a new sender as a new stream, even
// one whose sequence starts just behind the old position, which the sequence
// alone would take for late packets and drop until it passed the old one.
func TestReorderBufferResyncsOnNewSSRC(t *testing.T) {
	b := newRTPReorderBuffer()
	now := time.Now()
	for seq := uint16(5000); seq < 5010; seq++ {
		b.Push(pkt(seq, 0), now)
	}
	p := pkt(4000, 0)
	p.SSRC = 2
	if g := seqs(b.Push(p, now)); len(g) != 1 || g[0] != 4000 {
		t.Fatalf("new sender %v, want [4000]", g)
	}
	if g := seqs(b.Push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 4001, SSRC: 2}}, now)); len(g) != 1 || g[0] != 4001 {
		t.Fatalf("new sender's next packet %v, want [4001]", g)
	}
}

// TestReorderBufferWaitCountsFromFirstHeld bounds a gap's wait by the first
// packet held behind it, not by one that arrived later, and exposes the
// deadline so the forwarder can release the gap with no further packet.
func TestReorderBufferWaitCountsFromFirstHeld(t *testing.T) {
	b := newRTPReorderBuffer()
	now := time.Now()
	if _, waiting := b.Deadline(); waiting {
		t.Fatal("waiting with nothing held")
	}
	b.Push(pkt(1, 0), now)
	b.Push(pkt(4, 0), now)                          // 2 and 3 missing: 4 held from now
	b.Push(pkt(3, 0), now.Add(30*time.Millisecond)) // 3 arrives, 2 still missing
	deadline, waiting := b.Deadline()
	if !waiting || !deadline.Equal(now.Add(reorderMaxWait)) {
		t.Fatalf("deadline %v (waiting %v), want %v", deadline, waiting, now.Add(reorderMaxWait))
	}
	if g := seqs(b.Release(now.Add(reorderMaxWait - time.Millisecond))); len(g) != 0 {
		t.Fatalf("released %v before the deadline", g)
	}
	if g := seqs(b.Release(now.Add(reorderMaxWait))); len(g) != 2 || g[0] != 3 || g[1] != 4 {
		t.Fatalf("released %v at the deadline, want [3 4]", g)
	}
	if _, waiting := b.Deadline(); waiting {
		t.Fatal("still waiting with nothing held")
	}
}

// TestReorderBufferBoundsMemory releases held packets when the window is full.
func TestReorderBufferBoundsMemory(t *testing.T) {
	b := newRTPReorderBuffer()
	now := time.Now()
	b.Push(pkt(0, 0), now)
	released := 0
	for seq := uint16(2); seq < 2+reorderMaxPackets+10; seq++ {
		released += len(b.Push(pkt(seq, 0), now))
	}
	if len(b.pending) > reorderMaxPackets {
		t.Fatalf("held %d packets, limit %d", len(b.pending), reorderMaxPackets)
	}
	if released == 0 {
		t.Fatal("a full window must release packets past the gap")
	}
}

// TestForwardVideoReleasesAGapWithoutAnotherPacket: when the lost packet is
// the last one before a pause (say, the one that ended a frame), what is held
// behind it still goes out after reorderMaxWait, not with the next frame.
func TestForwardVideoReleasesAGapWithoutAnotherPacket(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	rf := NewRTPForwarder()
	defer rf.Stop()
	if err := rf.AddTCPClient("s1", server, 0, 2, 4); err != nil {
		t.Fatalf("add client: %v", err)
	}
	wait := collect(t, client, 2)
	rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: 30, Marker: true}, Payload: []byte{0x41, 0x9a}})
	rf.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: 32, Marker: true}, Payload: []byte{0x41, 0x9a}}) // 31 lost
	got := wait()
	if got[0].SequenceNumber != 30 || got[1].SequenceNumber != 32 {
		t.Fatalf("got seq %d %d, want 30 32", got[0].SequenceNumber, got[1].SequenceNumber)
	}
}

// TestForwardVideoReordersSwappedFragments covers Wi-Fi reordering inside one frame:
// the late fragment must not be dropped by consumers or open a new frame timestamp.
func TestForwardVideoReordersSwappedFragments(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	rf := NewRTPForwarder()
	if err := rf.AddTCPClient("s1", server, 0, 2, 4); err != nil {
		t.Fatalf("add client: %v", err)
	}

	send := []*rtp.Packet{
		{Header: rtp.Header{SequenceNumber: 20}, Payload: []byte{0x7c, 0x81}},               // FU-A start
		{Header: rtp.Header{SequenceNumber: 22, Marker: true}, Payload: []byte{0x7c, 0x41}}, // FU-A end, early
		{Header: rtp.Header{SequenceNumber: 21}, Payload: []byte{0x7c, 0x01}},               // FU-A middle, late
		{Header: rtp.Header{SequenceNumber: 23, Marker: true}, Payload: []byte{0x41, 0x9a}}, // next frame
	}

	done := make(chan []*rtp.Packet)
	go func() { done <- readInterleaved(t, client, 4) }()
	for _, p := range send {
		rf.ForwardVideoPacket(p)
	}
	got := <-done

	for i, want := range []uint16{20, 21, 22, 23} {
		if got[i].SequenceNumber != want {
			t.Fatalf("packet %d: seq %d, want %d", i, got[i].SequenceNumber, want)
		}
	}
	if got[0].Timestamp != got[1].Timestamp || got[1].Timestamp != got[2].Timestamp {
		t.Errorf("one frame split across timestamps %d %d %d", got[0].Timestamp, got[1].Timestamp, got[2].Timestamp)
	}
	if int32(got[3].Timestamp-got[2].Timestamp) <= 0 {
		t.Errorf("next frame ts %d, want after %d", got[3].Timestamp, got[2].Timestamp)
	}
}
