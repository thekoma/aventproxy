"""Number entities for Philips Avent Baby Monitor."""
from __future__ import annotations

from homeassistant.components.number import NumberEntity
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import (
    DOMAIN,
    DPS_AWAKE_DELAY,
    DPS_BRIGHTNESS,
    DPS_LULLABY_VOLUME,
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
            AventNumber(coordinator, cam_id, DPS_BRIGHTNESS, None, "mdi:brightness-6", 1, 100, 1, "%", translation_key="night_light_brightness"),
            AventNumber(coordinator, cam_id, DPS_LULLABY_VOLUME, None, "mdi:volume-medium", 1, 100, 1, "%", translation_key="lullaby_volume"),
        ])
        # SenseIQ: delay before the "baby awake" alert fires (seconds).
        dps = coordinator.data or {}
        if DPS_AWAKE_DELAY in dps:
            entities.append(
                AventNumber(coordinator, cam_id, DPS_AWAKE_DELAY, None, "mdi:timer-outline", 0, 600, 30, "s", translation_key="awake_alert_delay")
            )
    async_add_entities(entities)


class AventNumber(CoordinatorEntity, NumberEntity):
    _attr_has_entity_name = True

    def __init__(
        self, coordinator: PhilipsAventCoordinator, cam_id: str,
        dps_id: str, name: str | None, icon: str,
        min_val: float, max_val: float, step: float, unit: str,
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
        self._attr_native_min_value = min_val
        self._attr_native_max_value = max_val
        self._attr_native_step = step
        self._attr_native_unit_of_measurement = unit
        self._attr_unique_id = f"{cam_id}_{dps_id}"
        self._attr_device_info = build_device_info(coordinator, cam_id)

    @property
    def native_value(self) -> float | None:
        dps = self.coordinator.data
        if dps and self._dps_id in dps:
            return float(dps[self._dps_id])
        return None

    async def async_set_native_value(self, value: float) -> None:
        await self.coordinator.set_dps({self._dps_id: int(value)})
