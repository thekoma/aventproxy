package rtsp

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// rtspPeer is the client end of an RTSP control connection to the server.
type rtspPeer struct {
	conn   net.Conn
	reader *bufio.Reader
}

// readResponse reads one RTSP response and returns its status line.
func (p *rtspPeer) readResponse(t *testing.T) string {
	t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	status, err := p.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	for {
		line, err := p.reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" {
			return strings.TrimSpace(status)
		}
	}
}

// newProtocolClient attaches an RTSP client to a fresh stream and returns the
// server's handle for it plus the peer's end of the connection.
func newProtocolClient(t *testing.T) (*RTSPServer, *RTSPClient, *rtspPeer) {
	t.Helper()
	server, stream := newTestCameraStream(t)
	peerEnd, serverEnd := net.Pipe()
	t.Cleanup(func() {
		peerEnd.Close()
		serverEnd.Close()
	})
	client := &RTSPClient{conn: serverEnd, session: "s1", stream: stream, audioRTPChannel: 2, backAudioRTPChannel: 4}
	return server, client, &rtspPeer{conn: peerEnd, reader: bufio.NewReader(peerEnd)}
}

func setupRequest(track, transport string) *RTSPRequest {
	return &RTSPRequest{
		Method:  "SETUP",
		URL:     "rtsp://127.0.0.1:8554/cam/" + track,
		Headers: map[string]string{"Transport": transport, "Session": "s1"},
	}
}

// Interleaved media shares the socket the client is still setting its tracks
// up on: anything sent before PLAY lands where the next SETUP response belongs
// (fork a336350, which lost the audio track this way).
func TestInterleavedMediaWaitsForPlay(t *testing.T) {
	server, client, peer := newProtocolClient(t)
	forwarder := client.stream.forwarder

	for _, track := range []string{"video", "audio"} {
		go server.handleSetup(client, setupRequest(track, "RTP/AVP/TCP;unicast;interleaved="+map[string]string{"video": "0-1", "audio": "2-3"}[track]))
		if status := peer.readResponse(t); status != "RTSP/1.0 200 OK" {
			t.Fatalf("SETUP %s: %q", track, status)
		}
		if n := forwarder.GetClientCount(); n != 0 {
			t.Fatalf("after SETUP %s the forwarder already sends to %d client(s)", track, n)
		}
	}

	go server.handlePlay(client, &RTSPRequest{Method: "PLAY", Headers: map[string]string{"Session": "s1"}})
	if status := peer.readResponse(t); status != "RTSP/1.0 200 OK" {
		t.Fatalf("PLAY: %q", status)
	}
	deadline := time.Now().Add(2 * time.Second)
	for forwarder.GetClientCount() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("PLAY did not attach the client to the forwarder")
		}
		time.Sleep(5 * time.Millisecond)
	}

	go forwarder.ForwardAudioPacket(&rtp.Packet{Header: rtp.Header{PayloadType: 0}, Payload: []byte{0xff, 0xff}})
	_ = peer.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := readInterleaved(t, peer.conn, 1)
	if got[0].PayloadType != 0 {
		t.Fatalf("got payload type %d on the audio channel", got[0].PayloadType)
	}
}

// UDP media also waits for PLAY: SETUP registers the client (the UDP
// backchannel SETUP needs that entry) but sends it nothing yet.
func TestUDPMediaWaitsForPlay(t *testing.T) {
	server, stream := newTestCameraStream(t)
	ln, _ := listenOnEphemeral(t)
	defer ln.Close()
	peerEnd, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer peerEnd.Close()
	serverEnd, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer serverEnd.Close()
	client := &RTSPClient{conn: serverEnd, session: "s1", stream: stream}
	peer := &rtspPeer{conn: peerEnd, reader: bufio.NewReader(peerEnd)}

	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	port := sink.LocalAddr().(*net.UDPAddr).Port
	var seq uint16 // a repeated sequence number would be dropped as a duplicate
	received := func() bool {
		seq++
		stream.forwarder.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Marker: true}, Payload: []byte{0x41, 0x9a}})
		_ = sink.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, _, err := sink.ReadFromUDP(make([]byte, 1500))
		return err == nil
	}

	go server.handleSetup(client, setupRequest("video", fmt.Sprintf("RTP/AVP;unicast;client_port=%d-%d", port, port+1)))
	if status := peer.readResponse(t); status != "RTSP/1.0 200 OK" {
		t.Fatalf("SETUP: %q", status)
	}
	if received() {
		t.Fatal("UDP media arrived before PLAY")
	}

	go server.handlePlay(client, &RTSPRequest{Method: "PLAY", Headers: map[string]string{"Session": "s1"}})
	if status := peer.readResponse(t); status != "RTSP/1.0 200 OK" {
		t.Fatalf("PLAY: %q", status)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !received() {
		if time.Now().After(deadline) {
			t.Fatal("no UDP media after PLAY")
		}
	}
}
