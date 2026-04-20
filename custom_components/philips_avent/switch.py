"""Switch entities for Philips Avent Baby Monitor."""

from typing import Any

from homeassistant.components.switch import SwitchEntity
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import (
    DOMAIN,
    DPS_MOTION_SWITCH,
    DPS_NIGHT_LIGHT,
    DPS_PRIVACY_MODE,
    DPS_SOUND_SWITCH,
)
from .coordinator import PhilipsAventCoordinator


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        entities.extend([
            AventSwitch(coordinator, cam_id, DPS_NIGHT_LIGHT, "Night Light", "mdi:lightbulb-night"),
            AventSwitch(coordinator, cam_id, DPS_MOTION_SWITCH, "Motion Alert", "mdi:motion-sensor"),
            AventSwitch(coordinator, cam_id, DPS_SOUND_SWITCH, "Sound Alert", "mdi:ear-hearing"),
            AventEnumSwitch(coordinator, cam_id, DPS_PRIVACY_MODE, "Privacy Mode", "mdi:eye-off"),
        ])
    async_add_entities(entities)


class AventSwitch(CoordinatorEntity, SwitchEntity):
    _attr_has_entity_name = True

    def __init__(
        self, coordinator: PhilipsAventCoordinator, cam_id: str,
        dps_id: str, name: str, icon: str,
    ):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._dps_id = dps_id
        self._attr_name = name
        self._attr_icon = icon
        self._attr_unique_id = f"{cam_id}_{dps_id}"
        self._attr_device_info = {
            "identifiers": {(DOMAIN, cam_id)},
            "name": coordinator.camera_name,
            "manufacturer": "Philips",
            "model": "Avent SCD973",
        }

    @property
    def is_on(self) -> bool | None:
        dps = self.coordinator.data
        if dps and self._dps_id in dps:
            return bool(dps[self._dps_id])
        return None

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self.coordinator.api.set_dps(self._cam_id, {self._dps_id: True})
        await self.coordinator.async_request_refresh()

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self.coordinator.api.set_dps(self._cam_id, {self._dps_id: False})
        await self.coordinator.async_request_refresh()


class AventEnumSwitch(AventSwitch):
    """Switch for DPS that use "0"/"1" enum instead of bool."""

    @property
    def is_on(self) -> bool | None:
        dps = self.coordinator.data
        if dps and self._dps_id in dps:
            return dps[self._dps_id] == "1"
        return None

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self.coordinator.api.set_dps(self._cam_id, {self._dps_id: "1"})
        await self.coordinator.async_request_refresh()

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self.coordinator.api.set_dps(self._cam_id, {self._dps_id: "0"})
        await self.coordinator.async_request_refresh()
