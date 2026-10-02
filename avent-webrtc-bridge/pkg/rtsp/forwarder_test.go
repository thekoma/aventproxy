package rtsp

import (
	"encoding/binary"
	"io"
	"net"
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
	var packets []*rtp.Packet
	header := make([]byte, 4)
	for len(packets) < n {
		if _, err := io.ReadFull(conn, header); err != nil {
			t.Fatalf("read header: %v", err)
		}
		body := make([]byte, binary.BigEndian.Uint16(header[2:4]))
		if _, err := io.ReadFull(conn, body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		p := &rtp.Packet{}
		if err := p.Unmarshal(body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		packets = append(packets, p)
	}
	return packets
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

	done := make(chan []*rtp.Packet)
	go func() { done <- readInterleaved(t, client, 7) }()
	for i, p := range send {
		if i == 3 {
			time.Sleep(20 * time.Millisecond)
		}
		rf.ForwardVideoPacket(p)
	}
	got := <-done

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
