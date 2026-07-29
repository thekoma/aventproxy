"""Binary sensor entities for Philips Avent Baby Monitor."""
from __future__ import annotations

import logging
import time

from homeassistant.components.binary_sensor import (
    BinarySensorDeviceClass,
    BinarySensorEntity,
)
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.entity_platform import AddEntitiesCallback
from homeassistant.helpers.event import async_call_later
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import (
    DOMAIN,
    DPS_ALARM_RECORD,
    DPS_ALERT_EVENT,
    DPS_DECIBEL_EVENT,
    DPS_LULLABY_STATE,
    DPS_MOTION_SWITCH,
)
from .coordinator import PhilipsAventCoordinator
from .entity import build_device_info
from .events import is_new_event, motion_event_timestamp, sound_event_timestamp

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
        self._attr_device_info = build_device_info(coordinator, cam_id)

    @property
    def is_on(self) -> bool | None:
        dps = self.coordinator.data
        if dps and DPS_LULLABY_STATE in dps:
            return dps[DPS_LULLABY_STATE] == "playing"
        return None


class AventMotionDetected(CoordinatorEntity, BinarySensorEntity):
    """Motion alerts, from whichever DPS the monitor reports them on.

    Two mechanisms, because the family differs (issues #40, #42, #59, #61):

    - DPS 250 set to `motion_detection`, used by the SCD973 and SCD923 family.
      It is an event that the coordinator merges into persistent state, so only a
      payload that arrived since the last look counts; otherwise every cloud poll
      replays the last alert (the same defect fixed for sound in #65).
    - DPS 212, the alarm record the SCD951 and SCD953 family posts instead, which
      carries its own timestamp. That timestamp is what makes it usable: the value
      stays in device state, so freshness comes from the stamp rather than from
      catching the push.
    """

    _attr_has_entity_name = True
    _attr_name = "Motion Detected"
    _attr_device_class = BinarySensorDeviceClass.MOTION

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_motion_detected"
        self._attr_device_info = build_device_info(coordinator, cam_id)
        self._is_on = False
        self._clear_unsub = None
        self._last_lan_update_sequence = coordinator.lan_update_sequence
        self._last_alarm_timestamp: float | None = None

    @property
    def is_on(self) -> bool:
        return self._is_on

    @callback
    def _handle_coordinator_update(self) -> None:
        if self._motion_reported():
            self._is_on = True
            self._schedule_clear()
        self.async_write_ha_state()

    @callback
    def _motion_reported(self) -> bool:
        dps = self.coordinator.data or {}

        sequence = self.coordinator.lan_update_sequence
        fresh_dps = None
        if sequence != self._last_lan_update_sequence:
            self._last_lan_update_sequence = sequence
            fresh_dps = self.coordinator.last_lan_dps

        if (
            fresh_dps
            and fresh_dps.get(DPS_ALERT_EVENT) == "motion_detection"
            and dps.get(DPS_MOTION_SWITCH, True)
        ):
            return True

        timestamp = motion_event_timestamp(dps.get(DPS_ALARM_RECORD))
        if is_new_event(timestamp, self._last_alarm_timestamp, time.time()):
            self._last_alarm_timestamp = timestamp
            _LOGGER.debug(
                "Motion alarm record for %s at %s", self.coordinator.camera_name, timestamp
            )
            return True

        # Remember a stale record so it cannot fire later as if it were new.
        if timestamp is not None and self._last_alarm_timestamp is None:
            self._last_alarm_timestamp = timestamp
        return False

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
        self._attr_device_info = build_device_info(coordinator, cam_id)
        self._is_on = False
        self._clear_unsub = None
        self._last_lan_update_sequence = coordinator.lan_update_sequence
        self._last_alarm_timestamp: float | None = None

    @property
    def is_on(self) -> bool:
        return self._is_on

    @callback
    def _handle_coordinator_update(self) -> None:
        if self._sound_reported():
            self._is_on = True
            self._schedule_clear()
        self.async_write_ha_state()

    @callback
    def _sound_reported(self) -> bool:
        sequence = self.coordinator.lan_update_sequence
        fresh_dps = None
        if sequence != self._last_lan_update_sequence:
            self._last_lan_update_sequence = sequence
            fresh_dps = self.coordinator.last_lan_dps

        if fresh_dps and fresh_dps.get(DPS_DECIBEL_EVENT) == "decibel_upload":
            return True

        dps = self.coordinator.data or {}
        timestamp = sound_event_timestamp(dps.get(DPS_ALARM_RECORD))
        if is_new_event(timestamp, self._last_alarm_timestamp, time.time()):
            self._last_alarm_timestamp = timestamp
            return True

        if timestamp is not None and self._last_alarm_timestamp is None:
            self._last_alarm_timestamp = timestamp
        return False

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
