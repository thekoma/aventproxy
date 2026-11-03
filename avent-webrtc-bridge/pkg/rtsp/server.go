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
	"sync/atomic"
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

	// SpeakerOff asks the monitor to keep its speaker closed on every cloud
	// session without talkback; see WebRTCBridge.SpeakerOff. Off by default
	// until a tester confirms it (issue #72): only the Dev card turns it on.
	SpeakerOff bool

	// WebRTCAudio is the audio cloud offers ask for: WebRTCAudioRecvonly (the
	// default) or WebRTCAudioNone. RTSP clients see the same description
	// either way.
	WebRTCAudio string

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

	// newStream, if set, prepares every stream getOrCreateStream creates,
	// before any client can reach it (tests only).
	newStream func(*CameraStream)
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

	// attached is closed once the client has joined its stream, or was
	// refused (see joinStream). Nil for a client that never joins one.
	attached chan struct{}
}

type CameraStream struct {
	camera       *storage.CameraInfo
	resolution   string
	user         *storage.UserSession
	webrtcBridge *WebRTCBridge
	// forwarder is where RTSP clients attach. It is the stream's, not the
	// bridge's, so clients stay attached when a retry or a restart replaces
	// the bridge, and SETUP can reach it without the lock: it never changes.
	forwarder    *RTPForwarder
	clients      map[string]*RTSPClient
	mutex        sync.RWMutex
	lastActivity time.Time

	// The state flags change under mutex (pending also under the server's
	// mutex) and are atomic so the server can read them without waiting for
	// a stream lock that a whole connect can hold.
	connecting atomic.Bool
	active     atomic.Bool
	// pending counts the clients getOrCreateStream handed this stream whose
	// AddClient has not run yet. A count, not a flag: one client arriving
	// must not hide another that is still on its way.
	pending atomic.Int32

	// stops counts the stops that dropped the forwarder's clients (written
	// under mutex). A client compares it across its RTSP setup to learn
	// whether a stop dropped it before it joined; see joinStream.
	stops atomic.Uint64

	// startGen numbers the stream's starts. A start that waited without the
	// lock checks it, so it never acts on a newer start's behalf.
	startGen uint64

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
	videoTimer    *time.Timer                     // fails a cloud session that carries no video; see armVideoCheck
}

// cloudRetryDelay separates the two cloud attempts of a stream start.
var cloudRetryDelay = 3 * time.Second

// cloudVideoTimeout bounds how long a cloud session that came up may carry no
// video before it counts as failed (#89): Start returns once ICE connects, or
// once the HEVC datachannel confirms its codec, which is no proof that video
// will follow. A working camera sends its first packet within a second or two.
var cloudVideoTimeout = 20 * time.Second

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
		WebRTCAudio:    WebRTCAudioRecvonly,
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
	if !s.running {
		s.mutex.Unlock()
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

	streams := make([]*CameraStream, 0, len(s.streams))
	for _, stream := range s.streams {
		streams = append(streams, stream)
	}
	mqttManager := s.mqttManager
	s.mutex.Unlock()

	// Outside s.mutex: a stream's lock can be held for a whole connect.
	for _, stream := range streams {
		stream.Stop()
	}
	if mqttManager != nil {
		mqttManager.Stop()
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
		if stream.active.Load() {
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

// Accept errors are retried after a back-off between these bounds, as
// net/http does, so a failure that persists (no file descriptors left) neither
// spins the loop nor floods the log.
const (
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = time.Second
)

func (s *RTSPServer) acceptConnections(listener net.Listener) {
	var delay time.Duration
	// running is written under s.mutex by Stop, so it is read through
	// IsRunning here rather than directly.
	for s.IsRunning() {
		select {
		case <-s.ctx.Done():
			return
		default:
			conn, err := listener.Accept()
			if err != nil {
				if !s.IsRunning() {
					continue
				}
				core.Logger.Error().Err(err).Msg("Error accepting connection")
				delay = min(max(2*delay, acceptRetryMin), acceptRetryMax)
				select {
				case <-s.ctx.Done():
					return
				case <-time.After(delay):
				}
				continue
			}
			delay = 0

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

	// Parse initial RTSP request. A client that connects and never sends
	// one would otherwise hold this goroutine and its socket for good: it is
	// not in the client map yet, so not even Stop closes it.
	if err := conn.SetReadDeadline(time.Now().Add(requestReadTimeout)); err != nil {
		return
	}
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
	stops := stream.stops.Load()

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
		attached:            make(chan struct{}),
	}

	// The client joins its stream on the side, because the stream's lock is
	// held for a whole cloud attempt, and none of the RTSP answers needs it:
	// joining first made every client after the first wait for the attempt,
	// so a camera that never answered stalled them all at DESCRIBE (#89, #97).
	s.addClient(client)
	go s.joinStream(client, stops)

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
		if stream.inUse() {
			core.Logger.Trace().Msgf("Reusing existing stream for camera: %s", camera.DeviceName)
			// Pending until the client's AddClient, so a stop that lands
			// first cannot drop the stream from the map under it.
			stream.pending.Add(1)
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
	stream.pending.Add(1)
	if s.newStream != nil {
		s.newStream(stream)
	}

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

	// A stream that restarted, or that getOrCreateStream just handed to a
	// client, stays: dropping it would leave it running outside the map.
	if stream.inUse() {
		return
	}
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

// joinStream adds client to its stream, which starts the stream if it is idle.
// stops is the stream's stop count from when the client was handed it. If a
// stop came in between, it dropped the client from the forwarder, and the
// teardown could not close a client the stream did not know yet: the client
// is closed now, to reconnect to a fresh stream, rather than joined to one
// that would never send it anything.
func (s *RTSPServer) joinStream(client *RTSPClient, stops uint64) {
	defer close(client.attached)
	if client.stream.attach(client, stops) {
		return
	}
	core.Logger.Info().Msgf("Closing RTSP client %s: the stream for %s stopped before the client could join it",
		client.session, client.stream.camera.DeviceName)
	_ = client.conn.Close()
}

func (s *RTSPServer) removeClient(sessionID string) {
	s.mutex.Lock()
	client, exists := s.clients[sessionID]
	delete(s.clients, sessionID)
	s.mutex.Unlock()

	if !exists {
		return
	}
	// The connection first: the stream's lock can be held for a whole
	// connect, and the peer must not wait for it. Never under s.mutex either.
	client.conn.Close()
	if client.stream != nil {
		if client.attached != nil {
			<-client.attached // leaving before joining would leave a ghost behind
		}
		client.stream.RemoveClient(sessionID)
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
	// A stream's lock can be held for a whole connect, so it is never taken
	// under s.mutex: snapshot the streams, then look at each one.
	s.mutex.RLock()
	streams := make([]*CameraStream, 0, len(s.streams))
	for _, stream := range s.streams {
		streams = append(streams, stream)
	}
	s.mutex.RUnlock()

	for _, stream := range streams {
		if stream.idleFor(5 * time.Minute) {
			core.Logger.Trace().Msgf("Cleaning up inactive stream for camera: %s", stream.camera.DeviceName)
			stream.Stop()
			s.removeStream(stream)
		}
	}
}

// idleFor reports whether the stream has served nobody for at least d.
func (cs *CameraStream) idleFor(d time.Duration) bool {
	cs.mutex.RLock()
	defer cs.mutex.RUnlock()
	return len(cs.clients) == 0 && time.Since(cs.lastActivity) > d
}

func NewCameraStream(camera *storage.CameraInfo, resolution string, user *storage.UserSession, storageManager *storage.StorageManager, server *RTSPServer) *CameraStream {
	stream := &CameraStream{
		camera:        camera,
		resolution:    resolution,
		user:          user,
		clients:       make(map[string]*RTSPClient),
		lastActivity:  time.Now(),
		shutdownDelay: 30 * time.Second,
		server:        server,
		streamId:      fmt.Sprintf("%s-%s", camera.DeviceID, resolution),
	}

	stream.forwarder = NewRTPForwarder()
	stream.webrtcBridge = NewWebRTCBridge(camera, resolution, user, storageManager, stream.forwarder)
	stream.configureBridge()
	stream.lanFactory = stream.newLANSource
	stream.cloudStart = stream.startCloud

	return stream
}

func (cs *CameraStream) AddClient(client *RTSPClient) {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()
	cs.addClientLocked(client)
}

// attach is AddClient for a client handed the stream when its stop count was
// stops. It refuses the client, and reports false, if the stream stopped
// since: see joinStream.
func (cs *CameraStream) attach(client *RTSPClient, stops uint64) bool {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()
	if cs.stops.Load() != stops {
		cs.clientArrived()
		return false
	}
	cs.addClientLocked(client)
	return true
}

func (cs *CameraStream) addClientLocked(client *RTSPClient) {
	// Cancel any pending shutdown
	if cs.shutdownTimer != nil {
		cs.shutdownTimer.Stop()
		cs.shutdownTimer = nil
		core.Logger.Trace().Msgf("Cancelled pending shutdown for camera %s - new client connected", cs.camera.DeviceName)
	}

	cs.clients[client.session] = client
	cs.lastActivity = time.Now()

	// Start stream if not active and not already connecting
	if !cs.active.Load() && !cs.connecting.Load() {
		gen := cs.claimStart()
		if cs.startStreamOverride != nil {
			go cs.startStreamOverride()
		} else {
			go cs.startStream(gen)
		}
	}
	cs.clientArrived() // after claimStart: see inUse
}

// inUse reports whether the stream is up, starting, or promised to a client
// that has not joined it yet: getOrCreateStream must then hand it out rather
// than replace it. The server reads the flags without the stream's lock, so
// they are read in the order they hand over to each other: a joining client
// claims the start before it leaves pending, and a start that succeeds sets
// active before it clears connecting (markActive). Read the other way round,
// a stream between two flags could look idle for an instant and get a twin,
// the duplicate of issue #97.
func (cs *CameraStream) inUse() bool {
	return cs.pending.Load() > 0 || cs.connecting.Load() || cs.active.Load()
}

// markActive commits a successful start; see inUse for the order. Callers
// hold mutex.
func (cs *CameraStream) markActive() {
	cs.active.Store(true)
	cs.connecting.Store(false)
}

// clientArrived takes one client off pending. A client attached without
// getOrCreateStream (tests, mostly) finds it at zero and leaves it there.
func (cs *CameraStream) clientArrived() {
	for {
		n := cs.pending.Load()
		if n <= 0 || cs.pending.CompareAndSwap(n, n-1) {
			return
		}
	}
}

// claimStart marks the stream connecting and returns the new start's number.
// Callers hold mutex.
func (cs *CameraStream) claimStart() uint64 {
	cs.connecting.Store(true)
	cs.startGen++
	return cs.startGen
}

// superseded reports whether start gen has nothing left to do: the stream is
// up, was stopped, or belongs to a newer start. Callers hold mutex.
func (cs *CameraStream) superseded(gen uint64) bool {
	return cs.active.Load() || !cs.connecting.Load() || cs.startGen != gen
}

func (cs *CameraStream) RemoveClient(sessionID string) {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()

	cs.forwarder.RemoveClient(sessionID)

	delete(cs.clients, sessionID)
	cs.lastActivity = time.Now()

	// Schedule stream shutdown if no clients and stream is active
	if len(cs.clients) == 0 && cs.active.Load() {
		cs.scheduleShutdown()
	}
}

func (cs *CameraStream) SetShutdownDelay(delay time.Duration) {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()
	cs.shutdownDelay = delay
}

func (cs *CameraStream) Stop() {
	// Snapshot first: RemoveClient takes the lock and deletes from the map,
	// so ranging over it directly raced with clients coming and going.
	cs.mutex.RLock()
	sessions := make([]string, 0, len(cs.clients))
	for sessionID := range cs.clients {
		sessions = append(sessions, sessionID)
	}
	cs.mutex.RUnlock()

	for _, sessionID := range sessions {
		cs.RemoveClient(sessionID)
	}
	cs.stopStream()
}

// startStream runs start gen: the LAN first, then up to two cloud attempts.
// Every time it takes the lock back after waiting, it gives up if the start
// was superseded, so a start that slept through a stop and a restart cannot
// tear down or duplicate the newer one.
func (cs *CameraStream) startStream(gen uint64) {
	cs.mutex.Lock()
	if cs.superseded(gen) {
		cs.mutex.Unlock()
		return
	}
	cs.mutex.Unlock()

	if cs.tryLAN(gen) {
		return
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			core.Logger.Info().Msgf("Retrying stream for camera %s (attempt %d/2)", cs.camera.DeviceName, attempt)
			time.Sleep(cloudRetryDelay)
		}

		cs.mutex.Lock()
		if cs.superseded(gen) {
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
			cs.markActive()
			if len(cs.clients) == 0 { // the last client left during a LAN attempt or the retry wait
				cs.scheduleShutdown()
			}
			cs.armPromotion()
			cs.armVideoCheck()
			cs.mutex.Unlock()
			core.Logger.Info().Msgf("Streaming %s with cloud signalling", cs.camera.DeviceName)
			return
		}

		core.Logger.Error().Err(err).Msgf("Failed to start WebRTC bridge (attempt %d/2)", attempt)
		cs.mutex.Unlock()
	}

	// Nothing streams. The clients are closed so they reconnect now rather
	// than at their own timeout, and a cool-down that alone kept this stream
	// off the LAN ends: with no working cloud there is nothing to wait for.
	cs.mutex.Lock()
	if cs.superseded(gen) {
		cs.mutex.Unlock()
		return
	}
	if ok, reason := cs.lanEligible(); !ok && reason == "cooling down" {
		cs.server.cooldownMutex.Lock()
		delete(cs.server.lanCooldowns, cs.camera.DeviceID)
		cs.server.cooldownMutex.Unlock()
		core.Logger.Info().Msgf("Cloud signalling failed too for %s, trying LAN signalling again on the next start", cs.camera.DeviceName)
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
		cs.webrtcBridge.SetOnError(nil)
		cs.webrtcBridge.Stop()
	}

	var storageManager *storage.StorageManager
	if cs.server != nil {
		storageManager = cs.server.storageManager
	}
	cs.webrtcBridge = NewWebRTCBridge(cs.camera, cs.resolution, cs.user, storageManager, cs.forwarder)
	cs.bridgeStarted = false
	cs.configureBridge()
	if cs.server != nil && cs.server.MobileClient != nil {
		cs.webrtcBridge.SetMobileClient(cs.server.MobileClient)
	}

	cs.attachBridgeErrorHandler()
}

// configureBridge copies the server's session settings onto the current
// bridge, the first one and every replacement alike.
func (cs *CameraStream) configureBridge() {
	if cs.server == nil {
		return
	}
	cs.webrtcBridge.Talkback = cs.server.Talkback
	cs.webrtcBridge.SpeakerOff = cs.server.SpeakerOff
	cs.webrtcBridge.WebRTCAudio = cs.server.WebRTCAudio
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

// armVideoCheck fails the cloud session that just came up if it has carried
// no video after cloudVideoTimeout: its clients would wait for good, and every
// later one would join the dead stream instead of starting a fresh one (#89).
// The failure is handled as a WebRTC error is: the clients are closed and
// reconnect. Callers hold cs.mutex.
func (cs *CameraStream) armVideoCheck() {
	bridge := cs.webrtcBridge
	var timer *time.Timer
	timer = time.AfterFunc(cloudVideoTimeout, func() {
		// Taken before timer is read: armVideoCheck's caller holds it while timer is set.
		cs.mutex.Lock()
		if cs.videoTimer != timer { // the stream stopped, or restarted, since
			cs.mutex.Unlock()
			return
		}
		cs.videoTimer = nil
		if !cs.active.Load() || cs.webrtcBridge != bridge || bridge.videoPackets.Load() > 0 {
			cs.mutex.Unlock()
			return
		}
		core.Logger.Warn().Msgf("Cloud session for %s is up but carried no video in %v; closing its clients so they reconnect to a new one",
			cs.camera.DeviceName, cloudVideoTimeout)
		clients := cs.teardownLocked()
		cs.mutex.Unlock()
		closeClients(clients)
	})
	cs.videoTimer = timer
}

// attachBridgeErrorHandler wires WebRTC OnError to handleBridgeError on the current bridge.
func (cs *CameraStream) attachBridgeErrorHandler() {
	bridge := cs.webrtcBridge
	if bridge == nil {
		return
	}
	bridge.SetOnError(func(err error) {
		cs.handleBridgeError(bridge, err)
	})
}

// handleBridgeError tears down a failed WebRTC session and force-closes RTSP clients
// so they reconnect against a fresh stream (required for persistent clients like Scrypted).
//
// Only an error from the bridge an active stream runs on counts. While the
// stream is connecting, the start owns a failed attempt and retries it; and a
// replaced bridge's handler can already be running when its successor takes
// over, and must leave the successor alone.
func (cs *CameraStream) handleBridgeError(bridge *WebRTCBridge, err error) {
	cs.mutex.Lock()
	if cs.handlingError || !cs.active.Load() || cs.webrtcBridge != bridge {
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
		cs.webrtcBridge.SetOnError(nil)
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
	if !cs.active.Load() && !cs.connecting.Load() {
		return
	}

	wasActive := cs.active.Load()
	cs.active.Store(false)
	cs.connecting.Store(false)
	cs.stops.Add(1) // the forwarder drops its clients below

	// Cancel any pending shutdown
	if cs.shutdownTimer != nil {
		cs.shutdownTimer.Stop()
		cs.shutdownTimer = nil
	}
	if cs.promoteTimer != nil {
		cs.promoteTimer.Stop()
		cs.promoteTimer = nil
	}
	if cs.videoTimer != nil {
		cs.videoTimer.Stop()
		cs.videoTimer = nil
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
		cs.webrtcBridge.SetOnError(nil)
		cs.webrtcBridge.Stop()
		// Even a bridge that never ran (a LAN stream) has lost its OnError,
		// so a restart must replace it.
		cs.bridgeStarted = true
	}
	// The bridge leaves the stream's forwarder alone; its clients go here.
	cs.forwarder.Stop()

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
	if !cs.active.Load() {
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
		if len(cs.clients) == 0 && cs.active.Load() {
			core.Logger.Info().Msgf("Executing delayed shutdown for camera %s", cs.camera.DeviceName)
			cs.stopStreamInternal()
		}

		cs.shutdownTimer = nil
	})
}
