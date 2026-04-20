"""Binary sensor entities for Philips Avent Baby Monitor."""

from homeassistant.components.binary_sensor import (
    BinarySensorDeviceClass,
    BinarySensorEntity,
)
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import DOMAIN, DPS_LULLABY_STATE
from .coordinator import PhilipsAventCoordinator


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        entities.append(AventLullabyPlaying(coordinator, cam_id))
    async_add_entities(entities)


class AventLullabyPlaying(CoordinatorEntity, BinarySensorEntity):
    _attr_has_entity_name = True
    _attr_name = "Lullaby Playing"
    _attr_icon = "mdi:music"
    _attr_device_class = BinarySensorDeviceClass.RUNNING

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_lullaby_playing"
        self._attr_device_info = {
            "identifiers": {(DOMAIN, cam_id)},
            "name": coordinator.camera_name,
            "manufacturer": "Philips",
            "model": "Avent SCD973",
        }

    @property
    def is_on(self) -> bool | None:
        dps = self.coordinator.data
        if dps and DPS_LULLABY_STATE in dps:
            return dps[DPS_LULLABY_STATE] == "playing"
        return None
