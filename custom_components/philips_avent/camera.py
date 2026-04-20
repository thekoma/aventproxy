"""Camera entity for Philips Avent Baby Monitor."""

import logging

from homeassistant.components.camera import Camera
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from .const import DOMAIN
from .coordinator import PhilipsAventCoordinator

_LOGGER = logging.getLogger(__name__)

RTSP_PORT = 8554


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        entities.append(AventCamera(coordinator, cam_id))
    async_add_entities(entities)


class AventCamera(Camera):
    """Camera entity pointing to the WebRTC→RTSP bridge."""

    _attr_has_entity_name = True
    _attr_name = "Camera"

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__()
        self.coordinator = coordinator
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_camera"
        safe_name = coordinator.camera_name.replace(" ", "_")
        self._stream_url = f"rtsp://localhost:{RTSP_PORT}/{safe_name}"
        self._attr_device_info = {
            "identifiers": {(DOMAIN, cam_id)},
            "name": coordinator.camera_name,
            "manufacturer": "Philips",
            "model": "Avent SCD973",
        }

    async def stream_source(self) -> str:
        return self._stream_url

    @property
    def is_streaming(self) -> bool:
        return True
