package rtsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pion "github.com/pion/webrtc/v4"
	"golang.org/x/net/publicsuffix"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/storage"
	"avent-webrtc-bridge/pkg/tuya"
	"avent-webrtc-bridge/pkg/utils"
	"avent-webrtc-bridge/pkg/webrtc"

	"github.com/pion/rtp"
)

// answerTimeout bounds the wait for the camera's answer so a silent camera cannot hang startStream.
const answerTimeout = 20 * time.Second

type WebRTCBridge struct {
	camera         *storage.CameraInfo
	resolution     string
	streamType     int
	isHEVC         bool
	user           *storage.UserSession
	storageManager *storage.StorageManager
	mobileClient   *tuya.MobileSDKClient

	// WebRTC components
	peerConnection *pion.PeerConnection
	dataChannel    *pion.DataChannel
	mqttClient     *tuya.MQTTClient
	cameraClient   *tuya.MQTTCameraClient
	controls       cameraControls // the camera client's 312 controls; a seam for tests
	rtpForwarder   *RTPForwarder
	ownsForwarder  bool // the bridge made rtpForwarder, so Stop stops it

	// SSRCs the camera announces for the HEVC datachannel transport: set on
	// the datachannel's message goroutine, read for every packet.
	videoSSRC atomic.Uint32
	audioSSRC atomic.Uint32

	// Packets this session delivered, logged when it stops (issue #72: does
	// audio keep flowing with the monitor's speaker kept closed?).
	videoPackets atomic.Uint64
	audioPackets atomic.Uint64

	// State
	connected         bool
	stopped           bool
	ownsClient        bool
	answerReceived    bool
	pendingCandidates []pion.ICECandidateInit
	waiter            utils.Waiter
	mutex             sync.RWMutex

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// RTP forwarding
	videoTrack *pion.TrackRemote
	audioTrack *pion.TrackRemote
	// backchannel is set by pion's track callback and read for every
	// talkback packet, on other goroutines.
	backchannel atomic.Pointer[pion.TrackLocalStaticRTP]

	// Talkback offers the camera two-way audio. Off by default: the camera
	// session of a stream sets it only with TalkbackSession, while with
	// TalkbackOnDemand only a talk's own session does (see talker). It is not
	// what stops a playing lullaby
	// (issue #72): DPS 253 (app_talking) flips with a recvonly offer and no
	// backchannel too, which points at the webrtc session mode itself; see
	// SpeakerOff.
	Talkback bool

	// SpeakerOff asks the monitor, once the session is up, to keep its
	// speaker closed (312 "speaker", value 0). Never sent with Talkback.
	SpeakerOff bool

	// WebRTCAudio is the audio the offer asks the camera for: recvonly, or
	// none to leave the section out (an issue #72 experiment). Talkback
	// overrides it.
	WebRTCAudio string

	// Callbacks
	OnVideoPacket func(packet *rtp.Packet)
	OnAudioPacket func(packet *rtp.Packet)

	// onError is called from pion's and MQTT's goroutines while the stream
	// sets and clears it, so it sits behind its own lock: wb.mutex is held
	// for the whole of Start.
	errMu   sync.Mutex
	onError func(error)
}

// NewWebRTCBridge prepares a bridge that feeds forwarder. The forwarder
// belongs to the caller, which stops it; a nil forwarder makes the bridge
// create its own and stop it with itself.
func NewWebRTCBridge(camera *storage.CameraInfo, streamResolution string, user *storage.UserSession, storageManager *storage.StorageManager, forwarder *RTPForwarder) *WebRTCBridge {
	ctx, cancel := context.WithCancel(context.Background())

	owns := forwarder == nil
	if owns {
		forwarder = NewRTPForwarder()
	}
	wb := &WebRTCBridge{
		camera:         camera,
		resolution:     streamResolution,
		user:           user,
		rtpForwarder:   forwarder,
		ownsForwarder:  owns,
		storageManager: storageManager,
		connected:      false,
		waiter:         utils.Waiter{},
		ctx:            ctx,
		cancel:         cancel,
	}
	wb.audioSSRC.Store(1) // the defaults the datachannel path matched before the camera names its own

	// Talkback from the forwarder's clients goes to the newest bridge.
	forwarder.SetBackchannelSink(wb.ForwardBackchannelAudioPacket)

	return wb
}

// SetOnError sets the handler for a failed session; nil detaches it.
func (wb *WebRTCBridge) SetOnError(fn func(error)) {
	wb.errMu.Lock()
	wb.onError = fn
	wb.errMu.Unlock()
}

func (wb *WebRTCBridge) errorHandler() func(error) {
	wb.errMu.Lock()
	defer wb.errMu.Unlock()
	return wb.onError
}

func (wb *WebRTCBridge) SetMobileClient(client *tuya.MobileSDKClient) {
	wb.mobileClient = client
}

func (wb *WebRTCBridge) SetMQTTClient(client *tuya.MQTTClient) {
	wb.mqttClient = client
}

func (wb *WebRTCBridge) Start() error {
	wb.mutex.Lock()
	defer wb.mutex.Unlock()

	if wb.connected {
		return errors.New("bridge already connected")
	}

	core.Logger.Info().Msgf("Starting WebRTC bridge for camera: %s", wb.camera.DeviceName)

	var webRTCConfig *tuya.WebRTCConfigResponse
	var mobileMqttsUrl string
	var err error

	if wb.mobileClient != nil {
		// Mobile SDK path
		core.Logger.Info().Msg("Using Tuya Mobile SDK API")

		if err := wb.mobileClient.P2PPreLink(wb.camera.DeviceID); err != nil {
			core.Logger.Warn().Err(err).Msgf(
				"P2P pre-link failed for camera %s (non-fatal, but the WebRTC config may be refused)",
				wb.camera.DeviceName,
			)
		}

		webRTCConfig, err = wb.mobileClient.GetWebRTCConfig(wb.camera.DeviceID)
		if err != nil {
			return fmt.Errorf(
				"failed to get WebRTC config for camera %s (id %s): %v",
				wb.camera.DeviceName, wb.camera.DeviceID, err,
			)
		}

		if err := wb.mobileClient.RTCSessionInit(wb.camera.DeviceID); err != nil {
			core.Logger.Warn().Err(err).Msg("RTC session init failed (non-fatal)")
		}

		if wb.mqttClient == nil {
			// No pre-set MQTT client — create one (fallback for non-managed mode)
			userInfo, err := wb.mobileClient.GetUserInfo()
			if err != nil {
				return fmt.Errorf("failed to get user info: %v", err)
			}
			mobileMqttsUrl = userInfo.Domain.MobileMqttsUrl

			ecode := wb.mobileClient.Ecode
			uid := wb.mobileClient.UID
			mqttDerived := wb.mobileClient.DeriveMQTTConfig(ecode)
			mqttUsername := wb.mobileClient.DeriveMQTTUsername(wb.mobileClient.SID, ecode, wb.mobileClient.PartnerIdentity)
			mqttClientID := wb.mobileClient.DeriveMQTTClientID(uid)
			subscribeTopic := fmt.Sprintf("/av/u/%s", uid)

			wb.mqttClient, err = tuya.NewMobileMqttClient(&tuya.MobileMQTTConfig{
				Username:       mqttUsername,
				Password:       mqttDerived.Password,
				ClientID:       mqttClientID,
				SubscribeTopic: subscribeTopic,
				UID:            uid,
				BrokerURL:      fmt.Sprintf("ssl://%s:8883", mobileMqttsUrl),
			})
			if err != nil {
				return fmt.Errorf("failed to connect to MQTT: %v", err)
			}

			if err = wb.mqttClient.Connected.WaitTimeout(mqttHandshakeTimeout); err != nil {
				return fmt.Errorf("MQTT connection failed: %v", err)
			}

			wb.ownsClient = true
		}

	} else {
		// Original web portal path
		httpClient := wb.createHTTPClient()
		if httpClient == nil {
			return errors.New("failed to create HTTP client")
		}

		appInfo, err := tuya.GetAppInfo(httpClient, wb.user.SessionData.ServerHost)
		if err != nil {
			return fmt.Errorf("failed to get app info: %v", err)
		}
		clientId := appInfo.Result.ClientId

		var mqttConfig *tuya.MQTTConfigResponse
		mqttConfig, err = tuya.GetMQTTConfig(httpClient, wb.user.SessionData.ServerHost)
		if err != nil {
			return fmt.Errorf("failed to get MQTT config: %v", err)
		}

		mobileMqttsUrl = wb.user.SessionData.LoginResult.Domain.MobileMqttsUrl

		webRTCConfig, err = tuya.GetWebRTCConfig(httpClient, wb.user.SessionData.ServerHost, wb.camera.DeviceID)
		if err != nil {
			return fmt.Errorf("failed to get WebRTC config: %v", err)
		}

		wb.mqttClient, err = tuya.NewMqttClient(
			clientId,
			mobileMqttsUrl,
			&mqttConfig.Result,
		)
		if err != nil {
			return fmt.Errorf("failed to connect to MQTT: %v", err)
		}

		if err = wb.mqttClient.Connected.WaitTimeout(mqttHandshakeTimeout); err != nil {
			return fmt.Errorf("MQTT connection failed: %v", err)
		}
	}

	// Parse skill information (may be empty for direct mode)
	var skill tuya.Skill
	if webRTCConfig.Result.Skill != "" {
		if err := json.Unmarshal([]byte(webRTCConfig.Result.Skill), &skill); err != nil {
			core.Logger.Warn().Err(err).Msg("Could not parse skill, using defaults")
		}
	}

	// Determine stream settings
	wb.streamType = tuya.GetStreamType(&skill, wb.resolution)
	wb.isHEVC = tuya.IsHEVC(&skill, wb.streamType)

	core.Logger.Info().Msgf("Stream settings - Resolution: %s, Type: %d, HEVC: %v", wb.resolution, wb.streamType, wb.isHEVC)

	// Triage aid for models that answer an offer with a disconnect instead of an
	// answer (issue #59): what the camera advertises, and what we ask it for. The
	// signaling mode is fixed at "webrtc"; Tuya's other mode is the proprietary
	// "imm" AES/KCP transport, which this bridge does not implement. No auth
	// token or credential is logged.
	core.Logger.Debug().Msgf(
		"Camera capabilities for %s: skill=%q ice_servers=%d requesting mode=webrtc stream_type=%d",
		wb.camera.DeviceName, webRTCConfig.Result.Skill,
		len(webRTCConfig.Result.P2PConfig.Ices), wb.streamType,
	)

	// Setup WebRTC peer connection
	if err := wb.setupPeerConnection(&webRTCConfig.Result); err != nil {
		return fmt.Errorf("failed to setup peer connection: %v", err)
	}

	// Setup MQTT camera client
	wb.setupMQTTCameraClient(&webRTCConfig.Result)

	// Create and send offer
	if err := wb.createAndSendOffer(); err != nil {
		return fmt.Errorf("failed to create offer: %v", err)
	}

	if err = wb.waiter.WaitTimeout(answerTimeout); err != nil {
		if errors.Is(err, utils.ErrWaitTimeout) {
			return fmt.Errorf("failed to establish connection: %w after %v: %s", err, answerTimeout, wb.stalledStep())
		}
		return fmt.Errorf("failed to establish connection: %v", err)
	}

	wb.connected = true
	core.Logger.Info().Msgf("WebRTC bridge started successfully for camera: %s", wb.camera.DeviceName)

	return nil
}

// stalledStep names what a start that timed out was waiting for (#89).
func (wb *WebRTCBridge) stalledStep() string {
	pc := wb.peerConnection
	return stalledAt(pc.RemoteDescription() != nil, pc.ConnectionState(), wb.isHEVC)
}

func stalledAt(answered bool, state pion.PeerConnectionState, hevc bool) string {
	switch {
	case !answered:
		return "the camera never answered the offer"
	case state != pion.PeerConnectionStateConnected:
		return fmt.Sprintf("the camera answered, but the connection to it never came up (%s)", state)
	case hevc:
		return "connected, but the camera never confirmed the video codec on the datachannel"
	default:
		return "connected"
	}
}

func (wb *WebRTCBridge) Stop() {
	wb.mutex.Lock()
	defer wb.mutex.Unlock()

	// A Start that failed after creating the PeerConnection (no answer in
	// time) left it and the MQTT camera client behind: tear those down too.
	if wb.stopped || (!wb.connected && wb.peerConnection == nil) {
		return
	}

	wb.stopped = true
	wb.connected = false

	core.Logger.Info().Msgf("Stopping WebRTC bridge for camera: %s", wb.camera.DeviceName)

	// Cancel context to stop all goroutines
	wb.cancel()

	// Send disconnect
	if wb.cameraClient != nil {
		wb.cameraClient.SendDisconnect()
	}

	// Close peer connection
	if wb.peerConnection != nil {
		wb.peerConnection.Close()
	}

	// Deregister camera client from MQTT
	if wb.mqttClient != nil && wb.cameraClient != nil {
		wb.mqttClient.RemoveCameraClient(wb.cameraClient.SessionId)
	}

	// Only stop MQTT client if we created it (not managed by MQTTManager)
	if wb.ownsClient && wb.mqttClient != nil {
		wb.mqttClient.Stop()
	}

	// Stop the RTP forwarder, unless it belongs to the stream: its clients
	// stay attached across a bridge replacement.
	if wb.ownsForwarder && wb.rtpForwarder != nil {
		wb.rtpForwarder.Stop()
	}

	core.Logger.Info().Msgf("Cloud session for %s carried %d video and %d audio packets",
		wb.camera.DeviceName, wb.videoPackets.Load(), wb.audioPackets.Load())
	core.Logger.Info().Msgf("WebRTC bridge stopped for camera: %s", wb.camera.DeviceName)
}

func (wb *WebRTCBridge) IsConnected() bool {
	wb.mutex.RLock()
	defer wb.mutex.RUnlock()
	return wb.connected
}

func (wb *WebRTCBridge) ForwardBackchannelAudioPacket(packet *rtp.Packet) {
	if track := wb.backchannel.Load(); track != nil {
		_ = track.WriteRTP(packet)
	}
}

func (wb *WebRTCBridge) setupPeerConnection(webRTCConfig *tuya.WebRTCConfig) error {
	// Convert ICE servers
	iceServerBytes, err := json.Marshal(webRTCConfig.P2PConfig.Ices)
	if err != nil {
		return fmt.Errorf("failed to marshal ICE servers: %v", err)
	}

	iceServers, err := webrtc.UnmarshalICEServers(iceServerBytes)
	if err != nil {
		return fmt.Errorf("failed to unmarshal ICE servers: %v", err)
	}

	// Create peer connection configuration
	conf := pion.Configuration{
		ICEServers:         iceServers,
		ICETransportPolicy: pion.ICETransportPolicyAll,
		BundlePolicy:       pion.BundlePolicyMaxBundle,
	}

	// Create WebRTC API
	api, err := webrtc.NewAPI()
	if err != nil {
		return fmt.Errorf("failed to create WebRTC API: %v", err)
	}

	// Create peer connection
	wb.peerConnection, err = api.NewPeerConnection(conf)
	if err != nil {
		return fmt.Errorf("failed to create peer connection: %v", err)
	}

	// On HEVC, use DataChannel to receive video/audio
	if wb.isHEVC {
		maxRetransmits := uint16(5)
		ordered := true

		wb.dataChannel, err = wb.peerConnection.CreateDataChannel("fmp4Stream", &pion.DataChannelInit{
			MaxRetransmits: &maxRetransmits,
			Ordered:        &ordered,
		})

		wb.dataChannel.OnMessage(func(msg pion.DataChannelMessage) {
			if msg.IsString {
				if connected, err := wb.probe(msg); err != nil {
					wb.handleError(err)
				} else if connected {
					wb.waiter.Done(nil)
				}
			} else {
				packet := &rtp.Packet{}
				if err := packet.Unmarshal(msg.Data); err != nil {
					// skip
					return
				}

				switch packet.SSRC {
				case wb.videoSSRC.Load():
					wb.forwardVideo(packet)
				case wb.audioSSRC.Load():
					wb.forwardAudio(packet)
				}
			}
		})

		wb.dataChannel.OnError(func(err error) {
			wb.handleError(err)
		})

		wb.dataChannel.OnClose(func() {
			wb.handleError(errors.New("datachannel: closed"))
		})

		wb.dataChannel.OnOpen(func() {
			codecRequest, _ := json.Marshal(tuya.DataChannelMessage{
				Type: "codec",
				Msg:  "",
			})

			if err := wb.sendMessageToDataChannel(codecRequest); err != nil {
				wb.handleError(fmt.Errorf("failed to send codec request: %w", err))
			}
		})
	}

	// Setup connection state handler
	wb.peerConnection.OnConnectionStateChange(wb.handlePeerConnectionStateChange)

	// Setup track handler for incoming media if not HEVC
	wb.peerConnection.OnTrack(func(track *pion.TrackRemote, receiver *pion.RTPReceiver) {
		codec := track.Codec()
		core.Logger.Trace().Msgf("Received track: %s, PayloadType: %d", codec.MimeType, codec.PayloadType)

		if track.Kind() == pion.RTPCodecTypeVideo {
			wb.videoTrack = track

			if !wb.isHEVC {
				go wb.handleVideoTrack(track)
			}
		} else if track.Kind() == pion.RTPCodecTypeAudio {
			wb.audioTrack = track

			for _, tr := range wb.peerConnection.GetTransceivers() {
				if tr.Receiver() == receiver && tr.Kind() == pion.RTPCodecTypeAudio {
					if tr.Direction() == pion.RTPTransceiverDirectionSendrecv || tr.Direction() == pion.RTPTransceiverDirectionSendonly {
						localTrack, _ := pion.NewTrackLocalStaticRTP(
							pion.RTPCodecCapability{MimeType: track.Codec().MimeType},
							"audio-backchannel", "pion",
						)
						tr.Sender().ReplaceTrack(localTrack)
						wb.backchannel.Store(localTrack)
						core.Logger.Trace().Msgf("Setup backchannel track")
						break
					}
				}
			}

			if !wb.isHEVC {
				go wb.handleAudioTrack(track)
			}
		}
	})

	return nil
}

func (wb *WebRTCBridge) handlePeerConnectionStateChange(state pion.PeerConnectionState) {
	if state == pion.PeerConnectionStateFailed || state == pion.PeerConnectionStateClosed {
		wb.handleError(errors.New("WebRTC connection failed/closed"))
	}

	if state == pion.PeerConnectionStateConnected {
		core.Logger.Info().Msgf("WebRTC connection established")
		wb.sendSessionControls()
		if !wb.isHEVC {
			wb.waiter.Done(nil)
		}
	}
}

// The audio a cloud offer asks the camera for (WebRTCBridge.WebRTCAudio).
const (
	WebRTCAudioRecvonly = "recvonly"
	WebRTCAudioNone     = "none"
)

// ParseWebRTCAudio checks a webrtc_audio setting; empty means recvonly.
func ParseWebRTCAudio(mode string) (string, error) {
	switch mode {
	case "", WebRTCAudioRecvonly:
		return WebRTCAudioRecvonly, nil
	case WebRTCAudioNone:
		return WebRTCAudioNone, nil
	}
	return "", fmt.Errorf("webrtc audio %q: want %s or %s", mode, WebRTCAudioRecvonly, WebRTCAudioNone)
}

// offerMedias is what an offer asks the camera for. Talkback needs a sendrecv
// audio section whatever audio says; otherwise audio is recvonly, or left out
// with WebRTCAudioNone. The RTSP description never depends on it.
func offerMedias(talkback bool, audio string) []*utils.Media {
	video := &utils.Media{Kind: utils.KindVideo, Direction: utils.DirectionRecvonly}
	switch {
	case talkback:
		return []*utils.Media{{Kind: utils.KindAudio, Direction: utils.DirectionSendRecv}, video}
	case audio == WebRTCAudioNone:
		return []*utils.Media{video}
	default:
		return []*utils.Media{{Kind: utils.KindAudio, Direction: utils.DirectionRecvonly}, video}
	}
}

// speakerAudioCheckDelay is how long after the speaker-off request the bridge
// checks that the session still carries audio.
var speakerAudioCheckDelay = 10 * time.Second

// cameraControls is the part of the MQTT camera client the bridge drives once
// the connection is up.
type cameraControls interface {
	SendResolution(resolution int) error
	SendSpeaker(speaker int) error
}

// sendSessionControls sends the 312 controls a cloud session needs once it is
// up: the HD request, as before (H.264 only), and a speaker-off unless
// talkback is on.
//
// Issue #72: a "webrtc"-mode session sets DPS 253 (app_talking) and pauses a
// playing lullaby whatever audio direction the offer carries. The official
// app never uses this mode: it streams over imm and starts a preview with
// IOCtrl commands 6 and 9, sending the speaker command (8) only while the
// parent talks. A webrtc client has no IOCtrls, so the monitor opens its
// speaker for the whole session; 312 "speaker" is this mode's counterpart of
// command 8. Unconfirmed on hardware: the session's log lines (this one, and
// the audio packet count at Stop) are what settle it.
func (wb *WebRTCBridge) sendSessionControls() {
	if wb.controls == nil {
		return
	}
	if !wb.isHEVC && wb.resolution == "hd" {
		if err := wb.controls.SendResolution(0); err != nil {
			core.Logger.Warn().Err(err).Msgf("Could not ask %s for HD", wb.camera.DeviceName)
		}
	}
	if wb.Talkback || !wb.SpeakerOff {
		return
	}
	if err := wb.controls.SendSpeaker(0); err != nil {
		core.Logger.Warn().Err(err).Msgf("Could not ask %s to keep its speaker off", wb.camera.DeviceName)
		return
	}
	core.Logger.Info().Msgf("Asked %s to keep its speaker off (issue #72)", wb.camera.DeviceName)
	go wb.checkAudioAfterSpeakerOff(wb.audioPackets.Load(), speakerAudioCheckDelay)
}

// checkAudioAfterSpeakerOff says, once, whether audio kept flowing after the
// speaker-off request. If Tuya's "speaker" means the viewer's audio rather
// than the monitor's loudspeaker, the request mutes the stream, and the count
// logged at Stop is never reached by an always-on consumer such as Frigate.
func (wb *WebRTCBridge) checkAudioAfterSpeakerOff(before uint64, after time.Duration) {
	var ended <-chan struct{}
	if wb.ctx != nil {
		ended = wb.ctx.Done()
	}
	select {
	case <-ended:
		return
	case <-time.After(after):
	}
	if n := wb.audioPackets.Load() - before; n > 0 {
		core.Logger.Info().Msgf("Audio from %s still flows with its speaker kept closed: %d packets in %v (issue #72)",
			wb.camera.DeviceName, n, after)
		return
	}
	core.Logger.Warn().Msgf("No audio from %s since asking the monitor to keep its speaker closed; "+
		"set speaker_off to false on the Dev card if you need audio (issue #72)", wb.camera.DeviceName)
}

func (wb *WebRTCBridge) setupMQTTCameraClient(webRTCConfig *tuya.WebRTCConfig) {
	device := &tuya.Device{
		DeviceId:   wb.camera.DeviceID,
		DeviceName: wb.camera.DeviceName,
		Category:   wb.camera.Category,
		ProductId:  wb.camera.ProductID,
		Uuid:       wb.camera.UUID,
	}

	// Create MQTT camera client
	wb.cameraClient = tuya.NewMqttCameraClient(wb.mqttClient, device, webRTCConfig)
	wb.controls = wb.cameraClient
	wb.mqttClient.AddCameraClient(wb.cameraClient.SessionId, wb.cameraClient)

	// Setup handlers. They run on the MQTT client's dispatch goroutine, which
	// every camera shares (MQTTManager), and paho processes no PUBACK while one
	// of them blocks. Reporting a failure takes the stream's lock
	// (handleBridgeError) and a disconnect takes this bridge's, and a teardown
	// can hold either for seconds while it waits for the PUBACK of its own
	// SendDisconnect: a handler waiting on one would hold every camera's
	// publishes and fail another camera's offer. Failures and disconnects are
	// therefore reported on their own goroutine. The answer and the candidates
	// are still handled here, in the order they arrive, which they need.
	wb.answerReceived = false
	wb.pendingCandidates = nil

	wb.cameraClient.HandleAnswer = func(answer tuya.AnswerFrame) {
		core.Logger.Info().Msgf("Answer from %s: %s", wb.camera.DeviceName, summarizeSDP(answer.Sdp))

		desc := pion.SessionDescription{
			Type: pion.SDPTypePranswer,
			SDP:  answer.Sdp,
		}

		if err := wb.peerConnection.SetRemoteDescription(desc); err != nil {
			go wb.handleError(err)
			return
		}

		if err := webrtc.SetAnswer(wb.peerConnection, answer.Sdp); err != nil {
			go wb.handleError(err)
			return
		}

		wb.answerReceived = true

		// Flush any ICE candidates that arrived before the answer
		for _, c := range wb.pendingCandidates {
			if err := wb.peerConnection.AddICECandidate(c); err != nil {
				core.Logger.Warn().Err(err).Msg("Failed to add buffered ICE candidate")
			}
		}
		wb.pendingCandidates = nil
	}

	wb.cameraClient.HandleCandidate = func(candidate tuya.CandidateFrame) {
		candidateStr := strings.TrimSpace(candidate.Candidate)
		if candidateStr == "" {
			return
		}

		if strings.HasPrefix(candidateStr, "a=") {
			candidateStr = candidateStr[2:]
		}
		if !strings.HasSuffix(candidateStr, "\r\n") && !strings.HasSuffix(candidateStr, "\n") {
			candidateStr = candidateStr + "\r\n"
		}

		ice := pion.ICECandidateInit{Candidate: strings.TrimSpace(candidateStr)}

		if !wb.answerReceived {
			core.Logger.Trace().Msgf("Buffering ICE candidate (answer not yet received): %s", strings.TrimSpace(candidateStr))
			wb.pendingCandidates = append(wb.pendingCandidates, ice)
			return
		}

		core.Logger.Trace().Msgf("Adding ICE candidate: %s", strings.TrimSpace(candidateStr))
		if err := wb.peerConnection.AddICECandidate(ice); err != nil {
			core.Logger.Warn().Err(err).Msg("Failed to add ICE candidate")
		}
	}

	wb.cameraClient.HandleError = func(err error) {
		go wb.handleError(err)
	}

	wb.cameraClient.HandleDisconnect = func() {
		go wb.cameraDisconnected()
	}
}

// cameraDisconnected handles the camera ending the session.
func (wb *WebRTCBridge) cameraDisconnected() {
	wb.handleError(errors.New("camera client disconnected"))
	wb.mutex.Lock()
	wb.connected = false
	wb.mutex.Unlock()
}

func (wb *WebRTCBridge) createAndSendOffer() error {
	wb.peerConnection.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate != nil {
			core.Logger.Trace().Msgf("Generated ICE candidate: %s", candidate.ToJSON().Candidate)

			if err := wb.cameraClient.SendCandidate("a=" + candidate.ToJSON().Candidate); err != nil {
				core.Logger.Error().Err(err).Msg("Error sending ICE candidate")
			}
		}
	})

	// Create offer
	offer, err := webrtc.CreateOffer(wb.peerConnection, offerMedias(wb.Talkback, wb.WebRTCAudio))
	if err != nil {
		return fmt.Errorf("failed to create offer: %v", err)
	}

	// Remove extmap lines to reduce payload size (device limitation)
	re := regexp.MustCompile(`\r\na=extmap[^\r\n]*`)
	offer = re.ReplaceAllString(offer, "")

	// The media sections only: the SDP's ICE credentials and fingerprint stay out.
	core.Logger.Info().Msgf("Offer to %s: mode=webrtc stream_type=%d datachannel=%t talkback=%t %s",
		wb.camera.DeviceName, wb.streamType, wb.isHEVC, wb.Talkback, summarizeSDP(offer))

	// Send offer
	if err := wb.cameraClient.SendOffer(offer, wb.resolution, wb.streamType, wb.isHEVC); err != nil {
		return fmt.Errorf("failed to send offer: %v", err)
	}

	return nil
}

func (wb *WebRTCBridge) handleVideoTrack(track *pion.TrackRemote) {
	core.Logger.Trace().Msgf("Starting video track handler")

	for {
		select {
		case <-wb.ctx.Done():
			return
		default:
			packet, _, err := track.ReadRTP()
			if err != nil {
				if err == io.EOF {
					return
				}
				// Check if it's a known close error
				if strings.Contains(err.Error(), "closed") ||
					strings.Contains(err.Error(), "EOF") ||
					strings.Contains(err.Error(), "use of closed network connection") {
					return
				}
				core.Logger.Warn().Err(err).Msg("Unexpected error reading video RTP packet")
				time.Sleep(10 * time.Millisecond)
				continue
			}

			wb.forwardVideo(packet)
		}
	}
}

func (wb *WebRTCBridge) handleAudioTrack(track *pion.TrackRemote) {
	core.Logger.Trace().Msgf("Starting audio track handler")

	for {
		select {
		case <-wb.ctx.Done():
			return
		default:
			packet, _, err := track.ReadRTP()
			if err != nil {
				if err == io.EOF {
					return
				}
				if strings.Contains(err.Error(), "closed") ||
					strings.Contains(err.Error(), "EOF") ||
					strings.Contains(err.Error(), "use of closed network connection") {
					return
				}
				core.Logger.Warn().Err(err).Msg("Unexpected error reading audio RTP packet")
				time.Sleep(10 * time.Millisecond)
				continue
			}

			wb.forwardAudio(packet)
		}
	}
}

// forwardVideo hands one video packet from the camera to the forwarder.
func (wb *WebRTCBridge) forwardVideo(packet *rtp.Packet) {
	wb.videoPackets.Add(1)
	wb.rtpForwarder.ForwardVideoPacket(packet)
}

// forwardAudio hands one audio packet from the camera to the forwarder.
func (wb *WebRTCBridge) forwardAudio(packet *rtp.Packet) {
	wb.audioPackets.Add(1)
	wb.rtpForwarder.ForwardAudioPacket(packet)
}

func (wb *WebRTCBridge) createHTTPClient() *http.Client {
	jar, err := cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	})
	if err != nil {
		return nil
	}

	if wb.user.SessionData != nil && len(wb.user.SessionData.Cookies) > 0 {
		serverURL, _ := url.Parse(fmt.Sprintf("https://%s", wb.user.SessionData.ServerHost))

		var httpCookies []*http.Cookie
		for _, cookie := range wb.user.SessionData.Cookies {
			httpCookies = append(httpCookies, &http.Cookie{
				Name:     cookie.Name,
				Value:    cookie.Value,
				Domain:   cookie.Domain,
				Path:     cookie.Path,
				Expires:  cookie.Expires,
				Secure:   cookie.Secure,
				HttpOnly: cookie.HttpOnly,
			})
		}

		jar.SetCookies(serverURL, httpCookies)
	}

	return &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
	}
}

func (wb *WebRTCBridge) probe(msg pion.DataChannelMessage) (bool, error) {
	var message tuya.DataChannelMessage
	if err := json.Unmarshal([]byte(msg.Data), &message); err != nil {
		return false, err
	}

	switch message.Type {
	case "codec":
		frameRequest, _ := json.Marshal(tuya.DataChannelMessage{
			Type: "start",
			Msg:  "frame",
		})

		err := wb.sendMessageToDataChannel(frameRequest)
		if err != nil {
			return false, err
		}

	case "recv":
		var recvMessage tuya.RecvMessage
		if err := json.Unmarshal([]byte(message.Msg), &recvMessage); err != nil {
			return false, err
		}

		wb.videoSSRC.Store(recvMessage.Video.SSRC)
		wb.audioSSRC.Store(recvMessage.Audio.SSRC)

		completeMsg, _ := json.Marshal(tuya.DataChannelMessage{
			Type: "complete",
			Msg:  "",
		})

		err := wb.sendMessageToDataChannel(completeMsg)
		if err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

func (wb *WebRTCBridge) sendMessageToDataChannel(message []byte) error {
	if wb.dataChannel != nil {
		return wb.dataChannel.Send(message)
	}

	return nil
}

func (wb *WebRTCBridge) handleError(err error) {
	if onError := wb.errorHandler(); onError != nil {
		wb.waiter.Done(err)
		onError(err)
	}
}
