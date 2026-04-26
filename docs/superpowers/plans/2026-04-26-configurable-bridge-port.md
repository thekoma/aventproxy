# Configurable Bridge Port Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the WebRTC bridge RTSP port user-configurable via HA options flow, with default 18554, flowing through the bridge JSON config to both the camera entity and the add-on.

**Architecture:** The integration owns the port. User sets it via options flow (Settings > Integrations > Configure). The value is written to `philips_avent_bridge.json` and read by both `camera.py` (for RTSP URL) and `run.sh` (for bridge `--port` flag). Default 18554 everywhere.

**Tech Stack:** Python (HA custom integration), bash (add-on run.sh), YAML (add-on config)

---

### Task 1: Add CONF_BRIDGE_PORT constant and default

**Files:**
- Modify: `custom_components/philips_avent/const.py:84-85`

- [ ] **Step 1: Add the constant**

Add at the end of `const.py`, after the existing `CONF_*` constants:

```python
CONF_BRIDGE_PORT = "bridge_port"
DEFAULT_BRIDGE_PORT = 18554
```

- [ ] **Step 2: Verify lint passes**

Run: `ruff check custom_components/philips_avent/const.py --ignore E501`
Expected: `All checks passed!`

- [ ] **Step 3: Commit**

```bash
git add custom_components/philips_avent/const.py
git commit -m "feat: add CONF_BRIDGE_PORT constant with default 18554"
```

---

### Task 2: Add options flow to config_flow.py

**Files:**
- Modify: `custom_components/philips_avent/config_flow.py`
- Test: `tests/test_philips_avent/test_config_flow.py`

- [ ] **Step 1: Write the test for options flow handler**

Add to `tests/test_philips_avent/test_config_flow.py`:

```python
from const import CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT


class TestOptionsFlow:
    def test_default_bridge_port(self):
        """Default bridge port should be 18554."""
        assert DEFAULT_BRIDGE_PORT == 18554

    def test_bridge_port_from_options(self):
        """Options dict with bridge_port should be readable."""
        options = {"bridge_port": 29000}
        assert options.get(CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT) == 29000

    def test_bridge_port_fallback(self):
        """Empty options should fall back to default."""
        options = {}
        assert options.get(CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT) == 18554

    def test_bridge_port_range_valid(self):
        """Port must be between 1024 and 65535."""
        assert 1024 <= DEFAULT_BRIDGE_PORT <= 65535
```

- [ ] **Step 2: Run tests to verify they pass**

Run: `PYTHONPATH=. pytest tests/test_philips_avent/test_config_flow.py::TestOptionsFlow -v`
Expected: 4 passed

- [ ] **Step 3: Add OptionsFlowHandler and register it**

Add these imports at the top of `config_flow.py`:

```python
from .const import CONF_BRIDGE_PORT, CONF_ECODE, CONF_PARTNER, CONF_SID, CONF_UID, DEFAULT_BRIDGE_PORT, DOMAIN
```

(replaces the existing `from .const import CONF_ECODE, CONF_PARTNER, CONF_SID, CONF_UID, DOMAIN`)

Add `async_get_options_flow` to `PhilipsAventConfigFlow`:

```python
    @staticmethod
    @config_entries.callback
    def async_get_options_flow(config_entry):
        return PhilipsAventOptionsFlowHandler(config_entry)
```

Add the options flow handler class at the end of the file:

```python
class PhilipsAventOptionsFlowHandler(config_entries.OptionsFlow):
    """Handle options flow for Philips Avent."""

    def __init__(self, config_entry: config_entries.ConfigEntry) -> None:
        self.config_entry = config_entry

    async def async_step_init(self, user_input: dict[str, Any] | None = None):
        """Manage the options."""
        if user_input is not None:
            return self.async_create_entry(title="", data=user_input)

        current_port = self.config_entry.options.get(
            CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT
        )

        return self.async_show_form(
            step_id="init",
            data_schema=vol.Schema(
                {
                    vol.Optional(CONF_BRIDGE_PORT, default=current_port): vol.All(
                        int, vol.Range(min=1024, max=65535)
                    ),
                }
            ),
        )
```

- [ ] **Step 4: Verify lint passes**

Run: `ruff check custom_components/philips_avent/config_flow.py --ignore E501`
Expected: `All checks passed!`

- [ ] **Step 5: Commit**

```bash
git add custom_components/philips_avent/config_flow.py tests/test_philips_avent/test_config_flow.py
git commit -m "feat: add options flow for bridge port configuration"
```

---

### Task 3: Add options flow strings to strings.json

**Files:**
- Modify: `custom_components/philips_avent/strings.json`

- [ ] **Step 1: Add options section**

Add the `"options"` key as a sibling to `"config"` in `strings.json`:

```json
{
  "config": {
    ...existing config...
  },
  "options": {
    "step": {
      "init": {
        "title": "Philips Avent Bridge Settings",
        "data": {
          "bridge_port": "Bridge RTSP port"
        },
        "data_description": {
          "bridge_port": "Port the WebRTC bridge listens on (default: 18554)"
        }
      }
    }
  }
}
```

- [ ] **Step 2: Validate JSON**

Run: `python3 -c "import json; json.load(open('custom_components/philips_avent/strings.json')); print('OK')"`
Expected: `OK`

- [ ] **Step 3: Commit**

```bash
git add custom_components/philips_avent/strings.json
git commit -m "feat: add options flow strings for bridge port"
```

---

### Task 4: Write bridge_port into bridge JSON and register update listener

**Files:**
- Modify: `custom_components/philips_avent/__init__.py`

- [ ] **Step 1: Add import for new constants**

Update the import line in `__init__.py`:

```python
from .const import (
    CONF_BRIDGE_PORT, CONF_ECODE, CONF_PARTNER, CONF_SID, DEFAULT_BRIDGE_PORT, DOMAIN,
    TUYA_APP_KEY, TUYA_PACKAGE_NAME, TUYA_SIGNING_KEY,
)
```

- [ ] **Step 2: Extract bridge config writing into a helper function**

Add this function before `async_setup_entry`:

```python
async def _write_bridge_config(hass: HomeAssistant, entry: ConfigEntry, api: PhilipsAventAPI, cameras: list) -> None:
    """Write bridge config JSON for the add-on."""
    bridge_port = entry.options.get(CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT)
    bridge_config = {
        "signing_key": TUYA_SIGNING_KEY,
        "sid": entry.data[CONF_SID],
        "ecode": entry.data.get(CONF_ECODE, ""),
        "partner": entry.data.get(CONF_PARTNER, ""),
        "app_key": TUYA_APP_KEY,
        "device_id": api.device_id,
        "package_name": TUYA_PACKAGE_NAME,
        "bridge_port": bridge_port,
        "cameras": [
            {"camera_id": cam.get("deviceId", cam.get("devId")),
             "camera_name": cam.get("deviceName", cam.get("name", "camera"))}
            for cam in cameras
        ],
    }
    bridge_path = Path(hass.config.path("philips_avent_bridge.json"))
    await hass.async_add_executor_job(
        bridge_path.write_text, json.dumps(bridge_config, indent=2)
    )
    _LOGGER.info("Bridge config written to %s (port: %d)", bridge_path, bridge_port)
```

- [ ] **Step 3: Replace inline bridge config in async_setup_entry**

Replace lines 78-97 (the `# Write bridge config` block) with:

```python
    await _write_bridge_config(hass, entry, api, cameras)
```

- [ ] **Step 4: Add update listener for options changes**

Add at the end of `async_setup_entry`, before `return True`:

```python
    entry.async_on_unload(entry.add_update_listener(_async_options_updated))
```

Add the listener function after `_write_bridge_config`:

```python
async def _async_options_updated(hass: HomeAssistant, entry: ConfigEntry) -> None:
    """Reload integration when options change."""
    await hass.config_entries.async_reload(entry.entry_id)
```

- [ ] **Step 5: Verify lint passes**

Run: `ruff check custom_components/philips_avent/__init__.py --ignore E501`
Expected: `All checks passed!`

- [ ] **Step 6: Commit**

```bash
git add custom_components/philips_avent/__init__.py
git commit -m "feat: write bridge_port to bridge JSON, reload on options change"
```

---

### Task 5: Read port from options in camera.py

**Files:**
- Modify: `custom_components/philips_avent/camera.py`

- [ ] **Step 1: Update camera.py**

Replace the full content of `camera.py` with:

```python
"""Camera entity for Philips Avent Baby Monitor."""
from __future__ import annotations

import logging

from homeassistant.components.camera import Camera, CameraEntityFeature, StreamType
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from .const import CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT, DOMAIN
from .coordinator import PhilipsAventCoordinator

_LOGGER = logging.getLogger(__name__)


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    bridge_port = entry.options.get(CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT)
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        entities.append(AventCamera(coordinator, cam_id, bridge_port))
    async_add_entities(entities)


class AventCamera(Camera):
    """Camera entity pointing to the WebRTC bridge."""

    _attr_has_entity_name = True
    _attr_name = "Camera"
    _attr_supported_features = CameraEntityFeature.STREAM
    _attr_frontend_stream_type = StreamType.WEB_RTC

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str, bridge_port: int = DEFAULT_BRIDGE_PORT):
        super().__init__()
        self.coordinator = coordinator
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_camera"
        safe_name = coordinator.camera_name.replace(" ", "_")
        self._stream_url = f"rtsp://localhost:{bridge_port}/{safe_name}"
        self._attr_device_info = {
            "identifiers": {(DOMAIN, cam_id)},
            "name": coordinator.camera_name,
            "manufacturer": "Philips",
            "model": "Avent SCD973",
        }
        self._cached_image: bytes | None = None

    async def stream_source(self) -> str:
        return self._stream_url

    async def async_camera_image(
        self, width: int | None = None, height: int | None = None
    ) -> bytes | None:
        return self._cached_image
```

- [ ] **Step 2: Verify lint passes**

Run: `ruff check custom_components/philips_avent/camera.py --ignore E501`
Expected: `All checks passed!`

- [ ] **Step 3: Commit**

```bash
git add custom_components/philips_avent/camera.py
git commit -m "feat: read bridge port from options instead of env var"
```

---

### Task 6: Update add-on config and run.sh

**Files:**
- Modify: `aventproxy-bridge-addon/config.yaml`
- Modify: `aventproxy-bridge-addon/run.sh`

- [ ] **Step 1: Update config.yaml**

Remove `ports`, `ports_description` sections. Add `host_network: true`. The file should become:

```yaml
name: "Philips Avent WebRTC Bridge"
description: "WebRTC to RTSP bridge for Philips Avent baby monitors"
version: "2026.4.4-rc5"
slug: "aventproxy_bridge"
image: "ghcr.io/thekoma/aventproxy-bridge"
url: "https://github.com/thekoma/aventproxy"
arch:
  - amd64
  - aarch64
host_network: true
options:
  cameras: []
schema:
  signing_key: str?
  sid: str?
  ecode: str?
  partner: str?
  app_key: str?
  device_id: str?
  package_name: str?
  cameras:
    - camera_id: str?
      camera_name: str?
startup: "application"
boot: "auto"
environment:
  WAIT_FOR_CONFIG: "true"
map:
  - config:rw
```

- [ ] **Step 2: Update run.sh to read port from JSON**

Replace line 53 (`PORT="${BRIDGE_PORT:-8554}"`) with:

```bash
PORT=$(jq -r '.bridge_port // 18554' "$CONFIG_PATH")
```

- [ ] **Step 3: Commit**

```bash
git add aventproxy-bridge-addon/config.yaml aventproxy-bridge-addon/run.sh
git commit -m "feat: add-on uses host_network, reads bridge_port from JSON"
```

---

### Task 7: Update docker-compose.yml for local dev

**Files:**
- Modify: `docker-compose.yml`

- [ ] **Step 1: Update BRIDGE_PORT default**

Change `BRIDGE_PORT=8556` to `BRIDGE_PORT=18554` in the `aventproxy-bridge` service environment, and in the `homeassistant` service environment.

```yaml
services:
  homeassistant:
    image: ghcr.io/home-assistant/home-assistant:stable
    restart: unless-stopped
    network_mode: host
    privileged: true
    volumes:
      - ha_config:/config
      - ./custom_components/philips_avent:/config/custom_components/philips_avent:ro
    environment:
      - TZ=Europe/Rome
      - BRIDGE_PORT=18554

  aventproxy-bridge:
    build:
      context: .
      dockerfile: docker/Dockerfile.bridge
    restart: unless-stopped
    network_mode: host
    volumes:
      - ha_config:/config:ro
    environment:
      - WAIT_FOR_CONFIG=true
      - BRIDGE_PORT=18554
    depends_on:
      - homeassistant

volumes:
  ha_config:
```

- [ ] **Step 2: Commit**

```bash
git add docker-compose.yml
git commit -m "chore: update default BRIDGE_PORT to 18554 in docker-compose"
```

---

### Task 8: Run full test suite and lint

**Files:** None (validation only)

- [ ] **Step 1: Run all Python tests**

Run: `PYTHONPATH=. pytest tests/test_philips_avent/ -v`
Expected: All tests pass

- [ ] **Step 2: Run full lint**

Run: `ruff check custom_components/ examples/ tools/ --ignore E501`
Expected: `All checks passed!`

- [ ] **Step 3: Build Docker image**

Run: `docker compose build aventproxy-bridge`
Expected: Build succeeds with new port defaults
