# Philips Avent Baby Monitor — Home Assistant Integration

<p align="center">
  <img src="custom_components/philips_avent/brand/logo.png" alt="Philips AVENT" width="300">
</p>

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![CI](https://github.com/thekoma/aventproxy/actions/workflows/ci.yml/badge.svg)](https://github.com/thekoma/aventproxy/actions/workflows/ci.yml)
[![HACS](https://img.shields.io/badge/HACS-Custom-41BDF5.svg)](https://github.com/hacs/integration)

Home Assistant integration for Philips Avent SCD973/SCD923 baby monitors, providing local streaming, temperature monitoring, night light control, lullaby playback, and motion/sound alerts.

## Features

| Feature | Entity Type | Description |
|---------|-------------|-------------|
| 📹 Live Video | Camera | 1080p H.264 stream via WebRTC→RTSP bridge |
| 🌡️ Temperature | Sensor | Room temperature from built-in sensor |
| 💡 Night Light | Switch + Number | On/off + brightness 1-100% |
| 🎵 Lullabies | Buttons + Number | Play/pause/stop/next/prev + volume |
| 🏃 Motion Alert | Switch | Motion detection on/off |
| 🔊 Sound Alert | Switch | Sound detection on/off |
| 🔒 Privacy Mode | Switch | Camera on/off |

## Installation

### Integration (HACS)

[![Open HACS repository](https://my.home-assistant.io/badges/hacs_repository.svg)](https://my.home-assistant.io/redirect/hacs_repository/?owner=thekoma&repository=aventproxy&category=integration)

Or manually:

1. Open HACS → Integrations → Three dots menu → Custom repositories
2. Add `https://github.com/thekoma/aventproxy` as category **Integration**
3. Install "Philips Avent Baby Monitor"
4. Restart Home Assistant

### Add-on (WebRTC Bridge)

[![Add add-on repository](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2Fthekoma%2Faventproxy)

Or manually:

1. Settings → Add-ons → Add-on Store → Three dots → Repositories
2. Add `https://github.com/thekoma/aventproxy`
3. Install "Philips Avent WebRTC Bridge"

### Setup

[![Add integration](https://my.home-assistant.io/badges/config_flow_start.svg)](https://my.home-assistant.io/redirect/config_flow_start/?domain=philips_avent)

1. Go to Settings → Integrations → Add Integration → **Philips Avent Baby Monitor**
2. Enter your email and password (same as the Baby Monitor+ app)
3. Check your email for the 6-digit verification code
4. Enter the code — done!

The integration discovers your cameras automatically and creates all entities. The bridge add-on starts streaming.

## How It Works

The integration uses the same Tuya Mobile SDK API as the official Philips Avent Baby Monitor+ app. Authentication uses the same password + MFA flow — your traffic is indistinguishable from the real app.

```
┌─────────────────────────────┐
│       Home Assistant         │
│                              │
│  ┌────────────────────────┐  │
│  │  Custom Integration    │  │   Tuya Cloud
│  │  (login, sensors,      │◄─┼──► a1.tuyaeu.com
│  │   lights, lullabies)   │  │   (API calls)
│  └────────────────────────┘  │
│                              │
│  ┌────────────────────────┐  │
│  │  Bridge Add-on         │  │   Camera
│  │  (WebRTC → RTSP)       │◄─┼──► via STUN/TURN
│  │  :8554                 │  │   (1080p H.264)
│  └────────────────────────┘  │
│                              │
│  ┌────────────────────────┐  │
│  │  Camera Entity         │  │
│  │  rtsp://...:8554/Name  │  │
│  └────────────────────────┘  │
└─────────────────────────────┘
```

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

Full reference: [examples/DPS_REFERENCE.md](examples/DPS_REFERENCE.md)

## Technical Background

This integration was built through reverse engineering of the Tuya Mobile SDK API. The complete methodology is documented in [WHITEPAPER.md](WHITEPAPER.md), including:

- API signing algorithm (HMAC-SHA256 with composite key)
- Password + MFA login flow
- MQTT credential derivation for WebRTC signaling
- 10 documented failures and dead ends

## For Developers

### Adapting for other Tuya whitelabel cameras

The signing algorithm is generic to all Tuya Thing SDK apps. See [tools/apk-key-extractor/](tools/apk-key-extractor/) for automated key extraction from any APK.

### Running tests

```bash
python3 -m venv .venv && source .venv/bin/activate
pip install pytest pycryptodome aiohttp voluptuous homeassistant
PYTHONPATH=. pytest tests/test_philips_avent/ -v
```

### Running the bridge manually

```bash
cd avent-webrtc-bridge && go build -o avent-webrtc-bridge .
./avent-webrtc-bridge direct --help
```

## Acknowledgments

The WebRTC bridge (`avent-webrtc-bridge/`) is forked from [tuya-ipc-terminal](https://github.com/seydx/tuya-ipc-terminal) by seydx, which itself uses WebRTC and codec utilities from [go2rtc](https://github.com/AlexxIT/go2rtc) by AlexxIT. The bridge has since diverged significantly with Philips Avent-specific features including RTP timestamp rebasing, SPS/PPS injection, RTSP backchannel audio, and MQTT-based WebRTC signaling via the Tuya Mobile SDK.

## License

MIT — See [LICENSE](LICENSE)

## Disclaimer

This project is not affiliated with, endorsed by, or connected to Koninklijke Philips N.V. or any of its subsidiaries. "Philips" and "AVENT" are registered trademarks of Koninklijke Philips N.V. The Philips AVENT logo and branding are used solely for identification purposes to help users recognize which device this integration supports. All API access uses the owner's own credentials and the same protocol as the official app.
