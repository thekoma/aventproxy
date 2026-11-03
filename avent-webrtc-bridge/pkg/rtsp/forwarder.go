package rtsp

import (
	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/utils"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/rtp"
)

// RTP transport mode (UDP or TCP)
type TransportMode int

const (
	TransportUDP TransportMode = iota
	TransportTCP               // Interleaved
)

type RTPForwarder struct {
	clients map[string]*RTPClient
	mutex   sync.RWMutex

	// RTP session info
	videoSSRC uint32
	audioSSRC uint32

	// Packet count
	firstVideoPacket bool
	firstAudioPacket bool

	// H264 SPS/PPS cache for parameter set injection
	spsPacket *rtp.Packet
	ppsPacket *rtp.Packet

	// Timestamp rebasing. The camera sends one constant RTP timestamp, so
	// timestamps come from the wall clock: per frame for video (every packet
	// of a frame must share one), per packet for audio.
	videoTs        frameClock
	audioTimeStart time.Time
	audioTsStarted bool

	// Packets injected into the video stream (cached SPS/PPS) shift the
	// sequence numbers of everything after them.
	videoSeqOffset uint16

	OnBackchannelAudio func(*rtp.Packet)
}

type RTPClient struct {
	sessionID     string
	transportMode TransportMode

	// UDP transport - Outgoing connections (server -> client)
	videoConn *net.UDPConn // For sending video to client
	audioConn *net.UDPConn // For sending audio to client

	// UDP transport - Client addresses
	videoAddr *net.UDPAddr
	audioAddr *net.UDPAddr

	// UDP transport - Client ports
	videoRTPPort          int // Client's video receiving port
	audioRTPPort          int // Client's audio receiving port
	backchannelClientPort int // Client's backchannel sending port

	// UDP backchannel listeners (server side)
	backchannelListener     *net.UDPConn // Server's RTP listener for backchannel
	backchannelRTCPListener *net.UDPConn // Server's RTCP listener for backchannel
	backchannelServerPort   int          // Server's RTP listening port
	backchannelRTCPPort     int          // Server's RTCP listening port

	// TCP interleaved transport
	tcpConn             net.Conn
	videoRTPChannel     byte
	audioRTPChannel     byte
	backAudioRTPChannel byte

	lastActivity time.Time
}

func NewRTPForwarder() *RTPForwarder {
	return &RTPForwarder{
		clients:          make(map[string]*RTPClient),
		videoSSRC:        0, // Default SSRC for video
		audioSSRC:        1, // Default SSRC for audio
		firstVideoPacket: true,
		firstAudioPacket: true,
	}
}

// dialClientUDP opens a connected socket to a client's media port. The host is
// the source address of the client's RTSP connection, as RFC 2326 places it; a
// client-supplied destination= is not honoured, since that would let anyone
// aim the stream at a third party.
func dialClientUDP(ip net.IP, port int) (*net.UDPConn, error) {
	return net.DialUDP("udp", nil, &net.UDPAddr{IP: ip, Port: port})
}

// remoteIP returns the peer IP of a TCP connection, or nil when it is not one.
func remoteIP(conn net.Conn) net.IP {
	if conn == nil {
		return nil
	}
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		return addr.IP
	}
	return nil
}

// AddUDPClient sends a client's media to ip, the address its RTSP connection
// comes from, or updates the ports of a client already added.
func (rf *RTPForwarder) AddUDPClient(sessionID string, ip net.IP, videoRTPPort, audioRTPPort int) error {
	if ip == nil {
		return fmt.Errorf("no address to send UDP client %s its media", sessionID)
	}

	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	client, exists := rf.clients[sessionID]
	if !exists {
		client = &RTPClient{sessionID: sessionID, transportMode: TransportUDP}
	}
	client.videoRTPPort = videoRTPPort
	client.audioRTPPort = audioRTPPort
	client.lastActivity = time.Now()

	if videoRTPPort > 0 && client.videoConn == nil {
		conn, err := dialClientUDP(ip, videoRTPPort)
		if err != nil {
			if !exists {
				client.close()
			}
			return fmt.Errorf("failed to create video UDP connection: %w", err)
		}
		client.videoAddr = conn.RemoteAddr().(*net.UDPAddr)
		client.videoConn = conn
	}
	if audioRTPPort > 0 && client.audioConn == nil {
		conn, err := dialClientUDP(ip, audioRTPPort)
		if err != nil {
			if !exists {
				client.close()
			}
			return fmt.Errorf("failed to create audio UDP connection: %w", err)
		}
		client.audioAddr = conn.RemoteAddr().(*net.UDPAddr)
		client.audioConn = conn
	}

	rf.clients[sessionID] = client
	core.Logger.Trace().Msgf("Added UDP RTP client %s at %v (video port:%d, audio port:%d)",
		sessionID, ip, videoRTPPort, audioRTPPort)
	return nil
}

// SetupUDPBackchannel opens the server-side RTP/RTCP ports for talkback audio.
// bindIP is the local address of the client's RTSP connection: the ports then
// live on the same interface the client already reached (loopback when the
// RTSP server is bound to localhost) instead of on every interface, where
// anyone could push audio to the camera speaker. nil keeps the old 0.0.0.0.
func (rf *RTPForwarder) SetupUDPBackchannel(sessionID string, clientPort int, bindIP net.IP) (int, error) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	client, exists := rf.clients[sessionID]
	if !exists {
		return 0, fmt.Errorf("client %s not found", sessionID)
	}

	if client.transportMode != TransportUDP {
		return 0, fmt.Errorf("client %s is not using UDP transport", sessionID)
	}

	// Store client's backchannel port
	client.backchannelClientPort = clientPort

	// If listeners already exist, return existing port
	if client.backchannelListener != nil {
		return client.backchannelServerPort, nil
	}

	// Allocate consecutive ports for RTP/RTCP
	portPair, err := utils.DefaultPortAllocator.GetConsecutiveUDPPorts(bindIP, 10)
	if err != nil {
		return 0, fmt.Errorf("failed to allocate UDP ports for backchannel: %v", err)
	}

	// Store listeners and ports
	client.backchannelListener = portPair.RTPListener
	client.backchannelRTCPListener = portPair.RTCPListener
	client.backchannelServerPort = portPair.RTPPort
	client.backchannelRTCPPort = portPair.RTCPPort

	// Start goroutines to handle incoming packets
	go rf.handleUDPBackchannelRTP(sessionID, client.backchannelListener)
	go rf.handleUDPBackchannelRTCP(client.backchannelRTCPListener)

	core.Logger.Trace().Msgf("Setup UDP backchannel for client %s (client ports:%d-%d, server ports:%d-%d)",
		sessionID, clientPort, clientPort+1, portPair.RTPPort, portPair.RTCPPort)

	return portPair.RTPPort, nil
}

func (rf *RTPForwarder) AddTCPClient(sessionID string, conn net.Conn, videoRTPChannel, audioRTPChannel, backAudioRTPChannel byte) error {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	// Check if client already exists, update it
	if existingClient, exists := rf.clients[sessionID]; exists {
		core.Logger.Trace().Msgf("TCP client %s already exists, updating channels (video:%d->%d, audio:%d->%d, back:%d->%d)",
			sessionID, existingClient.videoRTPChannel, videoRTPChannel, existingClient.audioRTPChannel, audioRTPChannel, existingClient.backAudioRTPChannel, backAudioRTPChannel)
		existingClient.videoRTPChannel = videoRTPChannel
		existingClient.audioRTPChannel = audioRTPChannel
		existingClient.backAudioRTPChannel = backAudioRTPChannel
		existingClient.lastActivity = time.Now()
		return nil
	}

	client := &RTPClient{
		sessionID:           sessionID,
		transportMode:       TransportTCP,
		tcpConn:             conn,
		videoRTPChannel:     videoRTPChannel,
		audioRTPChannel:     audioRTPChannel,
		backAudioRTPChannel: backAudioRTPChannel,
		lastActivity:        time.Now(),
	}

	rf.clients[sessionID] = client

	core.Logger.Trace().Msgf("Added TCP RTP client %s (video channel:%d, audio channel:%d, back audio channel:%d)",
		sessionID, videoRTPChannel, audioRTPChannel, backAudioRTPChannel)
	return nil
}

// close releases the sockets the forwarder opened for a client. The RTSP
// connection of a TCP client belongs to the server and is left alone.
func (c *RTPClient) close() {
	for _, conn := range []*net.UDPConn{c.videoConn, c.audioConn, c.backchannelListener, c.backchannelRTCPListener} {
		if conn != nil {
			_ = conn.Close()
		}
	}
}

func (rf *RTPForwarder) RemoveClient(sessionID string) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	if client, exists := rf.clients[sessionID]; exists {
		client.close()
		delete(rf.clients, sessionID)
		core.Logger.Trace().Msgf("Removed RTP client %s", sessionID)
	}
}

// tcpWriteTimeout bounds one interleaved write. Packets go to every client in
// turn, so a client that stops reading would otherwise stall all of them.
var tcpWriteTimeout = 2 * time.Second

// isDeadClientError reports whether a client is gone for good rather than hit
// by a transient failure worth logging. A UDP client that closed its socket
// answers with ICMP port unreachable, which a connected socket reports as
// ECONNREFUSED on every later packet; a TCP client that stopped reading runs
// into the write deadline.
func isDeadClientError(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, os.ErrDeadlineExceeded)
}

// send writes one marshalled packet to a client on the video or audio track.
func (rf *RTPForwarder) send(client *RTPClient, video bool, data []byte) error {
	if client.transportMode == TransportTCP {
		if client.tcpConn == nil {
			return nil
		}
		channel := client.audioRTPChannel
		if video {
			channel = client.videoRTPChannel
		}
		return rf.sendInterleavedRTP(client.tcpConn, channel, data)
	}
	conn := client.audioConn
	if video {
		conn = client.videoConn
	}
	if conn == nil {
		return nil
	}
	_, err := conn.Write(data)
	return err
}

// forward sends one marshalled packet to every client and drops the ones that
// are gone. Callers hold rf.mutex for writing.
func (rf *RTPForwarder) forward(data []byte, video bool) {
	track := "audio"
	if video {
		track = "video"
	}
	var dead []string
	for sessionID, client := range rf.clients {
		client.lastActivity = time.Now()
		if err := rf.send(client, video, data); err != nil {
			if isDeadClientError(err) {
				dead = append(dead, sessionID)
			} else {
				core.Logger.Error().Err(err).Msgf("Error forwarding %s packet to client %s", track, sessionID)
			}
			continue
		}
		if video && rf.firstVideoPacket {
			rf.firstVideoPacket = false
			core.Logger.Trace().Msgf("Sent the first video packet to client %s", sessionID)
		} else if !video && rf.firstAudioPacket {
			rf.firstAudioPacket = false
			core.Logger.Trace().Msgf("Sent the first audio packet to client %s", sessionID)
		}
	}

	for _, sessionID := range dead {
		client := rf.clients[sessionID]
		core.Logger.Info().Msgf("Removing dead %s client %s", track, sessionID)
		client.close()
		// A stalled or broken interleaved stream cannot be resumed: the
		// client has to reconnect, so its RTSP connection goes too.
		if client.tcpConn != nil {
			_ = client.tcpConn.Close()
		}
		delete(rf.clients, sessionID)
	}
}

func (rf *RTPForwarder) getNALType(packet *rtp.Packet) byte {
	if len(packet.Payload) == 0 {
		return 0
	}
	nalType := packet.Payload[0] & 0x1F
	if nalType == 28 && len(packet.Payload) > 1 {
		// FU-A: real NAL type is in second byte, only on start bit
		if packet.Payload[1]&0x80 != 0 {
			return packet.Payload[1] & 0x1F
		}
		return 0
	}
	return nalType
}

func (rf *RTPForwarder) cacheSTAP(packet *rtp.Packet) {
	// STAP-A (type 24): contains multiple NAL units including SPS/PPS
	payload := packet.Payload[1:] // skip STAP-A header byte
	for len(payload) > 2 {
		nalSize := int(payload[0])<<8 | int(payload[1])
		payload = payload[2:]
		if nalSize > len(payload) {
			break
		}
		nalType := payload[0] & 0x1F
		if nalType == 7 || nalType == 8 {
			rf.cacheNAL(packet, nalType)
		}
		payload = payload[nalSize:]
	}
}

func (rf *RTPForwarder) cacheNAL(packet *rtp.Packet, nalType byte) {
	switch nalType {
	case 7:
		rf.spsPacket = packet.Clone()
	case 8:
		rf.ppsPacket = packet.Clone()
	}
}

// ForwardVideoPacket restamps an H.264 RTP packet from the camera (one
// timestamp per frame, sequence numbers shifted past injected packets),
// injects the cached SPS/PPS ahead of each IDR, and sends it to every client.
func (rf *RTPForwarder) ForwardVideoPacket(packet *rtp.Packet) {
	// Write lock: this mutates forwarder state, and forwardVideoData may
	// remove dead clients from the map.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	if len(rf.clients) == 0 {
		return
	}

	packet.Timestamp = rf.videoTs.timestamp(packet.Marker, 90000)
	packet.SequenceNumber += rf.videoSeqOffset

	nalType := rf.getNALType(packet)

	// Cache SPS (7), PPS (8), and STAP-A (24) which may contain both
	switch nalType {
	case 7:
		rf.spsPacket = packet.Clone()
	case 8:
		rf.ppsPacket = packet.Clone()
	case 24:
		rf.cacheSTAP(packet)
	}

	// Before IDR keyframe (5), inject cached SPS/PPS. They belong to the
	// IDR's access unit, so they take its timestamp and get their own
	// sequence numbers instead of replaying the stale cached ones.
	if nalType == 5 && rf.spsPacket != nil && rf.ppsPacket != nil {
		for _, cached := range []*rtp.Packet{rf.spsPacket, rf.ppsPacket} {
			injected := cached.Clone()
			injected.Timestamp = packet.Timestamp
			injected.SequenceNumber = packet.SequenceNumber
			injected.Marker = false
			rf.forwardVideoData(injected)
			packet.SequenceNumber++
			rf.videoSeqOffset++
		}
	}

	rf.forwardVideoData(packet)
}

func (rf *RTPForwarder) forwardVideoData(packet *rtp.Packet) {
	data, err := packet.Marshal()
	if err != nil {
		core.Logger.Error().Err(err).Msg("Error marshaling video RTP packet")
		return
	}
	rf.forward(data, true)
}

// ForwardAudioPacket restamps a PCMU RTP packet from the wall clock and sends
// it to every client.
func (rf *RTPForwarder) ForwardAudioPacket(packet *rtp.Packet) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	if len(rf.clients) == 0 {
		return
	}

	// Rebase timestamp to wall clock (8kHz for PCMU)
	if !rf.audioTsStarted {
		rf.audioTimeStart = time.Now()
		rf.audioTsStarted = true
	}
	packet.Timestamp = rtpTicks(time.Since(rf.audioTimeStart), 8000)

	data, err := packet.Marshal()
	if err != nil {
		core.Logger.Error().Err(err).Msg("Error marshaling audio RTP packet")
		return
	}
	rf.forward(data, false)
}

func (rf *RTPForwarder) Stop() {
	// Reset SSRCs
	rf.videoSSRC = 0
	rf.audioSSRC = 1

	// Reset first packet flags
	rf.firstVideoPacket = true
	rf.firstAudioPacket = true

	// Clear all clients
	for sessionID := range rf.clients {
		rf.RemoveClient(sessionID)
	}

	core.Logger.Trace().Msg("RTPForwarder stopped and all clients cleared")
}

func (rf *RTPForwarder) GetClientCount() int {
	rf.mutex.RLock()
	defer rf.mutex.RUnlock()
	return len(rf.clients)
}

func (rf *RTPForwarder) CleanupInactiveClients(timeout time.Duration) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	now := time.Now()
	var toRemove []string

	for sessionID, client := range rf.clients {
		if now.Sub(client.lastActivity) > timeout {
			toRemove = append(toRemove, sessionID)
		}
	}

	for _, sessionID := range toRemove {
		if client, exists := rf.clients[sessionID]; exists {
			client.close()
			delete(rf.clients, sessionID)
			core.Logger.Trace().Msgf("Cleaned up inactive RTP client %s", sessionID)
		}
	}
}

func (rf *RTPForwarder) handleUDPBackchannelRTP(sessionID string, listener *net.UDPConn) {
	defer listener.Close()

	buffer := make([]byte, 1500)

	for {
		n, _, err := listener.ReadFromUDP(buffer)
		if err != nil {
			if !strings.Contains(err.Error(), "closed") {
				core.Logger.Error().Err(err).Msgf("Error reading UDP RTP backchannel for client %s", sessionID)
			}
			break
		}

		// Parse RTP packet
		packet := &rtp.Packet{}
		if err := packet.Unmarshal(buffer[:n]); err != nil {
			continue
		}

		// Forward to WebRTC bridge
		if rf.OnBackchannelAudio != nil {
			rf.OnBackchannelAudio(packet)
		}
	}
}

func (rf *RTPForwarder) handleUDPBackchannelRTCP(listener *net.UDPConn) {
	defer listener.Close()

	buffer := make([]byte, 1500)

	for {
		_, _, err := listener.ReadFromUDP(buffer)
		if err != nil {
			// Ignore
			break
		}

		// Simply discard RTCP packets
	}
}

func (rf *RTPForwarder) sendInterleavedRTP(conn net.Conn, channel byte, rtpData []byte) error {
	// Interleaved format: $ + channel + length(2 bytes) + RTP data
	header := make([]byte, 4)
	header[0] = '$'                     // Magic byte
	header[1] = channel                 // Channel number
	header[2] = byte(len(rtpData) >> 8) // Length high byte
	header[3] = byte(len(rtpData))      // Length low byte

	// Send header + data in one write to avoid fragmentation
	fullPacket := append(header, rtpData...)

	// The deadline is lifted afterwards: the RTSP responses written on this
	// connection must not inherit an expired one once media stops.
	if err := conn.SetWriteDeadline(time.Now().Add(tcpWriteTimeout)); err != nil {
		return err
	}
	_, err := conn.Write(fullPacket)
	if err == nil {
		err = conn.SetWriteDeadline(time.Time{})
	}
	return err
}

// frameClock stamps video packets with the wall clock, once per frame. A frame
// runs from its first packet to the one carrying the RTP marker bit; if that
// packet is lost, a frame is closed anyway after maxFrameSpan so the next one
// does not inherit its timestamp.
type frameClock struct {
	started   bool
	wallStart time.Time
	open      bool
	frameWall time.Time
	frameTs   uint32
}

const maxFrameSpan = 100 * time.Millisecond

// timestamp returns the RTP timestamp for the next video packet. marker is
// the packet's RTP marker bit, which ends the current frame.
func (c *frameClock) timestamp(marker bool, clockRate int) uint32 {
	now := time.Now()
	if !c.started {
		c.started = true
		c.wallStart = now
		c.frameWall = now
	} else if !c.open || now.Sub(c.frameWall) > maxFrameSpan {
		ts := rtpTicks(now.Sub(c.wallStart), clockRate)
		// Keep frames strictly increasing; the int32 difference survives wraparound.
		if int32(ts-c.frameTs) <= 0 {
			ts = c.frameTs + 1
		}
		c.frameTs = ts
		c.frameWall = now
	}
	c.open = !marker
	return c.frameTs
}

// rtpTicks converts elapsed time to RTP clock ticks, wrapping at 2^32 the way
// RTP timestamps do. It uses integer arithmetic because the Go spec leaves a
// float-to-uint32 conversion past the uint32 range implementation-dependent,
// and at 90 kHz the clock passes that range after about 13 hours.
func rtpTicks(elapsed time.Duration, clockRate int) uint32 {
	secs := uint64(elapsed / time.Second)
	frac := uint64(elapsed % time.Second)
	return uint32(secs*uint64(clockRate) + frac*uint64(clockRate)/uint64(time.Second))
}
