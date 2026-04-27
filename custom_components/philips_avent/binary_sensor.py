"""Binary sensor entities for Philips Avent Baby Monitor."""
from __future__ import annotations

import logging

from homeassistant.components.binary_sensor import (
    BinarySensorDeviceClass,
    BinarySensorEntity,
)
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.entity_platform import AddEntitiesCallback
from homeassistant.helpers.event import async_call_later
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import DOMAIN, DPS_ALERT_EVENT, DPS_DECIBEL_EVENT, DPS_LULLABY_STATE, DPS_MOTION_SWITCH
from .coordinator import PhilipsAventCoordinator

_LOGGER = logging.getLogger(__name__)

ALERT_CLEAR_SECONDS = 30


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        entities.extend([
            AventLullabyPlaying(coordinator, cam_id),
            AventMotionDetected(coordinator, cam_id),
            AventSoundDetected(coordinator, cam_id),
        ])
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


class AventMotionDetected(CoordinatorEntity, BinarySensorEntity):
    """Turns on when DPS 250 reports 'motion_detection', auto-clears after timeout."""

    _attr_has_entity_name = True
    _attr_name = "Motion Detected"
    _attr_device_class = BinarySensorDeviceClass.MOTION

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_motion_detected"
        self._attr_device_info = {
            "identifiers": {(DOMAIN, cam_id)},
            "name": coordinator.camera_name,
            "manufacturer": "Philips",
            "model": "Avent SCD973",
        }
        self._is_on = False
        self._clear_unsub = None

    @property
    def is_on(self) -> bool:
        return self._is_on

    @callback
    def _handle_coordinator_update(self) -> None:
        dps = self.coordinator.data
        if dps and dps.get(DPS_ALERT_EVENT) == "motion_detection":
            if dps.get(DPS_MOTION_SWITCH):
                self._is_on = True
                self._schedule_clear()
        self.async_write_ha_state()

    @callback
    def _schedule_clear(self) -> None:
        if self._clear_unsub:
            self._clear_unsub()
        self._clear_unsub = async_call_later(
            self.hass, ALERT_CLEAR_SECONDS, self._clear_alert
        )

    @callback
    def _clear_alert(self, _now=None) -> None:
        self._is_on = False
        self._clear_unsub = None
        self.async_write_ha_state()

    async def async_will_remove_from_hass(self) -> None:
        if self._clear_unsub:
            self._clear_unsub()


class AventSoundDetected(CoordinatorEntity, BinarySensorEntity):
    """Turns on when DPS 141 reports 'decibel_upload', auto-clears after timeout."""

    _attr_has_entity_name = True
    _attr_name = "Sound Detected"
    _attr_device_class = BinarySensorDeviceClass.SOUND

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_sound_detected"
        self._attr_device_info = {
            "identifiers": {(DOMAIN, cam_id)},
            "name": coordinator.camera_name,
            "manufacturer": "Philips",
            "model": "Avent SCD973",
        }
        self._is_on = False
        self._clear_unsub = None

    @property
    def is_on(self) -> bool:
        return self._is_on

    @callback
    def _handle_coordinator_update(self) -> None:
        dps = self.coordinator.data
        if dps and dps.get(DPS_DECIBEL_EVENT) == "decibel_upload":
            self._is_on = True
            self._schedule_clear()
        self.async_write_ha_state()

    @callback
    def _schedule_clear(self) -> None:
        if self._clear_unsub:
            self._clear_unsub()
        self._clear_unsub = async_call_later(
            self.hass, ALERT_CLEAR_SECONDS, self._clear_alert
        )

    @callback
    def _clear_alert(self, _now=None) -> None:
        self._is_on = False
        self._clear_unsub = None
        self.async_write_ha_state()

    async def async_will_remove_from_hass(self) -> None:
        if self._clear_unsub:
            self._clear_unsub()
