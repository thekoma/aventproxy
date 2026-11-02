package rtsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
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
	listeners      []net.Listener
	storageManager *storage.StorageManager
	clients        map[string]*RTSPClient
	streams        map[string]*CameraStream
	mutex          sync.RWMutex
	ctx            context.Context
	cancel         context.CancelFunc
	running        bool
	MobileClient   *tuya.MobileSDKClient
	mqttManager    *MQTTManager

	// Talkback asks the camera for two-way audio on every stream. Off by
	// default; see WebRTCBridge.Talkback and issue #72.
	Talkback bool

	// BindAddress limits the interfaces the RTSP server listens on. The server
	// has no authentication, so anyone who can reach the port can watch the
	// camera. Accepted values (see bindHosts):
	//   - "" (default for library callers): all interfaces, the old behavior;
	//   - "localhost": 127.0.0.1 and ::1 (the add-on default);
	//   - one address or a comma-separated list, e.g. "127.0.0.1,192.168.1.10";
	//   - "0.0.0.0" or "::": all interfaces, on purpose.
	// Set it before Start.
	BindAddress string

	// LAN streaming: tried first for eligible streams (see lanEligible). Set
	// before Start, read-only after. LANTargets is keyed by device id.
	LanStreaming bool
	UID          string
	LANTargets   map[string]LANTarget

	cooldownMutex sync.Mutex
	lanCooldowns  map[string]time.Time // device id -> end of its LAN cool-down
}

// describeBind is the human-readable form of BindAddress for the log.
func describeBind(bind string) string {
	if strings.TrimSpace(bind) == "" {
		return "all interfaces"
	}
	return bind
}

// localIP returns the local IP of a TCP connection, or nil when it is not one.
func localIP(conn net.Conn) net.IP {
	if conn == nil {
		return nil
	}
	if addr, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		return addr.IP
	}
	return nil
}

// bindHosts turns BindAddress into the list of hosts to listen on. "localhost"
// expands to both loopback addresses: a client that resolves localhost to ::1
// first must not hit a closed port when only 127.0.0.1 is bound.
func bindHosts(bind string) []string {
	bind = strings.TrimSpace(bind)
	if bind == "" {
		return []string{""}
	}
	var hosts []string
	seen := make(map[string]bool)
	add := func(h string) {
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	for _, part := range strings.Split(bind, ",") {
		part = strings.Trim(strings.TrimSpace(part), "[]")
		switch {
		case part == "":
			continue
		case strings.EqualFold(part, "localhost"):
			add("127.0.0.1")
			add("::1")
		default:
			add(part)
		}
	}
	// A value with no address in it (",,") is a mistake, not a request for
	// every interface: returning nil makes Start refuse it instead of failing
	// open on an unauthenticated stream.
	return hosts
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
	pending      bool // created, first client not attached yet
	lastActivity time.Time

	// handlingError prevents re-entrant OnError from PeerConnection.Close during teardown.
	handlingError bool

	// bridgeStarted records that the current bridge has been started or
	// stopped once, so a restart replaces it instead of reusing a dead one.
	bridgeStarted bool

	// Delayed shutdown
	shutdownTimer *time.Timer
	shutdownDelay time.Duration

	// Reference to server for cleanup
	server   *RTSPServer
	streamId string

	// startStreamOverride, if set, replaces startStream (tests only).
	startStreamOverride func()

	lan           lanStreamer                     // the LAN source, from its attempt on; nil on the cloud
	lanFactory    func(*RTPForwarder) lanStreamer // builds a LAN source feeding that forwarder; callers hold mutex
	cloudStart    func() error                    // the cloud attempt; callers hold mutex
	attemptCancel context.CancelFunc              // ends a LAN attempt or probe in flight
	promoteTimer  *time.Timer                     // moves a cloud stream back to the LAN; see armPromotion
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
		lanCooldowns:   make(map[string]time.Time),
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

	hosts := bindHosts(s.BindAddress)
	if len(hosts) == 0 {
		return fmt.Errorf("no address to listen on in bind %q", s.BindAddress)
	}

	lc := net.ListenConfig{Control: reuseAddrControl}
	var listeners []net.Listener
	var listenErrs []string
	for _, host := range hosts {
		addr := net.JoinHostPort(host, strconv.Itoa(s.port))
		ln, err := lc.Listen(s.ctx, "tcp", addr)
		if err != nil {
			// One missing address family (e.g. ::1 on a host with IPv6 off)
			// must not stop the bridge as long as another address is bound.
			core.Logger.Warn().Msgf("RTSP: cannot listen on %s: %v", addr, err)
			listenErrs = append(listenErrs, err.Error())
			continue
		}
		core.Logger.Info().Msgf("RTSP: listening on %s", ln.Addr())
		listeners = append(listeners, ln)
	}
	if len(listeners) == 0 {
		return fmt.Errorf("failed to listen on port %d (bind %q): %s", s.port, s.BindAddress, strings.Join(listenErrs, "; "))
	}

	s.listeners = listeners
	s.running = true

	if s.MobileClient != nil {
		s.mqttManager = NewMQTTManager(s.MobileClient)
	}

	core.Logger.Info().Msgf("RTSP Server started on port %d (bind: %s)", s.port, describeBind(s.BindAddress))
	core.Logger.Info().Msgf("Available endpoints:")

	// List available camera endpoints
	if err := s.printAvailableEndpoints(); err != nil {
		core.Logger.Warn().Msgf("Could not list camera endpoints: %v", err)
	}

	// Start accepting connections
	for _, ln := range s.listeners {
		go s.acceptConnections(ln)
	}

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

	// Close listeners
	for _, ln := range s.listeners {
		ln.Close()
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

func (s *RTSPServer) acceptConnections(listener net.Listener) {
	// running is written under s.mutex by Stop, so it is read through
	// IsRunning here rather than directly.
	for s.IsRunning() {
		select {
		case <-s.ctx.Done():
			return
		default:
			conn, err := listener.Accept()
			if err != nil {
				if s.IsRunning() {
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
		if stream.active || stream.connecting || stream.pending {
			core.Logger.Trace().Msgf("Reusing existing stream for camera: %s", camera.DeviceName)
			stream.lastActivity = time.Now()
			return stream, nil
		}
	}

	// Create new stream. The cloud attempt connects MQTT (startCloud), so a
	// stream exists while the cloud is down and s.mutex never waits on it.
	stream := NewCameraStream(camera, streamResolution, user, s.storageManager, s)

	if s.MobileClient != nil {
		stream.webrtcBridge.SetMobileClient(s.MobileClient)
	}

	stream.attachBridgeErrorHandler()
	stream.pending = true

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
	client, exists := s.clients[sessionID]
	delete(s.clients, sessionID)
	s.mutex.Unlock()

	if !exists {
		return
	}
	// The stream lock can be held for a whole connect, so never wait on it under s.mutex.
	if client.stream != nil {
		client.stream.RemoveClient(sessionID)
	}
	client.conn.Close()
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
	if server != nil {
		stream.webrtcBridge.Talkback = server.Talkback
	}
	stream.lanFactory = stream.newLANSource
	stream.cloudStart = stream.startCloud

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
	cs.pending = false

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

	if cs.tryLAN() {
		return
	}

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
		err := cs.cloudStart()
		if err == nil {
			cs.connecting = false
			cs.active = true
			if len(cs.clients) == 0 { // the last client left during a LAN attempt or the retry wait
				cs.scheduleShutdown()
			}
			cs.armPromotion()
			cs.mutex.Unlock()
			core.Logger.Info().Msgf("Streaming %s over the cloud", cs.camera.DeviceName)
			return
		}

		core.Logger.Error().Err(err).Msgf("Failed to start WebRTC bridge (attempt %d/2)", attempt)
		cs.mutex.Unlock()
	}

	// Nothing streams. The clients are closed so they reconnect now rather
	// than at their own timeout, and a cool-down that alone kept this stream
	// off the LAN ends: with no working cloud there is nothing to wait for.
	cs.mutex.Lock()
	if ok, reason := cs.lanEligible(); !ok && reason == "cooling down" {
		cs.server.cooldownMutex.Lock()
		delete(cs.server.lanCooldowns, cs.camera.DeviceID)
		cs.server.cooldownMutex.Unlock()
		core.Logger.Info().Msgf("The cloud failed too for %s, trying the LAN again on the next start", cs.camera.DeviceName)
	}
	clients := cs.teardownLocked()
	cs.mutex.Unlock()
	closeClients(clients)
}

func (cs *CameraStream) stopStream() {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()
	cs.stopStreamInternal()
}

// replaceBridge swaps in a fresh WebRTC bridge, rewired to the server's mobile
// client, with the error handler attached; startCloud adds the MQTT client.
// Callers must hold cs.mutex.
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
	cs.bridgeStarted = false

	if cs.server != nil {
		cs.webrtcBridge.Talkback = cs.server.Talkback
		if cs.server.MobileClient != nil {
			cs.webrtcBridge.SetMobileClient(cs.server.MobileClient)
		}
	}

	cs.attachBridgeErrorHandler()
}

// startCloud is the default cloudStart. It fetches the MQTT client only now,
// so a stream can exist while the cloud is down. Callers must hold cs.mutex.
func (cs *CameraStream) startCloud() error {
	if cs.server != nil && cs.server.mqttManager != nil {
		mqttClient, err := cs.server.mqttManager.GetClient(cs.camera.DeviceID)
		if err != nil {
			return fmt.Errorf("failed to get MQTT client: %v", err)
		}
		cs.webrtcBridge.SetMQTTClient(mqttClient)
	}
	return cs.webrtcBridge.Start()
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

	core.Logger.Error().Err(err).Msgf("WebRTC error for camera %s", cs.camera.DeviceName)

	clients := cs.teardownLocked()
	cs.mutex.Unlock()
	closeClients(clients)
}

// teardownLocked stops a failed stream and returns its clients, for the caller
// to close once it has released cs.mutex. Callers must hold cs.mutex.
func (cs *CameraStream) teardownLocked() []*RTSPClient {
	cs.handlingError = true

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
	return clients
}

func closeClients(clients []*RTSPClient) {
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
	if cs.promoteTimer != nil {
		cs.promoteTimer.Stop()
		cs.promoteTimer = nil
	}

	// Only log if we were actually active
	if wasActive {
		core.Logger.Info().Msgf("Stopping stream for camera: %s", cs.camera.DeviceName)
	}

	// A LAN attempt in flight ends now rather than at its deadline.
	if cs.attemptCancel != nil {
		cs.attemptCancel()
		cs.attemptCancel = nil
	}
	if cs.lan != nil {
		cs.lan.Stop()
		cs.lan = nil
	}

	// Stop WebRTC bridge (sends SendDisconnect to clean device-side session)
	if cs.webrtcBridge != nil {
		cs.webrtcBridge.OnError = nil
		cs.webrtcBridge.Stop()
		// Even a bridge that never ran (a LAN stream) has lost its OnError,
		// so a restart must replace it.
		cs.bridgeStarted = true
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
