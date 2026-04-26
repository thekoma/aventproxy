package rtsp

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/utils"

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

	// Timestamp rebasing
	videoTimeStart  time.Time
	videoSeqStart   uint16
	videoTsStarted  bool
	audioTimeStart  time.Time
	audioSeqStart   uint16
	audioTsStarted  bool

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

func (rf *RTPForwarder) AddUDPClient(sessionID string, videoRTPPort, audioRTPPort int) error {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	// Check if client already exists
	if client, exists := rf.clients[sessionID]; exists {
		// Update existing client with new ports
		client.videoRTPPort = videoRTPPort
		client.audioRTPPort = audioRTPPort
		client.lastActivity = time.Now()

		// Create new connections if needed
		if videoRTPPort > 0 && client.videoConn == nil {
			videoAddr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("localhost:%d", videoRTPPort))
			videoConn, _ := net.DialUDP("udp", nil, videoAddr)
			client.videoAddr = videoAddr
			client.videoConn = videoConn
		}

		if audioRTPPort > 0 && client.audioConn == nil {
			audioAddr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("localhost:%d", audioRTPPort))
			audioConn, _ := net.DialUDP("udp", nil, audioAddr)
			client.audioAddr = audioAddr
			client.audioConn = audioConn
		}

		return nil
	}

	client := &RTPClient{
		sessionID:     sessionID,
		transportMode: TransportUDP,
		videoRTPPort:  videoRTPPort,
		audioRTPPort:  audioRTPPort,
		lastActivity:  time.Now(),
	}

	// Create video connection if port provided
	if videoRTPPort > 0 {
		videoAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("localhost:%d", videoRTPPort))
		if err != nil {
			return fmt.Errorf("failed to resolve video UDP address: %v", err)
		}

		videoConn, err := net.DialUDP("udp", nil, videoAddr)
		if err != nil {
			return fmt.Errorf("failed to create video UDP connection: %v", err)
		}

		client.videoAddr = videoAddr
		client.videoConn = videoConn
	}

	// Create audio connection if port provided
	if audioRTPPort > 0 {
		audioAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("localhost:%d", audioRTPPort))
		if err != nil {
			if client.videoConn != nil {
				client.videoConn.Close()
			}
			return fmt.Errorf("failed to resolve audio UDP address: %v", err)
		}

		audioConn, err := net.DialUDP("udp", nil, audioAddr)
		if err != nil {
			if client.videoConn != nil {
				client.videoConn.Close()
			}
			return fmt.Errorf("failed to create audio UDP connection: %v", err)
		}

		client.audioAddr = audioAddr
		client.audioConn = audioConn
	}

	rf.clients[sessionID] = client

	core.Logger.Trace().Msgf("Added UDP RTP client %s (video port:%d, audio port:%d)",
		sessionID, videoRTPPort, audioRTPPort)
	return nil
}

func (rf *RTPForwarder) SetupUDPBackchannel(sessionID string, clientPort int) (int, error) {
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
	portPair, err := utils.DefaultPortAllocator.GetConsecutiveUDPPorts(nil, 10)
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

func (rf *RTPForwarder) RemoveClient(sessionID string) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	if client, exists := rf.clients[sessionID]; exists {
		if client.transportMode == TransportUDP {
			if client.videoConn != nil {
				client.videoConn.Close()
			}
			if client.audioConn != nil {
				client.audioConn.Close()
			}
			if client.backchannelListener != nil {
				client.backchannelListener.Close()
			}
			if client.backchannelRTCPListener != nil {
				client.backchannelRTCPListener.Close()
			}
		}

		delete(rf.clients, sessionID)
		core.Logger.Trace().Msgf("Removed RTP client %s", sessionID)
	}
}

func isDeadClientError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "use of closed network connection")
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

func (rf *RTPForwarder) ForwardVideoPacket(packet *rtp.Packet) {
	rf.mutex.RLock()

	if len(rf.clients) == 0 {
		rf.mutex.RUnlock()
		return
	}

	// Rebase timestamp to wall clock (90kHz for H264)
	if !rf.videoTsStarted {
		rf.videoTimeStart = time.Now()
		rf.videoSeqStart = packet.SequenceNumber
		rf.videoTsStarted = true
	}
	elapsed := time.Since(rf.videoTimeStart)
	packet.Timestamp = uint32(elapsed.Seconds() * 90000)
	packet.SequenceNumber = rf.videoSeqStart + (packet.SequenceNumber - rf.videoSeqStart)

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

	// Before IDR keyframe (5), inject cached SPS/PPS
	if nalType == 5 && rf.spsPacket != nil && rf.ppsPacket != nil {
		rf.forwardVideoData(rf.spsPacket)
		rf.forwardVideoData(rf.ppsPacket)
	}

	rf.forwardVideoData(packet)
	rf.mutex.RUnlock()
}

func (rf *RTPForwarder) forwardVideoData(packet *rtp.Packet) {
	data, err := packet.Marshal()
	if err != nil {
		core.Logger.Error().Err(err).Msg("Error marshaling video RTP packet")
		return
	}

	var deadClients []string

	for sessionID, client := range rf.clients {
		client.lastActivity = time.Now()

		if client.transportMode == TransportUDP {
			if client.videoConn != nil {
				if _, err := client.videoConn.Write(data); err != nil {
					if isDeadClientError(err) {
						deadClients = append(deadClients, sessionID)
					} else {
						core.Logger.Error().Err(err).Msgf("Error forwarding video packet to UDP client %s", sessionID)
					}
				} else if rf.firstVideoPacket {
					rf.firstVideoPacket = false
					core.Logger.Trace().Msgf("Successfully sent first video packet to UDP client %s on port %d",
						sessionID, client.videoRTPPort)
				}
			}
		} else if client.transportMode == TransportTCP {
			if client.tcpConn != nil {
				if err := rf.sendInterleavedRTP(client.tcpConn, client.videoRTPChannel, data); err != nil {
					if isDeadClientError(err) {
						deadClients = append(deadClients, sessionID)
					} else {
						core.Logger.Error().Err(err).Msgf("Error forwarding video packet to TCP client %s", sessionID)
					}
				} else if rf.firstVideoPacket {
					rf.firstVideoPacket = false
					core.Logger.Trace().Msgf("Successfully sent first video packet to TCP client %s on channel %d",
						sessionID, client.videoRTPChannel)
				}
			}
		}
	}

	if len(deadClients) > 0 {
		for _, id := range deadClients {
			core.Logger.Info().Msgf("Removing dead video client %s", id)
			delete(rf.clients, id)
		}
	}
}

func (rf *RTPForwarder) ForwardAudioPacket(packet *rtp.Packet) {
	rf.mutex.RLock()
	defer rf.mutex.RUnlock()

	if len(rf.clients) == 0 {
		return
	}

	// Rebase timestamp to wall clock (8kHz for PCMU)
	if !rf.audioTsStarted {
		rf.audioTimeStart = time.Now()
		rf.audioTsStarted = true
	}
	elapsed := time.Since(rf.audioTimeStart)
	packet.Timestamp = uint32(elapsed.Seconds() * 8000)

	// Serialize packet
	data, err := packet.Marshal()
	if err != nil {
		core.Logger.Error().Err(err).Msg("Error marshaling audio RTP packet")
		return
	}

	// Forward to all clients
	for sessionID, client := range rf.clients {
		client.lastActivity = time.Now()

		if client.transportMode == TransportUDP {
			if client.audioConn != nil {
				if _, err := client.audioConn.Write(data); err != nil {
					core.Logger.Error().Err(err).Msgf("Error forwarding audio packet to UDP client %s", sessionID)
				} else if rf.firstAudioPacket {
					rf.firstAudioPacket = false
					core.Logger.Trace().Msgf("Successfully sent first audio packet to UDP client %s on port %d",
						sessionID, client.audioRTPPort)
				}
			}
		} else if client.transportMode == TransportTCP {
			if client.tcpConn != nil {
				if err := rf.sendInterleavedRTP(client.tcpConn, client.audioRTPChannel, data); err != nil {
					core.Logger.Error().Err(err).Msgf("Error forwarding audio packet to TCP client %s", sessionID)
				} else if rf.firstAudioPacket {
					rf.firstAudioPacket = false
					core.Logger.Trace().Msgf("Successfully sent first audio packet to TCP client %s on channel %d",
						sessionID, client.audioRTPChannel)
				}
			}
		}
	}
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
			if client.transportMode == TransportUDP {
				if client.videoConn != nil {
					client.videoConn.Close()
				}
				if client.audioConn != nil {
					client.audioConn.Close()
				}
				if client.backchannelListener != nil {
					client.backchannelListener.Close()
				}
				if client.backchannelRTCPListener != nil {
					client.backchannelRTCPListener.Close()
				}
			}
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

	if _, err := conn.Write(fullPacket); err != nil {
		return err
	}

	return nil
}
