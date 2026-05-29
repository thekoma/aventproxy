"""Camera entity for Philips Avent Baby Monitor."""
from __future__ import annotations

import logging

from homeassistant.components.camera import Camera, CameraEntityFeature
from homeassistant.components.ffmpeg import async_get_image as ffmpeg_get_image
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from .const import CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT, DOMAIN, sanitize_rtsp_path
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
    """Camera entity pointing to the WebRTC bridge RTSP stream."""

    _attr_has_entity_name = True
    _attr_name = "Camera"
    _attr_supported_features = CameraEntityFeature.STREAM

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str, bridge_port: int = DEFAULT_BRIDGE_PORT):
        super().__init__()
        self.coordinator = coordinator
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_camera"
        safe_name = sanitize_rtsp_path(coordinator.camera_name, cam_id)
        self._stream_url = f"rtsp://localhost:{bridge_port}/{safe_name}"
        self._attr_device_info = {
            "identifiers": {(DOMAIN, cam_id)},
            "name": coordinator.camera_name,
            "manufacturer": "Philips",
            "model": "Avent SCD973",
        }

    async def stream_source(self) -> str:
        return self._stream_url

    async def async_camera_image(
        self,
        width: int | None = None,
        height: int | None = None,
    ) -> bytes | None:
        """Pull a single JPEG frame from the live RTSP stream.

        Required by the `camera.snapshot` service. Without this override the
        base class raises NotImplementedError. The helper runs ffmpeg under
        the hood; it shares the running bridge stream so it adds no extra
        load on the camera itself.
        """
        try:
            return await ffmpeg_get_image(
                self.hass, self._stream_url, width=width, height=height
            )
        except Exception:
            _LOGGER.exception("ffmpeg snapshot failed for %s", self._stream_url)
            return None
