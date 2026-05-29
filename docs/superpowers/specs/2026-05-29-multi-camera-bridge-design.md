# Multi-Camera Bridge

## Problem

When a Tuya account has more than one Philips Avent monitor (issue #35), the HA integration discovers all cameras and creates entities for each, but the add-on serves only `cameras[0]`. The remaining cameras have no working video stream. The limit is in two places:

- `aventproxy-bridge-addon/run.sh:58-59` — passes only the first camera to the binary.
- `avent-webrtc-bridge/cmd/direct/direct.go` — accepts only `--camera-id`/`--camera-name` single-value flags.

The RTSP server underneath is already multi-camera: `pkg/rtsp/server.go` holds a `streams map[string]*CameraStream` and `findCamera(path)` iterates the storage. Only the front-end wiring is single-camera.

## Goals

- One add-on container serves every camera in the account simultaneously, each on its own RTSP path.
- The user does nothing beyond the existing config flow (email + password + MFA). Adding a second monitor in the Avent app eventually reaches HA via the same discovery path.
- No backward-incompatible change to the JSON contract or to `direct` (which stays a single-camera dev/test tool).

## Non-goals

- Hot-reload without restart. The existing md5 watcher in `run.sh` already restarts the bridge on config change; that is fast enough for adding/removing cameras (~3 s blip).
- Dynamic refresh of the camera list without recreating the config entry. If a user adds a monitor in the app, the integration today picks it up only by re-running the config flow. Improving that is a separate issue.
- A UI to manage cameras. HA renders the discovered entities automatically.

## Design

A new subcommand `addon` on the bridge binary reads the JSON file already written by the integration, registers every camera with the existing RTSP server, and serves them all on one port. `run.sh` becomes one line.

### Data flow

```
config_flow                  __init__.py                       run.sh                   avent-webrtc-bridge
─────────────                ─────────────────                 ──────────               ──────────────────────
discover N cams              writes JSON                       glob+legacy              addon --config FILE
   |                            cameras: [{id, name}, ...]      lookup
   v                            bridge_port: 38554                 |
entry.data["cameras"]                                              v
                                                              exec ... addon
                                                                                        |  parse JSON
                                                                                        |  verify API once
                                                                                        |  GetUserInfo() -> userKey
                                                                                        |  register N CameraInfo
                                                                                        |  start RTSPServer(port)
                                                                                        v
                                                                              rtsp://host:38554/<sanitized-name1>
                                                                              rtsp://host:38554/<sanitized-name2>
                                                                              ...
```

### JSON contract (unchanged)

The shape written by `_write_bridge_config` in `__init__.py` is already the source of truth. The new subcommand consumes it verbatim:

```json
{
  "signing_key": "...",
  "sid": "...",
  "ecode": "...",
  "partner": "...",
  "app_key": "...",
  "device_id": "...",
  "package_name": "...",
  "bridge_port": 38554,
  "cameras": [
    {"camera_id": "abc123", "camera_name": "Erik"},
    {"camera_id": "def456", "camera_name": "Anna"}
  ]
}
```

### CLI contract

```
avent-webrtc-bridge addon --config /config/philips_avent_bridge_<entry_id>.json
```

One required flag. Every other parameter lives in the JSON. There is no second representation of the schema; the bridge unmarshals straight into a Go struct that mirrors the Python writer.

### Path sanitization

`camera_name` is mapped to an RTSP path with the same rule used today in `direct.go`:

```
sanitized := "/" + strings.ReplaceAll(name, " ", "_")
```

If two cameras sanitize to the same path (unusual — the Avent app encourages distinct names), the second one falls back to `<sanitized>_<camera_id[:6]>` and a `WARN` is logged. The integration side (`camera.py`) uses the same rule so the URL it stores matches what the bridge serves. A shared helper avoids drift.

### Startup

1. Parse and validate the JSON.
2. Build `MobileSDKClient` with account-scoped credentials.
3. Verify API access with `smartlife.p.time.get` (one call, not per-camera).
4. Fetch `GetUserInfo()` once; derive `userKey` shared across all cameras.
5. Build the full `[]storage.CameraInfo` list and call `storageManager.UpdateCamerasForUser(userKey, list)` once.
6. Start `RTSPServer(bridge_port)`.
7. Log `Serving N cameras on rtsp://localhost:<port>/{path1, path2, ...}`.
8. Wait for SIGINT/SIGTERM, then `server.Stop()`.

### Error handling

- **Fatal at startup** (JSON unparseable, missing credentials, API verification fails, port bind fails): exit 1 with a clear message. These would fail with one camera too.
- **Empty `cameras` array**: exit 1 — there is nothing to serve.
- **Single camera entry missing `camera_id`**: `WARN` and skip that entry; continue with the rest.
- **WebRTC init failures**: already lazy in the server. The stream is opened only when a client `DESCRIBE`/`PLAY`s its path. One unreachable camera does not block the others.

### Logging

```
INFO Serving 2 cameras on port 38554: /Erik /Anna
INFO Camera registered: id=abc123 name=Erik path=/Erik
INFO Camera registered: id=def456 name=Anna path=/Anna
WARN Path collision on /Baby, falling back to /Baby_def456
WARN Camera config invalid: missing camera_id, skipping entry index=2
```

## File changes

### `avent-webrtc-bridge/cmd/addon/addon.go` (new)

Sibling of `cmd/direct/direct.go`. Defines `NewAddonCmd()` returning a Cobra command with one required flag `--config`. The `RunE` implements the startup sequence above.

### `avent-webrtc-bridge/cmd/addon/addon_test.go` (new)

Table-driven unit tests for JSON parsing and path sanitization:
- valid JSON with N cameras → N `CameraInfo` entries with expected paths
- malformed JSON → error with file path in the message
- empty `cameras` → specific error
- entry missing `camera_id` → skipped, warning recorded
- two cameras with colliding sanitized names → second one gets the `_<idprefix>` suffix

### `avent-webrtc-bridge/cmd/root.go`

Register the new subcommand alongside `NewDirectCmd()`.

### `avent-webrtc-bridge/pkg/storage/path.go` (new, small)

Single source of truth for path sanitization, exported as `SanitizeRTSPPath(name, id string) string`. `direct.go` switches to use it.

### `aventproxy-bridge-addon/run.sh`

Drop the per-camera `jq` extraction and the explicit `direct` flag list. The body becomes:

```bash
exec avent-webrtc-bridge addon --config "$CONFIG_PATH"
```

The config discovery (glob + legacy + `WAIT_FOR_CONFIG`) and the md5 watcher stay as they are.

### `custom_components/philips_avent/camera.py`

Verify the RTSP URL builder uses the same sanitization rule as the bridge. If a discrepancy exists, add a helper in `const.py` (`def sanitize_rtsp_path(name: str, cam_id: str) -> str`) and use it both in `camera.py` and in any test that asserts the URL shape.

### `custom_components/philips_avent/__init__.py`

No structural change. The existing `_write_bridge_config` already writes the full `cameras` array.

### `README.md`

Update the "How It Works" diagram to show one bridge process exposing N RTSP paths, and add a sentence in the cameras section that multiple monitors on one account are served from the same port at different paths.

## What does NOT change

- `pkg/rtsp/server.go`, `pkg/rtsp/bridge.go`, `pkg/storage` — already multi-camera capable.
- `cmd/direct/direct.go` — stays the single-camera dev/test tool. Its only edit is to switch to the shared path helper.
- The bridge JSON schema — no new required fields.
- `config_flow.py`, `coordinator.py`, all entity files except possibly `camera.py` — unaffected.
- The `host_network: true` add-on setting and the firewall posture — one port still suffices.
- CI workflows — unchanged.

## Testing

- **Go unit tests** as listed under `cmd/addon/addon_test.go`. Run with `go test ./cmd/addon/...`.
- **Local CI mirror** ([[feedback-local-ci]]): `golang:1.26-bookworm` for `go test` and `go build`; full add-on Docker build via the existing `aventproxy-bridge-addon/Dockerfile`; smoke run of `bridge addon --config <fixture>` with a fake 2-camera JSON to confirm it starts the RTSP server and binds the port. Real WebRTC streaming is not part of CI — no test devices available.
- **Python side**: no new tests strictly required. If the path helper is added to `const.py`, a small unit test asserts it matches the Go helper for representative inputs (spaces, accents, colliding names).

## Migration

Existing single-camera users are unaffected: the JSON they already have works as a 1-element `cameras` array, and the server behavior is identical. No version gate, no data migration, no manual user step.

## References

- Issue: https://github.com/thekoma/aventproxy/issues/35
- Related prior work: commit `4e548a0` (multi-config-entry filenames) — solved the multi-account collision; this spec handles multi-camera within one account.
- Upstream multi-camera storage primitive: `avent-webrtc-bridge/pkg/storage` (already accepts a slice).
