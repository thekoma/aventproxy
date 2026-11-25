package rtsp

import (
	"strings"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"
	"avent-webrtc-bridge/pkg/tuya"

	pion "github.com/pion/webrtc/v4"
)

// Placeholder servers and documentation addresses only; nothing here is real.
var testICEServers = []tuya.ICEServer{
	{Urls: "stun:stun.example.invalid:3478"},
	{Urls: "turn:turn.example.invalid:3478", Username: "u", Credential: "fake"},
}

func TestPeerConfiguration_LANOnlyHasNoICEServers(t *testing.T) {
	conf, err := peerConfiguration(testICEServers, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(conf.ICEServers) != 0 {
		t.Fatalf("LAN-only config has %d ICE servers, want none", len(conf.ICEServers))
	}
}

func TestPeerConfiguration_DefaultKeepsTuyaICEServers(t *testing.T) {
	conf, err := peerConfiguration(testICEServers, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(conf.ICEServers) != 2 || conf.ICEServers[1].Username != "u" ||
		conf.ICEServers[1].URLs[0] != "turn:turn.example.invalid:3478" {
		t.Fatalf("ICE servers = %+v", conf.ICEServers)
	}
}

const (
	hostLAN   = "a=candidate:1 1 udp 2130706431 192.168.1.20 40000 typ host"
	hostULA   = "a=candidate:2 1 udp 2130706431 fd12:3456::1 40000 typ host"
	srflx     = "a=candidate:3 1 udp 1694498815 203.0.113.9 51000 typ srflx raddr 192.168.1.20 rport 40000"
	relay     = "a=candidate:4 1 udp 16777215 203.0.113.50 3478 typ relay raddr 203.0.113.9 rport 51000"
	hostWorld = "a=candidate:5 1 udp 2130706431 198.51.100.7 40000 typ host"
)

func newTestBridge(lanOnly bool) *WebRTCBridge {
	wb := NewWebRTCBridge(&storage.CameraInfo{DeviceID: "dev", DeviceName: "cam"}, "hd", nil, nil, nil)
	wb.LANOnly = lanOnly
	return wb
}

func TestHandleRemoteCandidate_LANOnlyKeepsOnlyLANHosts(t *testing.T) {
	wb := newTestBridge(true)
	for _, c := range []string{hostLAN, srflx, relay, hostWorld, hostULA} {
		wb.handleRemoteCandidate(c)
	}
	if len(wb.pendingCandidates) != 2 {
		t.Fatalf("buffered %d candidates, want the 2 LAN host ones: %+v", len(wb.pendingCandidates), wb.pendingCandidates)
	}
	if !strings.Contains(wb.pendingCandidates[0].Candidate, "192.168.1.20") ||
		!strings.Contains(wb.pendingCandidates[1].Candidate, "fd12:3456::1") {
		t.Fatalf("wrong candidates kept: %+v", wb.pendingCandidates)
	}
	if wb.droppedCandidates != 3 {
		t.Fatalf("dropped %d, want 3", wb.droppedCandidates)
	}
}

func TestHandleRemoteCandidate_DefaultKeepsEverything(t *testing.T) {
	wb := newTestBridge(false)
	for _, c := range []string{hostLAN, srflx, relay, hostWorld} {
		wb.handleRemoteCandidate(c)
	}
	if len(wb.pendingCandidates) != 4 || wb.droppedCandidates != 0 {
		t.Fatalf("buffered %d, dropped %d; want 4, 0", len(wb.pendingCandidates), wb.droppedCandidates)
	}
	if wb.pendingCandidates[1].Candidate != strings.TrimPrefix(srflx, "a=") {
		t.Fatalf("candidate changed: %q", wb.pendingCandidates[1].Candidate)
	}
}

func TestRemoteAnswerSDP(t *testing.T) {
	answer := strings.Join([]string{"v=0", "m=video 9 UDP/TLS/RTP/SAVPF 96", hostLAN, srflx, relay, hostWorld, ""}, "\r\n")

	if got := newTestBridge(false).remoteAnswerSDP(answer); got != answer {
		t.Fatalf("default bridge changed the answer:\n%s", got)
	}

	got := newTestBridge(true).remoteAnswerSDP(answer)
	if !strings.Contains(got, hostLAN+"\r\n") {
		t.Fatalf("LAN host candidate lost:\n%s", got)
	}
	for _, gone := range []string{srflx, relay, hostWorld} {
		if strings.Contains(got, gone) {
			t.Fatalf("%q survived:\n%s", gone, got)
		}
	}
}

func TestServerPassesWebRTCLANOnlyToTheBridge(t *testing.T) {
	server := NewRTSPServer(0, nil)
	server.WebRTCLANOnly = true
	server.CameraLANHosts = map[string]string{"dev": "192.168.1.20"}
	camera := &storage.CameraInfo{DeviceID: "dev", DeviceName: "cam"}
	stream := NewCameraStream(camera, "hd", nil, nil, server)
	if !stream.webrtcBridge.LANOnly || stream.webrtcBridge.LANHost != "192.168.1.20" {
		t.Fatalf("new stream's WebRTC bridge: LAN-only %v, host %q", stream.webrtcBridge.LANOnly, stream.webrtcBridge.LANHost)
	}
	stream.mutex.Lock()
	stream.replaceBridge()
	stream.mutex.Unlock()
	if !stream.webrtcBridge.LANOnly || stream.webrtcBridge.LANHost != "192.168.1.20" {
		t.Fatal("a replaced WebRTC bridge lost the LAN-only settings")
	}
}

// A host candidate on a public address is kept on the camera's one known LAN
// address (an SCD923 on protocol 3.3 put its public address in its host
// candidate, 02-11-2026).
func TestHandleRemoteCandidate_LANOnlyRewritesOntoTheCamerasAddress(t *testing.T) {
	wb := newTestBridge(true)
	wb.LANHost = "192.168.1.20"
	wb.handleRemoteCandidate(hostWorld)
	if len(wb.pendingCandidates) != 1 || !strings.Contains(wb.pendingCandidates[0].Candidate, "192.168.1.20 40000 typ host") {
		t.Fatalf("buffered %+v, want the candidate on the camera's address", wb.pendingCandidates)
	}
}

// With WebRTC over the LAN only, a talk's own session gets the same LAN-only
// setting and the camera's address as the stream's bridge.
func TestTalkSessionKeepsWebRTCOnTheLAN(t *testing.T) {
	server := NewRTSPServer(0, nil)
	server.Talkback = true
	server.TalkbackMode = TalkbackOnDemand
	server.WebRTCLANOnly = true
	server.CameraLANHosts = map[string]string{"d": "192.168.1.10"}
	cs := NewCameraStream(&storage.CameraInfo{DeviceID: "d", DeviceName: "n"}, "hd", nil, nil, server)
	session, err := cs.openTalkSession(func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	wb := session.(talkBridge).WebRTCBridge
	defer wb.cancel()
	if !wb.Talkback || !wb.LANOnly || wb.LANHost != "192.168.1.10" {
		t.Fatalf("talk session: talkback %v, LAN only %v, host %q; want two-way on the camera's LAN address",
			wb.Talkback, wb.LANOnly, wb.LANHost)
	}
}

// LAN-only picks the LAN-only pion API, the default the plain one.
func TestNewWebRTCAPIPicksTheLANOnlyAPI(t *testing.T) {
	oldLAN, oldDefault := newLANOnlyAPI, newDefaultAPI
	defer func() { newLANOnlyAPI, newDefaultAPI = oldLAN, oldDefault }()
	var got []string
	newLANOnlyAPI = func() (*pion.API, error) { got = append(got, "lan"); return oldLAN() }
	newDefaultAPI = func() (*pion.API, error) { got = append(got, "default"); return oldDefault() }
	for _, lanOnly := range []bool{true, false} {
		if _, err := newWebRTCAPI(lanOnly); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, ",") != "lan,default" {
		t.Fatalf("constructors called: %v", got)
	}
}

// The answer reaches the peer connection filtered on a LAN-only bridge, and
// whole otherwise.
func TestHandleAnswerFiltersTheCandidates(t *testing.T) {
	for _, lanOnly := range []bool{true, false} {
		wb := newTestBridge(lanOnly)
		api, err := newWebRTCAPI(false)
		if err != nil {
			t.Fatal(err)
		}
		offerer, err := api.NewPeerConnection(pion.Configuration{})
		if err != nil {
			t.Fatal(err)
		}
		answerer, err := api.NewPeerConnection(pion.Configuration{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := offerer.AddTransceiverFromKind(pion.RTPCodecTypeVideo,
			pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
		offer, err := offerer.CreateOffer(nil)
		if err != nil || offerer.SetLocalDescription(offer) != nil || answerer.SetRemoteDescription(offer) != nil {
			t.Fatalf("offer: %v", err)
		}
		answer, err := answerer.CreateAnswer(nil)
		if err != nil {
			t.Fatal(err)
		}
		wb.peerConnection = offerer
		sdp := strings.TrimRight(answer.SDP, "\r\n") + "\r\n" +
			hostLAN + "\r\n" + hostWorld + "\r\n"
		wb.handleAnswer(tuya.AnswerFrame{Sdp: sdp})
		remote := offerer.RemoteDescription()
		if remote == nil || !wb.answerReceived {
			t.Fatalf("lan-only %v: the answer was not applied", lanOnly)
		}
		if !strings.Contains(remote.SDP, "192.168.1.20") || strings.Contains(remote.SDP, "198.51.100.7") == lanOnly {
			t.Fatalf("lan-only %v: remote description\n%s", lanOnly, remote.SDP)
		}
		offerer.Close()
		answerer.Close()
	}
}

// Candidates trickled before the answer are buffered, and handleAnswer hands
// them to the peer connection once the answer is in, then empties the buffer.
func TestHandleAnswerFlushesTheBufferedCandidates(t *testing.T) {
	wb := newTestBridge(true)
	api, err := newWebRTCAPI(false)
	if err != nil {
		t.Fatal(err)
	}
	offerer, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer offerer.Close()
	answerer, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer answerer.Close()
	if _, err := offerer.AddTransceiverFromKind(pion.RTPCodecTypeVideo,
		pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	offer, err := offerer.CreateOffer(nil)
	if err != nil || offerer.SetLocalDescription(offer) != nil || answerer.SetRemoteDescription(offer) != nil {
		t.Fatalf("offer: %v", err)
	}
	answer, err := answerer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	wb.peerConnection = offerer

	wb.handleRemoteCandidate("a=candidate:6 1 udp 2130706431 192.168.1.21 40001 typ host")
	if len(wb.pendingCandidates) != 1 {
		t.Fatalf("buffered %d candidates before the answer, want 1", len(wb.pendingCandidates))
	}
	wb.handleAnswer(tuya.AnswerFrame{Sdp: answer.SDP})
	if wb.pendingCandidates != nil {
		t.Fatalf("buffer after the answer: %+v", wb.pendingCandidates)
	}
	// pion adds a remote candidate from a goroutine of its own.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var ips []string
		for _, s := range offerer.GetStats() {
			if c, ok := s.(pion.ICECandidateStats); ok && c.Type == pion.StatsTypeRemoteCandidate {
				ips = append(ips, c.IP)
			}
		}
		if strings.Join(ips, ",") == "192.168.1.21" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote candidates after the answer: %v; want the buffered one", ips)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
