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

from . import senseiq
from .const import (
    DOMAIN,
    DPS_ALARM_RECORD,
    DPS_ALERT_EVENT,
    DPS_CRY_DET_SWITCH,
    DPS_DECIBEL_EVENT,
    DPS_LULLABY_STATE,
    DPS_MOTION_SWITCH,
    DPS_SENSEIQ_STATUS,
)
from .coordinator import PhilipsAventCoordinator
from .entity import build_device_info
from .events import (
    cry_event_timestamp,
    is_new_event,
    motion_event_timestamp,
    sound_event_timestamp,
)

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
        # SenseIQ presence, from the live status (DPS 3).
        if DPS_SENSEIQ_STATUS in (coordinator.data or {}):
            entities.append(AventBabyDetected(coordinator, cam_id))
        # Dedicated crying alert, on monitors advertising cry detection (DPS 12).
        if DPS_CRY_DET_SWITCH in (coordinator.data or {}):
            entities.append(AventCryDetected(coordinator, cam_id))
    async_add_entities(entities)


class AventLullabyPlaying(CoordinatorEntity, BinarySensorEntity):
    _attr_has_entity_name = True
    _attr_translation_key = "lullaby_playing"
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
    _attr_translation_key = "motion_detected"
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


class AventBabyDetected(CoordinatorEntity, BinarySensorEntity):
    """Whether SenseIQ currently sees the baby in the crib.

    Read from the live status (DPS 3): a reported breathing rate, or an ``r``
    flag that is not the "no baby" value. DPS 15 (``no_senseiq_signal``) looked
    like the right source but does not track live presence — it stayed set all
    night on hardware while breathing and sleep were reported — so it is not
    used here. The companion to the breathing and sleep sensors.
    """

    _attr_has_entity_name = True
    _attr_translation_key = "baby_detected"
    _attr_icon = "mdi:baby-face-outline"
    _attr_device_class = BinarySensorDeviceClass.OCCUPANCY

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_baby_detected"
        self._attr_device_info = build_device_info(coordinator, cam_id)

    @property
    def is_on(self) -> bool | None:
        return senseiq.is_baby_present((self.coordinator.data or {}).get(DPS_SENSEIQ_STATUS))


class AventSoundDetected(CoordinatorEntity, BinarySensorEntity):
    """Sound alerts, from whichever DPS the monitor reports them on.

    Same split as motion: DPS 141 set to `decibel_upload` on the SCD973 and
    SCD923 family, and the timestamped DPS 212 alarm record on the SCD951 and
    SCD953 family, which names a noise alert `ipc_bang` (#42). Auto-clears after
    ALERT_CLEAR_SECONDS.
    """

    _attr_has_entity_name = True
    _attr_translation_key = "sound_detected"
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


class AventCryDetected(CoordinatorEntity, BinarySensorEntity):
    """Crying alerts (``ipc_baby_cry`` / ``ipc_cry``): the SenseIQ family's cry
    detection, as a dedicated companion to "Sound Detected".

    Fires only on a cry, from the timestamped DPS 212 alarm record, and
    auto-clears after ALERT_CLEAR_SECONDS. "Sound Detected" still fires too, so
    remove the cry commands from SOUND_COMMANDS in events.py if you want strict
    separation instead.
    """

    _attr_has_entity_name = True
    _attr_translation_key = "crying_detected"
    _attr_icon = "mdi:emoticon-cry-outline"
    _attr_device_class = BinarySensorDeviceClass.SOUND

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_crying_detected"
        self._attr_device_info = build_device_info(coordinator, cam_id)
        self._is_on = False
        self._clear_unsub = None
        self._last_alarm_timestamp: float | None = None

    @property
    def is_on(self) -> bool:
        return self._is_on

    @callback
    def _handle_coordinator_update(self) -> None:
        if self._cry_reported():
            self._is_on = True
            self._schedule_clear()
        self.async_write_ha_state()

    @callback
    def _cry_reported(self) -> bool:
        dps = self.coordinator.data or {}
        timestamp = cry_event_timestamp(dps.get(DPS_ALARM_RECORD))
        if is_new_event(timestamp, self._last_alarm_timestamp, time.time()):
            self._last_alarm_timestamp = timestamp
            _LOGGER.debug(
                "Cry alarm record for %s at %s", self.coordinator.camera_name, timestamp
            )
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
