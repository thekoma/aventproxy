# Bridge MQTT Singleton Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate the MQTT reconnect loop by making the MQTT client a singleton managed outside the WebRTC bridge lifecycle.

**Architecture:** A new `MQTTManager` in `pkg/rtsp/mqtt_manager.go` owns the MQTT client per camera. `WebRTCBridge.Start()` receives the client as a parameter instead of creating it. `WebRTCBridge.Stop()` no longer closes the MQTT client.

**Tech Stack:** Go 1.23, paho.mqtt.golang, pion/webrtc v4

---

### Task 1: Add `IsConnected()` to MQTTClient and disable AutoReconnect

**Files:**
- Modify: `tuya-ipc-terminal/pkg/tuya/mqtt.go`

- [ ] **Step 1: Add `IsConnected()` method**

Add after the `Stop()` method (line 123):

```go
func (c *MQTTClient) IsConnected() bool {
	return c.mqtt != nil && c.mqtt.IsConnected() && !c.closed
}
```

- [ ] **Step 2: Disable AutoReconnect in both constructors**

In `NewMqttClient` (line 66), change:
```go
opts.SetAutoReconnect(false)
```

In `NewMobileMqttClient` (line 102), change:
```go
opts.SetAutoReconnect(false)
```

- [ ] **Step 3: Verify build**

Run: `cd tuya-ipc-terminal && go build ./...`
Expected: No errors

- [ ] **Step 4: Commit**

```bash
git add pkg/tuya/mqtt.go
git commit -m "refactor: disable MQTT AutoReconnect and add IsConnected()"
```

---

### Task 2: Create MQTTManager

**Files:**
- Create: `tuya-ipc-terminal/pkg/rtsp/mqtt_manager.go`

- [ ] **Step 1: Create the MQTTManager file**

```go
package rtsp

import (
	"fmt"
	"sync"
	"time"

	"tuya-ipc-terminal/pkg/core"
	"tuya-ipc-terminal/pkg/tuya"
)

var mqttBackoffDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}

type MQTTManager struct {
	mobileClient *tuya.MobileSDKClient
	clients      map[string]*tuya.MQTTClient // keyed by deviceId
	mutex        sync.Mutex
}

func NewMQTTManager(mobileClient *tuya.MobileSDKClient) *MQTTManager {
	return &MQTTManager{
		mobileClient: mobileClient,
		clients:      make(map[string]*tuya.MQTTClient),
	}
}

func (m *MQTTManager) GetClient(deviceId string) (*tuya.MQTTClient, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if client, exists := m.clients[deviceId]; exists && client.IsConnected() {
		core.Logger.Trace().Msgf("Reusing existing MQTT client for device %s", deviceId)
		return client, nil
	}

	core.Logger.Info().Msgf("Creating new MQTT client for device %s", deviceId)
	client, err := m.connect(deviceId)
	if err != nil {
		return nil, err
	}

	m.clients[deviceId] = client
	return client, nil
}

func (m *MQTTManager) connect(deviceId string) (*tuya.MQTTClient, error) {
	if m.mobileClient == nil {
		return nil, fmt.Errorf("mobile client not configured")
	}

	userInfo, err := m.mobileClient.GetUserInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %v", err)
	}

	ecode := m.mobileClient.Ecode
	uid := m.mobileClient.UID
	mqttDerived := m.mobileClient.DeriveMQTTConfig(ecode)
	mqttUsername := m.mobileClient.DeriveMQTTUsername(m.mobileClient.SID, ecode, m.mobileClient.PartnerIdentity)
	mqttClientID := m.mobileClient.DeriveMQTTClientID(uid)
	subscribeTopic := fmt.Sprintf("/av/u/%s", uid)
	brokerURL := fmt.Sprintf("ssl://%s:8883", userInfo.Domain.MobileMqttsUrl)

	var lastErr error
	for attempt, delay := range mqttBackoffDelays {
		if attempt > 0 {
			core.Logger.Info().Msgf("MQTT connect retry %d/%d for device %s in %v", attempt+1, len(mqttBackoffDelays), deviceId, delay)
			time.Sleep(delay)
		}

		client, err := tuya.NewMobileMqttClient(&tuya.MobileMQTTConfig{
			Username:       mqttUsername,
			Password:       mqttDerived.Password,
			ClientID:       mqttClientID,
			SubscribeTopic: subscribeTopic,
			UID:            uid,
			BrokerURL:      brokerURL,
		})
		if err != nil {
			lastErr = err
			core.Logger.Warn().Err(err).Msgf("MQTT connect attempt %d/%d failed for device %s", attempt+1, len(mqttBackoffDelays), deviceId)
			continue
		}

		if err = client.Connected.Wait(); err != nil {
			lastErr = err
			client.Stop()
			core.Logger.Warn().Err(err).Msgf("MQTT handshake attempt %d/%d failed for device %s", attempt+1, len(mqttBackoffDelays), deviceId)
			continue
		}

		core.Logger.Info().Msgf("MQTT connected for device %s", deviceId)
		return client, nil
	}

	return nil, fmt.Errorf("MQTT connect failed after %d attempts: %v", len(mqttBackoffDelays), lastErr)
}

func (m *MQTTManager) Stop() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for deviceId, client := range m.clients {
		core.Logger.Info().Msgf("Closing MQTT client for device %s", deviceId)
		client.Stop()
	}
	m.clients = make(map[string]*tuya.MQTTClient)
}
```

- [ ] **Step 2: Verify build**

Run: `cd tuya-ipc-terminal && go build ./...`
Expected: No errors

- [ ] **Step 3: Commit**

```bash
git add pkg/rtsp/mqtt_manager.go
git commit -m "feat: add MQTTManager singleton for per-camera MQTT clients"
```

---

### Task 3: Wire MQTTManager into RTSPServer

**Files:**
- Modify: `tuya-ipc-terminal/pkg/rtsp/server.go`

- [ ] **Step 1: Add mqttManager field to RTSPServer**

In the `RTSPServer` struct (after line 28 `MobileClient`), add:

```go
mqttManager *MQTTManager
```

- [ ] **Step 2: Initialize MQTTManager in Start()**

In `Start()`, after `s.running = true` (line 109), add:

```go
if s.MobileClient != nil {
	s.mqttManager = NewMQTTManager(s.MobileClient)
}
```

- [ ] **Step 3: Cleanup MQTTManager in Stop()**

In `Stop()`, after the stream stop loop (line 155), add:

```go
if s.mqttManager != nil {
	s.mqttManager.Stop()
}
```

- [ ] **Step 4: Pass MQTT client in getOrCreateStream()**

In `getOrCreateStream()`, after creating the stream (line 334), replace the `SetMobileClient` block. Change the existing line 334 from:

```go
stream := NewCameraStream(camera, streamResolution, user, s.storageManager, s)
```

to:

```go
stream := NewCameraStream(camera, streamResolution, user, s.storageManager, s)

if s.mqttManager != nil {
	mqttClient, err := s.mqttManager.GetClient(camera.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get MQTT client: %v", err)
	}
	stream.webrtcBridge.SetMQTTClient(mqttClient)
}
```

Remove the old `SetMobileClient` call from `NewCameraStream()` — it moves here. In `NewCameraStream()` (around line 461-463), remove:

```go
if server != nil && server.MobileClient != nil {
	stream.webrtcBridge.SetMobileClient(server.MobileClient)
}
```

Keep `SetMobileClient` on the bridge (it's still needed for WebRTC config), but also call it here alongside the MQTT client:

```go
if s.mqttManager != nil {
	mqttClient, err := s.mqttManager.GetClient(camera.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get MQTT client: %v", err)
	}
	stream.webrtcBridge.SetMQTTClient(mqttClient)
} else if s.MobileClient != nil {
	stream.webrtcBridge.SetMobileClient(s.MobileClient)
}
```

And in `NewCameraStream()`, keep `SetMobileClient` only when mqttManager is also used (the bridge still needs it for API calls). The final `NewCameraStream()`:

```go
func NewCameraStream(camera *storage.CameraInfo, resolution string, user *storage.UserSession, storageManager *storage.StorageManager, server *RTSPServer) *CameraStream {
	stream := &CameraStream{
		camera:        camera,
		resolution:    resolution,
		user:          user,
		clients:       make(map[string]*RTSPClient),
		active:        false,
		lastActivity:  time.Now(),
		shutdownDelay: 5 * time.Second,
		server:        server,
		streamId:      fmt.Sprintf("%s-%s", camera.DeviceID, resolution),
	}

	stream.webrtcBridge = NewWebRTCBridge(camera, resolution, user, storageManager)

	return stream
}
```

And in `getOrCreateStream()`, after creating the stream, before setting OnError:

```go
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
```

- [ ] **Step 5: Verify build**

Run: `cd tuya-ipc-terminal && go build ./...`
Expected: Fails — `SetMQTTClient` not defined yet on WebRTCBridge. That's Task 4.

- [ ] **Step 6: Commit (WIP)**

```bash
git add pkg/rtsp/server.go
git commit -m "wip: wire MQTTManager into RTSPServer"
```

---

### Task 4: Modify WebRTCBridge to receive MQTT client externally

**Files:**
- Modify: `tuya-ipc-terminal/pkg/rtsp/bridge.go`

- [ ] **Step 1: Add SetMQTTClient method and field tracking**

After `SetMobileClient()` (line 87), add:

```go
func (wb *WebRTCBridge) SetMQTTClient(client *tuya.MQTTClient) {
	wb.mqttClient = client
}
```

- [ ] **Step 2: Modify Start() to skip MQTT creation when client is pre-set**

In `Start()`, replace the entire Mobile SDK MQTT block (lines 103-140) with:

```go
if wb.mobileClient != nil {
	// Mobile SDK path
	core.Logger.Info().Msg("Using Tuya Mobile SDK API")

	webRTCConfig, err = wb.mobileClient.GetWebRTCConfig(wb.camera.DeviceID)
	if err != nil {
		return fmt.Errorf("failed to get WebRTC config: %v", err)
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

		if err = wb.mqttClient.Connected.Wait(); err != nil {
			return fmt.Errorf("MQTT connection failed: %v", err)
		}

		wb.ownsClient = true
	}
```

- [ ] **Step 3: Add `ownsClient` field to WebRTCBridge struct**

In the struct definition (after `connected bool`, around line 46), add:

```go
ownsClient bool // true if this bridge created the MQTT client (not managed by MQTTManager)
```

- [ ] **Step 4: Modify Stop() to only close MQTT when bridge owns it**

Replace lines 244-247:

```go
// Stop MQTT client
if wb.mqttClient != nil {
	wb.mqttClient.Stop()
}
```

With:

```go
// Only stop MQTT client if we created it (not managed by MQTTManager)
if wb.ownsClient && wb.mqttClient != nil {
	wb.mqttClient.Stop()
}
```

Also deregister the camera client from the shared MQTT client. Add before the MQTT stop block:

```go
// Deregister camera client from MQTT
if wb.mqttClient != nil && wb.cameraClient != nil {
	wb.mqttClient.RemoveCameraClient(wb.cameraClient.SessionId)
}
```

- [ ] **Step 5: Verify build**

Run: `cd tuya-ipc-terminal && go build ./...`
Expected: No errors — both Task 3 and Task 4 compile together.

- [ ] **Step 6: Commit**

```bash
git add pkg/rtsp/bridge.go pkg/rtsp/server.go
git commit -m "feat: WebRTCBridge receives MQTT client from MQTTManager

Bridge.Start() skips MQTT creation when a client is pre-set via
SetMQTTClient(). Bridge.Stop() only closes MQTT if it created
the client (ownsClient flag). Camera client is deregistered from
shared MQTT client on stop."
```

---

### Task 5: Build, deploy, and validate

**Files:**
- No code changes

- [ ] **Step 1: Full build**

Run: `cd tuya-ipc-terminal && go build -o tuya-ipc-terminal .`
Expected: Binary built successfully.

- [ ] **Step 2: Docker build and deploy**

```bash
cd /home/koma/src/babymonitor
docker compose build aventproxy-bridge
docker compose stop homeassistant
docker compose up -d aventproxy-bridge
```

- [ ] **Step 3: Test with ffplay (single client)**

```bash
ffplay -rtsp_transport tcp -loglevel warning rtsp://localhost:8556/Erik
```

Check logs:
```bash
docker logs babymonitor-aventproxy-bridge-1 2>&1 | grep -c "Connected to mqtt"
# Expected: 1
docker logs babymonitor-aventproxy-bridge-1 2>&1 | grep "WebRTC connection established"
# Expected: 1 line
```

Kill ffplay, wait for shutdown delay (5s), reconnect. Check logs show still just 1 "Connected to mqtt" (client reused).

- [ ] **Step 4: Test with go2rtc (HA multi-client)**

```bash
docker compose start homeassistant
```

Wait 30 seconds, check logs:
```bash
docker logs babymonitor-aventproxy-bridge-1 2>&1 | grep -c "Connected to mqtt"
# Expected: 1
docker logs babymonitor-aventproxy-bridge-1 2>&1 | grep -c "WebRTC connection established"
# Expected: 1
docker logs babymonitor-aventproxy-bridge-1 2>&1 | grep -c "mqtt client is closed"
# Expected: 0
```

- [ ] **Step 5: Test lullaby during stream**

From HA or via test script, play a lullaby. Verify audio plays from monitor. Stop the lullaby. Verify it stops.

- [ ] **Step 6: Commit all Go changes**

```bash
cd /home/koma/src/babymonitor
git add tuya-ipc-terminal/
git commit -m "feat: singleton MQTT client per camera via MQTTManager

Eliminates the MQTT reconnect loop caused by go2rtc's rapid
concurrent RTSP requests. MQTTManager creates one MQTT client
per camera, shared by all WebRTC bridge sessions. The client
persists across stream start/stop cycles and is only closed
on server shutdown.

Fixes: duplicate clientId causing Tuya broker to disconnect
previous connections, AutoReconnect ping-pong, ICE candidate
failures during WebRTC setup."
```

---

### Task 6: Manual validation checklist

Run these after deploy. All must pass.

- [ ] **Test 1 — Single client (ffplay):** WebRTC first attempt. 1 MQTT connect.
- [ ] **Test 2 — Multi-client (go2rtc/HA):** Multiple RTSP requests, 1 MQTT connect. Video plays.
- [ ] **Test 3 — On-demand cycle:** Open → close → wait 10s → reopen. 1 MQTT connect total.
- [ ] **Test 4 — Long-running (leave 30+ min):** Zero MQTT reconnects. Zero WebRTC errors.
- [ ] **Test 5 — Lullaby + streaming:** Play/stop during stream. Close stream, lullaby continues.
