# Bridge MQTT Singleton Design

**Date:** 2026-04-21
**Status:** Approved
**Context:** Baby monitor — reliability is critical

## Problem

The WebRTC bridge creates a new MQTT connection for every `startStream()` call. All connections use the same deterministic clientId (`tuya_bridge_mb_{deviceId}_{uidHash}_DEFAULT`). When go2rtc sends rapid concurrent RTSP requests, multiple MQTT connections compete for the same clientId on the Tuya broker. The broker disconnects previous connections (standard MQTT behavior), causing ICE candidates to fail and WebRTC to never establish.

With a single client (ffplay), the bridge works perfectly. The bug only manifests with go2rtc's concurrent connection pattern.

## Root Cause Analysis

Seven interacting defects:

| # | Defect | Severity |
|---|--------|----------|
| 1 | Deterministic clientId — all bridge MQTT clients share the same ID | HIGH |
| 2 | No MQTT singleton — each `WebRTCBridge.Start()` creates a new client | HIGH |
| 3 | `AutoReconnect(true)` — disconnected clients keep reconnecting with same ID, causing ping-pong | MEDIUM |
| 4 | Waiter state machine blocks after first `Done()` — error from disconnect is lost | MEDIUM |
| 5 | Error callback only cleans up if `clientCount==0` but go2rtc maintains multiple clients | MEDIUM |
| 6 | `CleanSession(true)` makes AutoReconnect useless (subscriptions lost) | LOW |
| 7 | No backoff between connection attempts | LOW |

## Approaches Considered

### Approach A — Singleton MQTT at server level (selected)

Server creates ONE MQTT client per camera, shared by all streams. WebRTCBridge receives the client instead of creating it.

- Eliminates the problem at the root
- MQTT client survives stream on/off cycles
- Slight overhead: persistent TLS connection even without viewers (negligible with 60s keepalive)

### Approach B — Lock on stream with client queue

Add a global mutex that blocks `startStream()` while a previous attempt is in progress. RTSP clients queue.

- Minimal code change but go2rtc could timeout during the wait (~10s)
- Serializes the problem without fixing it

### Approach C — Random clientId + AutoReconnect off

Each `startStream()` uses a unique clientId (nonce suffix). Old clients don't kick new ones.

- Simple two-line fix but creates zombie MQTT connections
- Multiple clients on the same topic = duplicate messages
- Broker may rate-limit

## Selected Design: Approach A — Singleton MQTT

### Architecture

```
RTSPServer
  |-- MQTTManager (singleton, lazy-init per camera)
  |     '-- MQTTClient (long-lived, one per camera)
  |
  '-- getOrCreateStream() --> CameraStream --> WebRTCBridge
                                                  |
                                                  '-- receives MQTTClient from MQTTManager
                                                      (does not create, does not destroy)
```

### New file: `pkg/rtsp/mqtt_manager.go`

Responsibilities:
- Lazy-creates an MQTT client on first request for a camera
- Reuses existing client for subsequent requests
- Reconnects with backoff (2s, 5s, 15s, 30s) if connection dies
- Max 4 retry attempts, then gives up (next RTSP request retries)
- Closes all clients on server shutdown

State machine:
```
DISCONNECTED --> CONNECTING --> CONNECTED --> DISCONNECTED (if dies)
                                   ^                |
                                   '-- reconnect ---'
                                       (backoff)
```

Key rules:
- `AutoReconnect: false` in paho — MQTTManager handles reconnection, not the library
- One client per camera (keyed by deviceId)
- Client persists across stream start/stop cycles
- Only destroyed on server shutdown

Estimated size: ~150 lines.

### Changes to existing files

**`pkg/tuya/mqtt.go`:**
- Set `AutoReconnect(false)` in both constructors
- Add `IsConnected() bool` method

**`pkg/rtsp/server.go`:**
- Add `mqttManager *MQTTManager` field to `RTSPServer`
- Initialize in `Start()`, cleanup in `Stop()`
- Pass MQTT client to `WebRTCBridge` via `getOrCreateStream()` or `CameraStream`

**`pkg/rtsp/bridge.go`:**
- `Start()` receives `*MQTTClient` as parameter instead of creating one
- `Stop()` does NOT call `mqttClient.Stop()` — not its responsibility
- `SendDisconnect()` stays — it cleans the WebRTC session on the device, not the MQTT connection

**No changes to:**
- `mqttCamera.go` — MQTTCameraClient stays identical
- `mobile_api.go` — credential derivation unchanged
- RTSP parsing, SDP, RTP forwarding
- WebRTC (pion, ICE, SDP)

Estimated changes: ~30 lines modified in existing files.

### WebRTCBridge.Stop() — before and after

Before:
```go
wb.cancel()
wb.cameraClient.SendDisconnect()
wb.peerConnection.Close()
wb.mqttClient.Stop()             // <-- REMOVED
wb.rtpForwarder.Stop()
```

After:
```go
wb.cancel()
wb.cameraClient.SendDisconnect()
wb.peerConnection.Close()
// mqttClient NOT stopped — lives in MQTTManager
wb.rtpForwarder.Stop()
```

### Error handling

| Case | Behavior |
|------|----------|
| MQTT dies during active stream | MQTTManager reconnects (backoff). Active WebRTCBridge gets error, tears down. Next RTSP request creates new stream with reconnected client. Brief interruption (~5s). |
| Camera offline | Bridge sends offer, device doesn't respond, waiter times out (~30s). Bridge fails. MQTTManager stays connected. Next RTSP request retries. |
| MQTT broker unreachable | MQTTManager retries 4x with backoff. RTSP clients get error. Next request retries from scratch. |
| go2rtc sends 10 requests in 100ms | All find same stream in `getOrCreateStream()`. One calls `startStream()`. That one gets the existing MQTT client from MQTTManager. Zero new connections. |
| Server shutdown | MQTTManager closes MQTT client. Active stream sends `SendDisconnect()` first. Lullaby stops — acceptable (only during container updates). |

### SessionId handling

Each `MQTTCameraClient` creates a random sessionId. The `consume()` function in `mqtt.go` filters messages by sessionId. When a stream dies and a new one starts:
- Old sessionId is deregistered from `MQTTClient.cameras` map
- New sessionId is registered
- Residual messages for old sessionId are ignored (logged as warning)

This works correctly today, no changes needed.

## Validation Plan

All tests must pass before deploy. This is a baby monitor.

| # | Test | Type | Criteria |
|---|------|------|----------|
| 1 | Single client (ffplay) | Manual | WebRTC establishes first attempt. 1 MQTT connect in logs. |
| 2 | Multi-client (go2rtc/HA) | Manual | Multiple RTSP requests, still 1 MQTT connect total. Video plays. |
| 3 | On-demand cycle (open/close/reopen) | Manual | MQTTManager reuses same client. 1 MQTT connect total. |
| 4 | MQTT drop recovery | Manual | Block port 8883 for 10s. MQTTManager reconnects. Stream resumes. |
| 5 | Camera offline then online | Manual | Fails cleanly when offline. Succeeds after camera reconnects. |
| 6 | Long-running stability (1 hour) | Manual | Zero MQTT reconnects. Zero WebRTC errors. |
| 7 | Lullaby + streaming | Manual | Play/stop lullaby during stream. Close stream, lullaby continues. |
| 8 | Unit: MQTTManager lifecycle | Auto | Test init, get-or-create, reconnect, shutdown. Mock MQTT client. |
| 9 | Unit: Bridge Start without creating MQTT | Auto | Verify Bridge.Start() uses provided client, doesn't create new one. |
| 10 | Unit: Bridge Stop doesn't close MQTT | Auto | Verify mqttClient.Stop() is NOT called during bridge.Stop(). |

## Non-goals

- Multiple camera support (only Erik today, can be added later — MQTTManager already keys by deviceId)
- MQTT connection pooling or load balancing
- Changing the clientId derivation formula
- Fixing the go2rtc retry behavior (that's go2rtc's concern)
