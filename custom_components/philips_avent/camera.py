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
