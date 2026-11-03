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

	// Packet count
	firstVideoPacket bool
	firstAudioPacket bool

	// H264 SPS/PPS cache for parameter set injection
	spsPacket *rtp.Packet
	ppsPacket *rtp.Packet

	// Timestamp rebasing. The camera sends one constant RTP timestamp, so
	// video timestamps come from the wall clock, one per frame (every packet
	// of a frame must share one), and audio ones count samples.
	videoTs frameClock
	audioTs audioClock

	// Packets injected into the video stream (cached SPS/PPS) shift the
	// sequence numbers of everything after them.
	videoSeqOffset uint16

	// backchannelSink takes talkback audio from the clients; see
	// SetBackchannelSink.
	backchannelSink func(*rtp.Packet)
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

	// failing is set while sends to this client fail with an error that is
	// not a dead client's, so that streak is logged once, not per packet.
	failing bool
}

func NewRTPForwarder() *RTPForwarder {
	return &RTPForwarder{
		clients:          make(map[string]*RTPClient),
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
// turn with the forwarder locked, so a client that stops reading would
// otherwise stall all of them, and the LAN source with them. The bound is the
// trade-off: a stalled viewer can still hold the others up for this long
// before it is dropped, while a shorter one drops viewers on a busy host for
// a hiccup (a dropped Frigate ffmpeg is a recording gap). go2rtc gives up on
// a silent source after 5 s, which is as long as this can afford to be.
var tcpWriteTimeout = 5 * time.Second

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
			} else if !client.failing {
				// A remote client off the network (EHOSTUNREACH, ENETUNREACH)
				// fails every packet until it comes back or its session ends.
				client.failing = true
				core.Logger.Warn().Err(err).Msgf("Cannot send %s to client %s, not logging again until it recovers", track, sessionID)
			}
			continue
		}
		if client.failing {
			client.failing = false
			core.Logger.Info().Msgf("Sending to client %s again", sessionID)
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

	// The clock and the parameter-set cache run whether or not anyone
	// watches: the first viewer still needs the SPS and PPS that came before.
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

	if len(rf.clients) == 0 {
		return
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

// ForwardAudioPacket restamps a G.711 RTP packet by its samples and sends it
// to every client.
func (rf *RTPForwarder) ForwardAudioPacket(packet *rtp.Packet) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	// G.711 (PCMU 0, PCMA 8) carries one sample per byte at 8 kHz. Anything
	// else is stamped from the wall clock.
	samples := -1
	if packet.PayloadType == 0 || packet.PayloadType == 8 {
		samples = len(packet.Payload)
	}
	packet.Timestamp = rf.audioTs.timestamp(samples, 8000)

	if len(rf.clients) == 0 {
		return
	}

	data, err := packet.Marshal()
	if err != nil {
		core.Logger.Error().Err(err).Msg("Error marshaling audio RTP packet")
		return
	}
	rf.forward(data, false)
}

// Stop drops every client and forgets the media state, so the stream that
// owns the forwarder starts its next session clean.
func (rf *RTPForwarder) Stop() {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	for sessionID, client := range rf.clients {
		client.close()
		delete(rf.clients, sessionID)
	}
	// No client survives a stop, so the clocks may start over too.
	rf.resetSourceLocked()
	rf.videoTs, rf.audioTs = frameClock{}, audioClock{}

	core.Logger.Trace().Msg("RTPForwarder stopped and all clients cleared")
}

// ResetMedia forgets what belonged to the source that fed the forwarder so
// far (its parameter sets and sequence offset), keeping the clients, as when
// the cloud takes over from a failed LAN attempt. The clocks keep running:
// they follow the wall clock, so timestamps stay monotonic for the clients
// that stay attached across the change of source.
func (rf *RTPForwarder) ResetMedia() {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()
	rf.resetSourceLocked()
	rf.videoTs.open = false // the old source's last picture ends here
}

func (rf *RTPForwarder) resetSourceLocked() {
	rf.firstVideoPacket = true
	rf.firstAudioPacket = true
	rf.spsPacket, rf.ppsPacket = nil, nil
	rf.videoSeqOffset = 0
}

// SetBackchannelSink sets where talkback audio from the clients goes: the
// stream's current WebRTC bridge.
func (rf *RTPForwarder) SetBackchannelSink(sink func(*rtp.Packet)) {
	rf.mutex.Lock()
	rf.backchannelSink = sink
	rf.mutex.Unlock()
}

// ForwardBackchannel hands one talkback packet from a client to the sink.
func (rf *RTPForwarder) ForwardBackchannel(packet *rtp.Packet) {
	rf.mutex.RLock()
	sink := rf.backchannelSink
	rf.mutex.RUnlock()
	if sink != nil {
		sink(packet)
	}
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

		rf.ForwardBackchannel(packet)
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

// audioClock stamps audio packets by the samples they carry, which is what a
// continuous stream needs: the timestamp says when the audio was recorded,
// not when its packet arrived. Packets reach the bridge in bursts (WebRTC
// jitter, KCP retransmits), and wall-clock stamps gave a burst nearly one
// timestamp. When the count falls behind the wall clock by more than
// audioMaxLag (a silence, or packets lost on the way) it catches up instead
// of pretending no time passed. It never steps back: timestamps that ran
// ahead after a burst stay ahead.
type audioClock struct {
	started   bool
	wallStart time.Time
	next      uint32 // timestamp of the next packet
}

const audioMaxLag = time.Second

// timestamp returns the RTP timestamp of a packet carrying samples samples, or
// the wall-clock time when samples is negative (an unknown codec).
func (c *audioClock) timestamp(samples, clockRate int) uint32 {
	now := time.Now()
	if !c.started {
		c.started = true
		c.wallStart = now
		c.next = 0
	}
	wall := rtpTicks(now.Sub(c.wallStart), clockRate)
	maxLag := int32(rtpTicks(audioMaxLag, clockRate))
	if samples < 0 || int32(wall-c.next) > maxLag {
		c.next = wall
	}
	ts := c.next
	if samples > 0 {
		c.next += uint32(samples)
	}
	return ts
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
