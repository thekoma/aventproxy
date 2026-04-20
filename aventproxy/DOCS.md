# Avent RTSP Proxy

RTSP proxy for Philips Avent and other Tuya-based cameras that reject
standard RTSP OPTIONS requests.

## How it works

Many Tuya-based cameras expose an RTSP server on port 554 that works
correctly for DESCRIBE, SETUP, and PLAY — but returns `400 Bad Request`
for OPTIONS. Since most RTSP clients (FFmpeg, GStreamer, VLC, Home Assistant)
send OPTIONS first, they fail to connect.

This proxy intercepts OPTIONS requests, returns a fake 200 OK, and forwards
everything else to the camera transparently.

## Configuration

### Cameras

Add one entry per camera. The **name** becomes part of the RTSP URL:

```
rtsp://<addon-ip>:8554/<name>/stream_0
```

- **name**: Unique identifier (used in the URL path)
- **host**: Camera IP address on your LAN
- **port**: Camera RTSP port (default: 554)

### Bind address

- `0.0.0.0` (default): Accept connections from any source (needed for Frigate)
- `127.0.0.1`: Only accept local connections

### Stream paths

Tuya cameras typically expose:

- `/stream_0` — Main stream (1080p)
- `/stream_1` — Sub stream (lower resolution)

## Usage in Home Assistant

After configuring the add-on, add a generic camera:

```yaml
camera:
  - platform: generic
    stream_source: "rtsp://<addon-hostname>:8554/<camera-name>/stream_0"
    name: "Baby Monitor"
```

## Usage with Frigate

Set `bind_address` to `0.0.0.0`, then in Frigate config:

```yaml
cameras:
  baby_monitor:
    ffmpeg:
      inputs:
        - path: "rtsp://aventproxy:8554/baby_monitor/stream_0"
          roles: ["detect", "record"]
```
