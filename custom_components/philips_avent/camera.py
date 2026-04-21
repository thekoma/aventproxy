"""Camera entity for Philips Avent Baby Monitor."""

import logging
import os

from homeassistant.components.camera import Camera, CameraEntityFeature, StreamType
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from .const import DOMAIN
from .coordinator import PhilipsAventCoordinator

_LOGGER = logging.getLogger(__name__)

RTSP_PORT_DEFAULT = 8554
BRIDGE_PORT_ENV = "BRIDGE_PORT"


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    rtsp_port = int(
        entry.options.get("rtsp_port")
        or os.environ.get(BRIDGE_PORT_ENV)
        or RTSP_PORT_DEFAULT
    )
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        entities.append(AventCamera(coordinator, cam_id, rtsp_port))
    async_add_entities(entities)


class AventCamera(Camera):
    """Camera entity pointing to the WebRTC→RTSP bridge."""

    _attr_has_entity_name = True
    _attr_name = "Camera"
    _attr_supported_features = CameraEntityFeature.STREAM
    _attr_frontend_stream_type = StreamType.WEB_RTC

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str, rtsp_port: int = RTSP_PORT_DEFAULT):
        super().__init__()
        self.coordinator = coordinator
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_camera"
        safe_name = coordinator.camera_name.replace(" ", "_")
        self._stream_url = f"rtsp://localhost:{rtsp_port}/{safe_name}"
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
