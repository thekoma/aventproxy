# LAN Push DPS — Design Spec

Replace API polling with Tuya LAN protocol push for real-time DPS updates, with API polling as fallback.

## Context

The coordinator currently polls `tuya.m.device.get` every 30 seconds. Testing confirmed that the baby monitor (Tuya protocol 3.3 at `192.168.85.90`) pushes DPS updates over LAN in real-time — temperature, night light, and motion detection were observed arriving instantly via TinyTuya's persistent socket.

Not all DPS codes push reliably over LAN (brightness and volume were not observed), so API polling remains necessary as reconciliation.

## Architecture

```
TuyaLANClient (lan.py)                  PhilipsAventAPI (api.py)
    │                                        │
    │  DPS push (real-time)                  │  GET device (every 120s)
    │                                        │
    └──────────┐                ┌────────────┘
               ▼                ▼
         PhilipsAventCoordinator (coordinator.py)
               │
               │  async_set_updated_data()
               ▼
         All entity platforms (sensor, switch, number, etc.)
```

## New file: `lan.py`

### `TuyaLANClient`

Wraps TinyTuya `Device` in an async interface.

**Constructor**: `(hass, device_id, local_key, on_dps_update callback)`

**Lifecycle**:
- `async start()`: discover device IP via `tinytuya.deviceScan()` on executor, create `Device`, call `updatedps()` to prime the connection, start listener loop
- `async stop()`: cancel listener, close socket

**Listener loop** (runs on executor thread):
- Calls `device.receive()` in a blocking loop (socket timeout 5s)
- When a message with `dps` arrives, schedules `on_dps_update(dps)` on the HA event loop
- On socket error: log, sleep 10s, reconnect (re-scan if needed)
- On shutdown: exit cleanly

**Device discovery**:
- Uses `tinytuya.deviceScan()` filtered by device_id
- Caches discovered IP
- Re-scans on connection failure (device IP may change via DHCP)

**No DPS writing**: writing stays in `api.py` via REST API (more reliable, already working).

## Modified: `coordinator.py`

### Changes to `PhilipsAventCoordinator`

**New constructor params**: `local_key: str | None`

**New attributes**:
- `self._lan_client: TuyaLANClient | None`
- `self._lan_connected: bool = False`

**`async_config_entry_first_refresh()`** behavior unchanged (HA calls this).

**`_async_update_data()`** changes:
- Always fetches from API (reconciliation) but interval changes:
  - LAN connected: `update_interval = 120s`
  - LAN disconnected: `update_interval = 30s` (current behavior)
- Merges API result with any LAN-pushed DPS

**New method `_on_lan_dps_update(dps: dict)`**:
- Called by LAN client when push arrives
- Merges into `self.data` (existing coordinator data dict)
- Calls `self.async_set_updated_data(merged)` to push to all entities immediately

**Startup**: after first API refresh, start LAN client. If LAN fails to connect, log warning and continue with API-only.

**Shutdown**: stop LAN client in `async_will_remove_from_hass` or via entry unload.

## Modified: `__init__.py`

- Extract `localKey` from device discovery API response (already present in the response data)
- Pass `local_key` to coordinator constructor
- Store `local_key` in config entry data for persistence
- Stop LAN client on entry unload

## Modified: `manifest.json`

Add `tinytuya` to `requirements`.

## Testing

### `test_lan.py`
- Mock `tinytuya.Device` and `tinytuya.deviceScan`
- Test: discovery finds device, returns correct IP
- Test: `receive()` with DPS data triggers callback
- Test: socket error triggers reconnect
- Test: stop cancels listener cleanly

### `test_coordinator_lan.py`
- Test: LAN push updates coordinator data immediately
- Test: API polling interval changes based on LAN connection state
- Test: LAN disconnect falls back to 30s polling
- Test: LAN reconnect restores 120s polling

## DPS push coverage (from testing)

| DPS | Name | LAN push observed? |
|-----|------|-------------------|
| 207 | Temperature | Yes |
| 138 | Night light | Yes |
| 134 | Motion detection | Yes |
| 158 | Brightness | Not observed |
| 209 | Volume | Not observed |
| 201 | Lullaby control | Unknown |
| 237 | Privacy mode | Unknown |

DPS not observed via push will be updated via API polling (120s fallback).

## Not in scope

- LAN-based DPS writing (keep using API)
- MQTT cloud integration (confirmed not working for DPS push)
- Multi-device LAN support (single camera for now, same as current)
