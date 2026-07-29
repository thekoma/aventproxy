package rtsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/storage"
	"avent-webrtc-bridge/pkg/tuya"
)

// reuseAddrControl sets SO_REUSEADDR on the listening socket so the bridge can
// re-bind its fixed port immediately after a restart, even while connections
// from the previous instance linger in TIME_WAIT. Without it the add-on crashes
// with "address already in use" on restart (issue #43).
func reuseAddrControl(network, address string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return sockErr
}

type RTSPServer struct {
	port           int
	listener       net.Listener
	storageManager *storage.StorageManager
	clients        map[string]*RTSPClient
	streams        map[string]*CameraStream
	mutex          sync.RWMutex
	ctx            context.Context
	cancel         context.CancelFunc
	running        bool
	MobileClient   *tuya.MobileSDKClient
	mqttManager    *MQTTManager
}

type RTSPClient struct {
	conn                 net.Conn
	session              string
	cameraPath           string
	stream               *CameraStream
	reader               *bufio.Reader
	transportMode        TransportMode
	videoRTPPort         int
	videoRTCPPort        int
	audioRTPPort         int
	audioRTCPPort        int
	backAudioRTPPort     int // server-side port for back audio
	backAudioRTCPPort    int // server-side port for back audio RTCP
	videoRTPChannel      byte
	videoRTCPChannel     byte
	audioRTPChannel      byte
	audioRTCPChannel     byte
	backAudioRTPChannel  byte
	backAudioRTCPChannel byte
	setupCount           int
}

type CameraStream struct {
	camera       *storage.CameraInfo
	resolution   string
	user         *storage.UserSession
	webrtcBridge *WebRTCBridge
	clients      map[string]*RTSPClient
	mutex        sync.RWMutex
	connecting   bool
	active       bool
	lastActivity time.Time

	// handlingError prevents re-entrant OnError from PeerConnection.Close during teardown.
	handlingError bool

	// bridgeStarted records that the current bridge has been started once, so a
	// restart replaces it instead of reusing a stopped one.
	bridgeStarted bool

	// Delayed shutdown
	shutdownTimer *time.Timer
	shutdownDelay time.Duration

	// Reference to server for cleanup
	server   *RTSPServer
	streamId string

	// startStreamOverride, if set, replaces startStream (tests only).
	startStreamOverride func()
}

type ServerConfig struct {
	Port                 int
	MaxClients           int
	StreamTimeout        time.Duration
	ConnectionTimeout    time.Duration
	EnableAuthentication bool
}

func NewRTSPServer(port int, storageManager *storage.StorageManager) *RTSPServer {
	ctx, cancel := context.WithCancel(context.Background())

	return &RTSPServer{
		port:           port,
		storageManager: storageManager,
		clients:        make(map[string]*RTSPClient),
		streams:        make(map[string]*CameraStream),
		ctx:            ctx,
		cancel:         cancel,
		running:        false,
	}
}

func (s *RTSPServer) Start() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.running {
		return errors.New("server is already running")
	}

	lc := net.ListenConfig{Control: reuseAddrControl}
	listener, err := lc.Listen(s.ctx, "tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %v", s.port, err)
	}

	s.listener = listener
	s.running = true

	if s.MobileClient != nil {
		s.mqttManager = NewMQTTManager(s.MobileClient)
	}

	core.Logger.Info().Msgf("RTSP Server started on port %d", s.port)
	core.Logger.Info().Msgf("Available endpoints:")

	// List available camera endpoints
	if err := s.printAvailableEndpoints(); err != nil {
		core.Logger.Warn().Msgf("Could not list camera endpoints: %v", err)
	}

	// Start accepting connections
	go s.acceptConnections()

	// Start cleanup routine
	go s.cleanupRoutine()

	return nil
}

func (s *RTSPServer) Stop() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if !s.running {
		return errors.New("server is not running")
	}

	core.Logger.Info().Msg("Stopping RTSP server...")

	// Cancel context to stop all goroutines
	s.running = false
	s.cancel()

	// Close listener
	if s.listener != nil {
		s.listener.Close()
	}

	// Close all client connections
	for _, client := range s.clients {
		client.conn.Close()
	}

	// Stop all streams
	for _, stream := range s.streams {
		stream.Stop()
	}

	if s.mqttManager != nil {
		s.mqttManager.Stop()
	}

	return nil
}

func (s *RTSPServer) IsRunning() bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.running
}

func (s *RTSPServer) GetPort() int {
	return s.port
}

func (s *RTSPServer) GetStats() ServerStats {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	activeStreams := 0
	for _, stream := range s.streams {
		if stream.active {
			activeStreams++
		}
	}

	return ServerStats{
		Port:         s.port,
		Running:      s.running,
		ClientCount:  len(s.clients),
		StreamCount:  activeStreams,
		TotalStreams: len(s.streams),
	}
}

type ServerStats struct {
	Port         int  `json:"port"`
	Running      bool `json:"running"`
	ClientCount  int  `json:"clientCount"`
	StreamCount  int  `json:"activeStreamCount"`
	TotalStreams int  `json:"totalStreams"`
}

func (s *RTSPServer) acceptConnections() {
	for s.running {
		select {
		case <-s.ctx.Done():
			return
		default:
			conn, err := s.listener.Accept()
			if err != nil {
				if s.running {
					core.Logger.Error().Err(err).Msg("Error accepting connection")
				}
				continue
			}

			// Handle connection in goroutine
			go s.handleConnection(conn)
		}
	}
}

func (s *RTSPServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	session := generateSessionID()
	core.Logger.Info().Msgf("New RTSP connection established, session=%s", session)

	reader := bufio.NewReader(conn)

	// Parse initial RTSP request
	request, err := s.parseRTSPRequestFromReader(reader)
	if err != nil {
		core.Logger.Error().Err(err).Msg("Error parsing initial RTSP request")
		return
	}

	// Extract camera path from URL
	cameraPath, streamResolution := extractCameraPath(request.URL)
	if cameraPath == "" {
		core.Logger.Error().Msg("Invalid RTSP URL")
		sendRTSPResponse(conn, 400, "Bad Request", nil, "")
		return
	}

	// Find camera
	camera, user, err := s.findCamera(cameraPath)
	if err != nil {
		core.Logger.Error().Msgf("Error finding camera for path %s: %v", cameraPath, err)
		sendRTSPResponse(conn, 500, "Internal Server Error", nil, "")
		return
	}

	if camera == nil {
		core.Logger.Error().Msgf("Camera not found for path %s", cameraPath)
		sendRTSPResponse(conn, 404, "Not Found", nil, "")
		return
	}

	core.Logger.Info().Msgf("New RTSP connection for camera: %s (%s)", camera.DeviceName, camera.DeviceID)

	// Create or get existing stream
	stream, err := s.getOrCreateStream(camera, streamResolution, user)
	if err != nil {
		core.Logger.Error().Err(err).Msgf("Failed to create stream for camera %s", camera.DeviceName)
		sendRTSPResponse(conn, 500, "Internal Server Error", nil, "Failed to create stream")
		return
	}

	// Create RTSP client
	client := &RTSPClient{
		conn:                conn,
		reader:              reader,
		session:             session,
		cameraPath:          cameraPath,
		stream:              stream,
		transportMode:       TransportUDP, // Default to UDP
		videoRTPPort:        0,
		audioRTPPort:        0,
		backAudioRTPPort:    0,
		videoRTPChannel:     0,
		audioRTPChannel:     2,
		backAudioRTPChannel: 4,
		setupCount:          0,
	}

	// Add client to server and stream
	s.addClient(client)
	stream.AddClient(client)

	// Handle initial request
	s.handleRTSPMethod(client, request)

	// Handle further requests
	s.handleRTSPProtocol(client)
}

func (s *RTSPServer) findCamera(path string) (*storage.CameraInfo, *storage.UserSession, error) {
	cameras, err := s.storageManager.GetAllCameras()
	if err != nil {
		return nil, nil, err
	}

	// Find camera by RTSP path
	for _, camera := range cameras {
		if camera.RTSPPath == path {
			// Get user for this camera
			users, err := s.storageManager.ListUsers()
			if err != nil {
				continue
			}

			for _, user := range users {
				if user.UserKey == camera.UserKey {
					return &camera, &user, nil
				}
			}
		}
	}

	return nil, nil, nil
}

func (s *RTSPServer) getOrCreateStream(camera *storage.CameraInfo, streamResolution string, user *storage.UserSession) (*CameraStream, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Check if stream already exists
	streamId := fmt.Sprintf("%s-%s", camera.DeviceID, streamResolution)
	if stream, exists := s.streams[streamId]; exists {
		if stream.active || stream.connecting {
			core.Logger.Trace().Msgf("Reusing existing stream for camera: %s", camera.DeviceName)
			stream.lastActivity = time.Now()
			return stream, nil
		}
	}

	// Create new stream
	stream := NewCameraStream(camera, streamResolution, user, s.storageManager, s)

	// Setup bridge dependencies
	if s.MobileClient != nil {
		stream.webrtcBridge.SetMobileClient(s.MobileClient)
	}
	if s.mqttManager != nil {
		mqttClient, err := s.mqttManager.GetClient(camera.DeviceID)
		if err != nil {
			return nil, fmt.Errorf("failed to get MQTT client: %v", err)
		}
		stream.webrtcBridge.SetMQTTClient(mqttClient)
	}

	stream.attachBridgeErrorHandler()

	s.streams[streamId] = stream

	core.Logger.Info().Msgf("Created new stream for camera: %s", camera.DeviceName)
	return stream, nil
}

// removeStream deletes stream from the server map only if it is still the
// registered instance for that streamId. A stale async cleanup must not remove
// a replacement CameraStream that reconnected under the same id.
func (s *RTSPServer) removeStream(stream *CameraStream) {
	if stream == nil {
		return
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if existing, exists := s.streams[stream.streamId]; exists && existing == stream {
		delete(s.streams, stream.streamId)
		core.Logger.Trace().Msgf("Removed stream %s from server map", stream.streamId)
	}
}

func (s *RTSPServer) addClient(client *RTSPClient) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.clients[client.session] = client
}

func (s *RTSPServer) removeClient(sessionID string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if client, exists := s.clients[sessionID]; exists {
		// Remove client from stream
		if client.stream != nil {
			client.stream.RemoveClient(sessionID)
		}

		client.conn.Close()
		delete(s.clients, sessionID)
	}
}

func (s *RTSPServer) printAvailableEndpoints() error {
	cameras, err := s.storageManager.GetAllCameras()
	if err != nil {
		return err
	}

	if len(cameras) == 0 {
		core.Logger.Warn().Msg("  No cameras available. Run 'cameras refresh' first.")
		return nil
	}

	for _, camera := range cameras {
		var skill *tuya.Skill
		json.Unmarshal([]byte(camera.Skill), &skill)

		supportClarity := skill != nil && (skill.WebRTC&(1<<5)) != 0
		baseUrl := fmt.Sprintf("rtsp://localhost:%d%s", s.port, camera.RTSPPath)

		if supportClarity {
			core.Logger.Info().Msgf("  %s/hd (%s)", baseUrl, camera.DeviceName)
			core.Logger.Info().Msgf("  %s/sd (%s)", baseUrl, camera.DeviceName)
		} else {
			core.Logger.Info().Msgf("  %s (%s)", baseUrl, camera.DeviceName)
		}
	}

	return nil
}

func (s *RTSPServer) cleanupRoutine() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.cleanupInactiveStreams()
		}
	}
}

func (s *RTSPServer) cleanupInactiveStreams() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	now := time.Now()
	for deviceID, stream := range s.streams {
		// Remove streams inactive for more than 5 minutes
		if now.Sub(stream.lastActivity) > 5*time.Minute && len(stream.clients) == 0 {
			core.Logger.Trace().Msgf("Cleaning up inactive stream for camera: %s", stream.camera.DeviceName)
			stream.Stop()
			delete(s.streams, deviceID)
		}
	}
}

func NewCameraStream(camera *storage.CameraInfo, resolution string, user *storage.UserSession, storageManager *storage.StorageManager, server *RTSPServer) *CameraStream {
	stream := &CameraStream{
		camera:        camera,
		resolution:    resolution,
		user:          user,
		clients:       make(map[string]*RTSPClient),
		active:        false,
		lastActivity:  time.Now(),
		shutdownDelay: 30 * time.Second,
		server:        server,
		streamId:      fmt.Sprintf("%s-%s", camera.DeviceID, resolution),
	}

	stream.webrtcBridge = NewWebRTCBridge(camera, resolution, user, storageManager)

	return stream
}

func (cs *CameraStream) AddClient(client *RTSPClient) {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()

	// Cancel any pending shutdown
	if cs.shutdownTimer != nil {
		cs.shutdownTimer.Stop()
		cs.shutdownTimer = nil
		core.Logger.Trace().Msgf("Cancelled pending shutdown for camera %s - new client connected", cs.camera.DeviceName)
	}

	cs.clients[client.session] = client
	cs.lastActivity = time.Now()

	// Start stream if not active and not already connecting
	if !cs.active && !cs.connecting {
		cs.connecting = true
		if cs.startStreamOverride != nil {
			go cs.startStreamOverride()
		} else {
			go cs.startStream()
		}
	}
}

func (cs *CameraStream) RemoveClient(sessionID string) {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()

	// Remove from RTP forwarder
	if cs.webrtcBridge != nil && cs.webrtcBridge.rtpForwarder != nil {
		cs.webrtcBridge.rtpForwarder.RemoveClient(sessionID)
	}

	delete(cs.clients, sessionID)
	cs.lastActivity = time.Now()

	// Schedule stream shutdown if no clients and stream is active
	if len(cs.clients) == 0 && cs.active {
		cs.scheduleShutdown()
	}
}

func (cs *CameraStream) SetShutdownDelay(delay time.Duration) {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()
	cs.shutdownDelay = delay
}

func (cs *CameraStream) Stop() {
	// Clear all clients first
	for sessionID := range cs.clients {
		cs.RemoveClient(sessionID)
	}

	// Stop the stream
	cs.stopStream()
}

func (cs *CameraStream) startStream() {
	cs.mutex.Lock()
	if cs.active || !cs.connecting {
		cs.mutex.Unlock()
		return
	}
	cs.mutex.Unlock()

	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			core.Logger.Info().Msgf("Retrying stream for camera %s (attempt %d/2)", cs.camera.DeviceName, attempt)
			time.Sleep(3 * time.Second)
		}

		cs.mutex.Lock()
		if cs.active || !cs.connecting {
			cs.mutex.Unlock()
			return
		}

		core.Logger.Info().Msgf("Starting stream for camera: %s (attempt %d/2)", cs.camera.DeviceName, attempt)

		// A retry always needs a fresh PeerConnection, and so does a first
		// attempt on a bridge that has already run: Stop() cancels its context
		// and closes the PeerConnection, and teardown clears OnError. A client
		// reconnecting before the async removeStream lands (Scrypted does this
		// in milliseconds) reuses this stream, so without the replacement the
		// attempt runs against a dead bridge with no error handler attached.
		if attempt > 1 || cs.bridgeStarted {
			cs.replaceBridge()
		}

		cs.bridgeStarted = true
		err := cs.webrtcBridge.Start()
		if err == nil {
			cs.connecting = false
			cs.active = true
			cs.mutex.Unlock()
			return
		}

		core.Logger.Error().Err(err).Msgf("Failed to start WebRTC bridge (attempt %d/2)", attempt)
		cs.mutex.Unlock()
	}

	cs.mutex.Lock()
	cs.stopStreamInternal()
	cs.mutex.Unlock()
}

func (cs *CameraStream) stopStream() {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()
	cs.stopStreamInternal()
}

// replaceBridge swaps in a fresh WebRTC bridge, rewired to the server's mobile
// and MQTT clients, with the error handler attached. Callers must hold cs.mutex.
func (cs *CameraStream) replaceBridge() {
	if cs.webrtcBridge != nil {
		cs.webrtcBridge.OnError = nil
		cs.webrtcBridge.Stop()
	}

	var storageManager *storage.StorageManager
	if cs.server != nil {
		storageManager = cs.server.storageManager
	}
	cs.webrtcBridge = NewWebRTCBridge(cs.camera, cs.resolution, cs.user, storageManager)

	if cs.server != nil {
		if cs.server.MobileClient != nil {
			cs.webrtcBridge.SetMobileClient(cs.server.MobileClient)
		}
		if cs.server.mqttManager != nil {
			if mqttClient, err := cs.server.mqttManager.GetClient(cs.camera.DeviceID); err == nil {
				cs.webrtcBridge.SetMQTTClient(mqttClient)
			}
		}
	}

	cs.attachBridgeErrorHandler()
}

// attachBridgeErrorHandler wires WebRTC OnError to handleBridgeError on the current bridge.
func (cs *CameraStream) attachBridgeErrorHandler() {
	if cs.webrtcBridge == nil {
		return
	}
	cs.webrtcBridge.OnError = func(err error) {
		cs.handleBridgeError(err)
	}
}

// handleBridgeError tears down a failed WebRTC session and force-closes RTSP clients
// so they reconnect against a fresh stream (required for persistent clients like Scrypted).
func (cs *CameraStream) handleBridgeError(err error) {
	cs.mutex.Lock()
	if cs.handlingError || (!cs.active && !cs.connecting) {
		cs.mutex.Unlock()
		return
	}
	cs.handlingError = true

	core.Logger.Error().Err(err).Msgf("WebRTC error for camera %s", cs.camera.DeviceName)

	clients := make([]*RTSPClient, 0, len(cs.clients))
	for _, client := range cs.clients {
		clients = append(clients, client)
	}

	// Clear OnError before Stop so PeerConnection.Close cannot re-enter this path.
	if cs.webrtcBridge != nil {
		cs.webrtcBridge.OnError = nil
	}
	cs.stopStreamInternal()
	cs.handlingError = false
	cs.mutex.Unlock()

	for _, client := range clients {
		if client.conn != nil {
			_ = client.conn.Close()
		}
	}
}

func (cs *CameraStream) stopStreamInternal() {
	// Check if we should actually stop
	if !cs.active && !cs.connecting {
		return
	}

	wasActive := cs.active
	cs.active = false
	cs.connecting = false

	// Cancel any pending shutdown
	if cs.shutdownTimer != nil {
		cs.shutdownTimer.Stop()
		cs.shutdownTimer = nil
	}

	// Only log if we were actually active
	if wasActive {
		core.Logger.Info().Msgf("Stopping stream for camera: %s", cs.camera.DeviceName)
	}

	// Stop WebRTC bridge (sends SendDisconnect to clean device-side session)
	if cs.webrtcBridge != nil {
		cs.webrtcBridge.OnError = nil
		cs.webrtcBridge.Stop()
	}

	// MQTT client is NOT closed — kept alive for next stream (matches app behavior)

	// Remove from server map in a separate goroutine to avoid potential deadlock.
	// Pass the stream pointer so a stale cleanup cannot delete a replacement.
	go func() {
		if cs.server != nil {
			cs.server.removeStream(cs)
		}
	}()
}

func (cs *CameraStream) scheduleShutdown() {
	// Don't schedule if we're not active
	if !cs.active {
		return
	}

	// Cancel any existing timer
	if cs.shutdownTimer != nil {
		cs.shutdownTimer.Stop()
	}

	core.Logger.Trace().Msgf("Scheduling shutdown for camera %s in %v", cs.camera.DeviceName, cs.shutdownDelay)

	cs.shutdownTimer = time.AfterFunc(cs.shutdownDelay, func() {
		cs.mutex.Lock()
		defer cs.mutex.Unlock()

		// Double-check no clients connected during the delay and stream is still active
		if len(cs.clients) == 0 && cs.active {
			core.Logger.Info().Msgf("Executing delayed shutdown for camera %s", cs.camera.DeviceName)
			cs.stopStreamInternal()
		}

		cs.shutdownTimer = nil
	})
}
