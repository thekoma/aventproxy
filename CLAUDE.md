# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Home Assistant custom integration for Philips Avent SCD973/SCD923 baby monitors. Reverse-engineered Tuya Mobile SDK API provides local video streaming, temperature monitoring, night light control, lullaby playback, and motion/sound alerts. The project has three main parts:

1. **HA integration** (`custom_components/philips_avent/`) — Python, talks to Tuya cloud API with HMAC-SHA256 signed requests
2. **WebRTC-to-RTSP bridge** (`avent-webrtc-bridge/`) — Go binary, converts Tuya WebRTC streams to RTSP on port 8554
3. **HA add-on** (`aventproxy-bridge-addon/`) — Docker container packaging the Go bridge for HA add-on store.
   Three cards, one per channel: `aventproxy-bridge-addon/` (stable), `-beta/` (rc and beta) and
   `-dev/` (builds off main). The release workflow bumps exactly one `config.yaml` per run, chosen by
   release type, so a dev build never moves the card rc testers follow. The cards therefore sit at
   different versions on purpose. A dev run publishes a tag and an image but **no GitHub release**,
   which is what keeps it out of HACS for anyone who merely enabled beta versions. A bridge older
   than the integration can miss config fields (`api_host` landed in 2026.7.0-rc2, `talkback` in
   2026.7.0-dev1), so testers must run the card matching their integration channel. Both cards pull the same
   published image, which is built from `aventproxy-bridge-addon/` only, so the beta folder's
   `Dockerfile` and `run.sh` are never built. Keep them byte-identical to the stable ones anyway;
   the beta copy had silently drifted to a pre-multi-entry `run.sh`

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
- Go bridge: `gofmt` check, build and `go test ./...` with Go 1.26
- Docker add-on build verification

Release workflow (`release.yml`): version pattern `YEAR.MONTH.INCREMENT`, multi-arch (amd64+arm64), pushes to `ghcr.io/thekoma/aventproxy-bridge`. Types: `release`, `rc`, `beta`, `dev`. Only `dev` skips the GitHub release step, via the `publish_release` output. Pushes touching `.github/workflows/` need the SSH remote, since the HTTPS token has no `workflow` scope.

## Style

- Python 3.11+, ruff with `line-length = 120`, E501 ignored in CI
- The enforced rule set is pinned in `pyproject.toml` (`[tool.ruff.lint] select`): pycodestyle
  errors, pyflakes, `I`, `UP`, `SIM`, `PERF`, `PLW`, `RUF`, `EXE`, `BLE`, `S110`. Pinning it keeps a
  new ruff release from failing CI on untouched code. `BLE001` means a broad `except Exception`
  needs a `# noqa: BLE001 - <reason>`; the existing ones sit on teardown paths, callback boundaries
  and tinytuya calls. Scripts under `tools/` and `examples/` are exempt via per-file-ignores
- Go 1.23, static build (`CGO_ENABLED=0`). CI gates `gofmt -l` and runs `go test ./...`, so format
  before pushing: `docker run --rm -v $PWD:/src -w /src golang:1.26-bookworm gofmt -w .` from
  `avent-webrtc-bridge/`
- Reverse-engineering notes and methodology in `WHITEPAPER.md`
