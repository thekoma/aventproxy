# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Home Assistant custom integration for Philips Avent SCD973/SCD923 baby monitors. Reverse-engineered Tuya Mobile SDK API provides local video streaming, temperature monitoring, night light control, lullaby playback, and motion/sound alerts. The project has three main parts:

1. **HA integration** (`custom_components/philips_avent/`) — Python, talks to Tuya cloud API with HMAC-SHA256 signed requests
2. **WebRTC-to-RTSP bridge** (`avent-webrtc-bridge/`) — Go binary, converts Tuya WebRTC streams to RTSP on port 8554
3. **HA add-on** (`aventproxy-bridge-addon/`) — Docker container packaging the Go bridge for HA add-on store

## Commands

### Python tests and lint
```bash
PYTHONPATH=. pytest tests/test_philips_avent/ -v          # all tests
PYTHONPATH=. pytest tests/test_philips_avent/test_api_signing.py -v  # single file
ruff check custom_components/ examples/ tools/ --ignore E501         # lint
```

Dependencies for tests: `pip install pytest pycryptodome aiohttp voluptuous`

### Go bridge
```bash
cd avent-webrtc-bridge && go build -o avent-webrtc-bridge . && ./avent-webrtc-bridge --help
```

### Docker
```bash
docker compose up                    # HA + bridge stack
docker compose up aventproxy-bridge  # bridge only
```

## Architecture

```
User's Philips Avent app (APK) ──extract signing key──▶ const.py
                                                          │
HA config flow (email+password+MFA) ─── api.py ──────────▶ Tuya Cloud (a1.tuyaeu.com)
                                                          │
coordinator.py polls device state ◀───────────────────────┘
         │
         ├── camera.py    → RTSP URL from bridge
         ├── sensor.py    → temperature (DPS 207)
         ├── switch.py    → night light, motion/sound alerts, privacy mode
         ├── number.py    → brightness (DPS 158), volume (DPS 209)
         ├── button.py    → lullaby play/pause/next/prev
         ├── select.py    → lullaby track picker (15 tracks)
         └── binary_sensor.py

__init__.py writes philips_avent_bridge.json ──▶ aventproxy-bridge reads it ──▶ RTSP stream
```

### API signing (`api.py`)

Requests are signed with HMAC-SHA256 using a composite key built from 4 APK components (package name, cert hash, embedded key, app secret). The signing string is constructed by sorting parameters, computing MD5, then rearranging blocks. All static credentials live in `const.py`.

### Device Property Set (DPS)

The monitor exposes functionality through numbered DPS codes (e.g., 138=night light, 207=temperature, 201=lullaby control). These are defined in `const.py` and mapped to HA entities.

### Bridge lifecycle

The HA integration writes bridge credentials to a JSON config file. The bridge container (`run.sh`) watches this file, extracts credentials with jq, and (re)starts `avent-webrtc-bridge direct` when config changes.

## CI

GitHub Actions (`.github/workflows/ci.yml`):
- Python tests on 3.11, 3.12, 3.13 with ruff lint
- Go bridge build with Go 1.23
- Docker add-on build verification

Release workflow (`release.yml`): version pattern `YEAR.MONTH.INCREMENT`, multi-arch (amd64+arm64), pushes to `ghcr.io/thekoma/aventproxy-bridge`.

## Style

- Python 3.11+, ruff with `line-length = 120`, E501 ignored in CI
- Go 1.23, static build (`CGO_ENABLED=0`)
- Reverse-engineering notes and methodology in `WHITEPAPER.md`
