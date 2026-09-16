"""Switch entities for Philips Avent Baby Monitor."""
from __future__ import annotations

from typing import Any

from homeassistant.components.switch import SwitchEntity
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import (
    DOMAIN,
    DPS_AWAKE_SWITCH,
    DPS_CRY_DET_SWITCH,
    DPS_MOTION_SWITCH,
    DPS_NIGHT_LIGHT,
    DPS_PRIVACY_MODE,
    DPS_SENSEIQ_SWITCH,
    DPS_SOUND_SWITCH,
)
from .coordinator import PhilipsAventCoordinator
from .entity import build_device_info


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        entities.extend([
            AventSwitch(coordinator, cam_id, DPS_NIGHT_LIGHT, None, "mdi:lightbulb-night", translation_key="night_light"),
            AventSwitch(coordinator, cam_id, DPS_MOTION_SWITCH, None, "mdi:motion-sensor", translation_key="motion_alert"),
            AventSwitch(coordinator, cam_id, DPS_SOUND_SWITCH, None, "mdi:ear-hearing", translation_key="sound_alert"),
            AventEnumSwitch(coordinator, cam_id, DPS_PRIVACY_MODE, None, "mdi:eye-off", translation_key="privacy_mode"),
        ])
        # SenseIQ controls, each gated on its own data point so a monitor that
        # exposes only some of them still gets the right entities.
        dps = coordinator.data or {}
        if DPS_SENSEIQ_SWITCH in dps:
            entities.append(AventSwitch(coordinator, cam_id, DPS_SENSEIQ_SWITCH, None, "mdi:baby-face-outline", translation_key="senseiq"))
        if DPS_AWAKE_SWITCH in dps:
            entities.append(AventSwitch(coordinator, cam_id, DPS_AWAKE_SWITCH, None, "mdi:sleep-off", translation_key="awake_alert"))
        if DPS_CRY_DET_SWITCH in dps:
            entities.append(AventSwitch(coordinator, cam_id, DPS_CRY_DET_SWITCH, None, "mdi:emoticon-cry-outline", translation_key="cry_alert"))
    async_add_entities(entities)


class AventSwitch(CoordinatorEntity, SwitchEntity):
    _attr_has_entity_name = True

    def __init__(
        self, coordinator: PhilipsAventCoordinator, cam_id: str,
        dps_id: str, name: str | None, icon: str,
        *, translation_key: str | None = None,
    ):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._dps_id = dps_id
        if translation_key is not None:
            self._attr_translation_key = translation_key
        else:
            self._attr_name = name
        self._attr_icon = icon
        self._attr_unique_id = f"{cam_id}_{dps_id}"
        self._attr_device_info = build_device_info(coordinator, cam_id)

    @property
    def is_on(self) -> bool | None:
        dps = self.coordinator.data
        if dps and self._dps_id in dps:
            return bool(dps[self._dps_id])
        return None

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self.coordinator.set_dps({self._dps_id: True})

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self.coordinator.set_dps({self._dps_id: False})


class AventEnumSwitch(AventSwitch):
    """Switch for DPS that use "0"/"1" enum instead of bool."""

    @property
    def is_on(self) -> bool | None:
        dps = self.coordinator.data
        if dps and self._dps_id in dps:
            return dps[self._dps_id] == "1"
        return None

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self.coordinator.set_dps({self._dps_id: "1"})

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self.coordinator.set_dps({self._dps_id: "0"})
