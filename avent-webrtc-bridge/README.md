# Avent WebRTC Bridge

Go binary that connects to Philips Avent baby monitors via Tuya WebRTC signaling and re-publishes the video stream as a local RTSP endpoint.

Originally forked from [tuya-ipc-terminal](https://github.com/seydx/tuya-ipc-terminal) by seydx, which itself incorporates WebRTC and codec utilities from [go2rtc](https://github.com/AlexxIT/go2rtc) by AlexxIT. The bridge has since diverged significantly with Philips Avent-specific functionality.

## What it does

1. Authenticates with Tuya cloud using the Mobile SDK API (same as the Philips Baby Monitor+ app)
2. Connects to the MQTT broker for WebRTC signaling
3. Establishes a peer connection with the camera
4. Receives H.264 RTP packets, rebases timestamps to wall clock, injects SPS/PPS before keyframes
5. Serves the stream as RTSP on a configurable port

## Usage

```bash
go build -o avent-webrtc-bridge .

avent-webrtc-bridge direct \
    --signing-key "..." \
    --sid "..." \
    --ecode "..." \
    --partner "..." \
    --app-key "..." \
    --device-id "..." \
    --package "com.philips.ph.babymonitorplus" \
    --camera-id "..." \
    --camera-name "MyCamera" \
    --port 38554
```

The stream is then available at `rtsp://localhost:38554/MyCamera`.

You don't normally run this manually. The HA add-on reads credentials from `philips_avent_bridge.json` (written by the HA integration) and starts the bridge automatically.

## Key differences from upstream

- **RTP timestamp rebasing** to 90kHz (video) and 8kHz (audio) wall clock, fixing 100% frame drop in browsers
- **SPS/PPS injection** before IDR keyframes for reliable stream startup
- **MQTT singleton** preventing broker kick loops from duplicate client IDs
- **Dead client cleanup** removing stale RTSP connections
- **Tuya Mobile SDK API** authentication (HMAC-SHA256 signed requests) instead of browser portal
- **RTSP backchannel audio** support for two-way communication

## Building

Requires Go 1.23+.

```bash
go build -o avent-webrtc-bridge .
```

Static build for containers:

```bash
CGO_ENABLED=0 go build -o avent-webrtc-bridge .
```

## License

MIT
