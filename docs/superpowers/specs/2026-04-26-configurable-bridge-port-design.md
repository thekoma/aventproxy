# Configurable Bridge Port

## Problem

The WebRTC bridge RTSP port is hardcoded to 8554, which conflicts with go2rtc on production HA. The port needs to be user-configurable from a single place, with the value flowing to both the camera entity and the bridge add-on.

## Design

The integration owns the port configuration. The user sets it via an HA options flow. The value is written to `philips_avent_bridge.json` (already the contract between integration and add-on). The add-on reads it from there.

Default port: `18554`.

### Data flow

```
Options flow (UI)
  -> entry.options["bridge_port"] = 18554
  -> __init__.py writes to philips_avent_bridge.json: {"bridge_port": 18554, ...}
  -> camera.py reads entry.options["bridge_port"] for RTSP URL
  -> run.sh reads bridge_port from JSON, passes --port to bridge binary
```

### File changes

#### `config_flow.py`
- Add `OptionsFlowHandler` class with single field `bridge_port` (int, default 18554, range 1024-65535).
- Register via `async_get_options_flow` on the config flow class.

#### `__init__.py`
- Read `entry.options.get("bridge_port", 18554)` when building bridge config.
- Write `bridge_port` field into `philips_avent_bridge.json`.
- Register `update_listener` to rewrite bridge JSON and reload when options change.

#### `camera.py`
- Read port from `entry.options.get("bridge_port", 18554)`.
- Remove `BRIDGE_PORT_ENV` and `os.environ` fallback.
- Remove `os` import.

#### `strings.json`
- Add `options` section with step/field labels for the options flow.

#### `aventproxy-bridge-addon/config.yaml`
- Remove `ports` and `ports_description` sections.
- Add `host_network: true`.

#### `aventproxy-bridge-addon/run.sh`
- Read `bridge_port` from the JSON config: `PORT=$(jq -r '.bridge_port // 18554' "$CONFIG_PATH")`.
- Remove hardcoded `BRIDGE_PORT` env var fallback.

#### `docker-compose.yml`
- Update `BRIDGE_PORT` env var default from `8556` to `18554` for local dev consistency.

### What does NOT change
- Login/auth flow unchanged.
- All entity types unchanged.
- Bridge binary CLI interface unchanged (already accepts `--port`).
- CI workflows unchanged.

### Testing
- Existing tests continue to pass (port is optional with default).
- Options flow testable with standard HA test patterns.

### Rollback
- If `bridge_port` is absent from options or JSON, everything falls back to 18554.
- Old bridge JSON files without `bridge_port` keep working (run.sh defaults to 18554).
