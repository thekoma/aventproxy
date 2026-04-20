# Philips Avent Baby Monitor — Home Assistant Integration

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Home Assistant integration for Philips Avent SCD973/SCD923 baby monitors, providing local streaming, temperature monitoring, night light control, lullaby playback, and motion/sound alerts.

## Features

| Feature | Entity Type | Description |
|---------|-------------|-------------|
| Live Video | Camera | 1080p H.264 stream via WebRTC→RTSP bridge |
| Temperature | Sensor | Room temperature from built-in sensor |
| Night Light | Switch + Number | On/off + brightness 1-100% |
| Lullabies | Buttons + Number | Play/pause/stop/next/prev + volume |
| Motion Alert | Switch | Motion detection on/off |
| Sound Alert | Switch | Sound detection on/off |
| Privacy Mode | Switch | Camera on/off |

## How It Works

The integration communicates with the camera through the Tuya Mobile SDK API (the same API used by the official Philips Avent Baby Monitor+ app). A WebRTC bridge translates the camera's video stream to standard RTSP, consumable by Home Assistant and any media player.

```
┌─────────────────────────┐
│     Home Assistant       │
│                          │
│  ┌────────────────────┐  │
│  │  Custom Component  │  │   Tuya Cloud
│  │  (config flow,     │◄─┼──► a1.tuyaeu.com
│  │   sensors, lights) │  │   (API calls)
│  └────────────────────┘  │
│                          │
│  ┌────────────────────┐  │
│  │  Bridge Add-on     │  │   Camera
│  │  (WebRTC → RTSP)   │◄─┼──► via STUN/TURN
│  │  :8554             │  │   (video stream)
│  └────────────────────┘  │
│                          │
│  ┌────────────────────┐  │
│  │  Camera Entity     │  │
│  │  rtsp://...:8554   │  │
│  └────────────────────┘  │
└─────────────────────────┘
```

## Installation

### 1. Add-on (WebRTC Bridge)

1. Add this repository to Home Assistant add-on store
2. Install "Philips Avent WebRTC Bridge"
3. The add-on auto-configures after the integration login (step 2)

### 2. Integration (HACS)

1. Add this repository as a custom HACS repository
2. Install "Philips Avent Baby Monitor"
3. Go to Settings → Integrations → Add → "Philips Avent"
4. Enter your email and password (same as the Baby Monitor+ app)
5. Check your email for the verification code, enter it
6. Done — camera and all entities appear automatically

## Setup Flow

The integration uses the same login as the official app:

1. **Email + Password** — your Philips Avent account
2. **Verification Code** — 6-digit code sent to your email (MFA)
3. **Auto-discovery** — cameras are found automatically

No additional configuration needed. The signing credentials are embedded in the integration (same for all users, extracted from the public APK).

## Technical Details

See [WHITEPAPER.md](WHITEPAPER.md) for the complete reverse engineering methodology, including:

- API signing algorithm (HMAC-SHA256)
- Login flow (password + RSA + MFA)
- MQTT credential derivation
- WebRTC signaling
- DPS (Data Point) reference for all camera features

## Camera Data Points

| DPS | Code | Description | Values |
|-----|------|-------------|--------|
| 138 | `bulb_switch` | Night light | on/off |
| 158 | `floodlight_lightness` | Brightness | 1–100 |
| 201 | `play_control` | Lullaby | play/pause/stop/next/prev |
| 207 | `sensor_temperature` | Temperature | °C × 100 |
| 209 | `play_volume` | Volume | 1–100 |
| 134 | `motion_switch` | Motion alert | on/off |
| 139 | `decibel_switch` | Sound alert | on/off |
| 237 | `privacy_switch` | Privacy mode | 0/1 |

Full list: [examples/DPS_REFERENCE.md](examples/DPS_REFERENCE.md)

## For Developers

### Adapting for other Tuya whitelabel cameras

The API signing algorithm is generic to all Tuya Thing SDK apps. To adapt for a different camera:

1. Extract signing key from the APK: `docker run -v app.apk:/input/app.apk apk-key-extractor`
2. Extract the embedded key via Frida (one-time): `python3 tools/extract_signing_key.py`
3. Update `const.py` with the new credentials

See [tools/apk-key-extractor/README.md](tools/apk-key-extractor/README.md) for details.

### Running the bridge manually

```bash
cd tuya-ipc-terminal
go build -o tuya-ipc-terminal .
./tuya-ipc-terminal direct \
  --signing-key "..." \
  --sid "..." \
  --ecode "..." \
  --partner "..." \
  --app-key "..." \
  --device-id "..." \
  --package "..." \
  --camera-id "..." \
  --camera-name "MyCamera" \
  --port 8554
```

### Running tests

```bash
pip install pytest pycryptodome
pytest tests/test_philips_avent/
```

## License

MIT — See [LICENSE](LICENSE)

## Disclaimer

This project is not affiliated with Philips or Tuya. It is created for interoperability purposes, enabling consumers to use their own devices with open home automation platforms. All API access uses the owner's own credentials and the same protocol as the official app.
