"""Sensor entities for Philips Avent Baby Monitor."""
from __future__ import annotations

from datetime import datetime, timezone

from homeassistant.components.sensor import (
    SensorDeviceClass,
    SensorEntity,
    SensorStateClass,
)
from homeassistant.config_entries import ConfigEntry
from homeassistant.const import (
    EntityCategory,
    SIGNAL_STRENGTH_DECIBELS_MILLIWATT,
    UnitOfTemperature,
    UnitOfTime,
)
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from . import senseiq
from .const import (
    DOMAIN,
    DPS_DEVICE_ERRORS,
    DPS_SENSEIQ_STATUS,
    DPS_SLEEP_SESSION,
    DPS_TEMPERATURE,
)
from .coordinator import PhilipsAventCoordinator
from .entity import build_device_info


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    entities = []

    for cam_id, coordinator in data["coordinators"].items():
        entities.append(AventTemperatureSensor(coordinator, cam_id))
        entities.append(AventWifiSignalSensor(coordinator, cam_id))

        dps = coordinator.data or {}

        # Create the SenseIQ entities only on monitors that actually expose the
        # corresponding DPS. DPS 4 may contain a persisted completed session;
        # entity creation must therefore depend on "decodable", not "active".
        if senseiq.decode_status(dps.get(DPS_SENSEIQ_STATUS)) is not None:
            entities.append(AventBreathingSensor(coordinator, cam_id))
            entities.append(AventSenseIQStateSensor(coordinator, cam_id))

        if senseiq.decode_sleep_session(dps.get(DPS_SLEEP_SESSION)) is not None:
            entities.append(AventSleepStartSensor(coordinator, cam_id))
            entities.append(AventSleepDurationSensor(coordinator, cam_id))
            entities.append(AventSleepStageSensor(coordinator, cam_id))

        if DPS_DEVICE_ERRORS in dps:
            entities.append(AventDeviceErrorsSensor(coordinator, cam_id))

        # Nightly sleep summary from the SenseIQ cloud aggregate. Accurate,
        # full-night figures (unlike DPS 4 which resets mid-night). Created for
        # SenseIQ monitors; the values fill in on the first cloud fetch.
        if DPS_SENSEIQ_STATUS in dps:
            entities.append(AventNightSleepSensor(coordinator, cam_id))
            entities.append(AventNightDurationSensor(
                coordinator, cam_id, "night_in_bed", "in_bed_seconds", "mdi:bed"))
            entities.append(AventNightDurationSensor(
                coordinator, cam_id, "night_deep_sleep", "deep_seconds", "mdi:sleep"))
            entities.append(AventNightDurationSensor(
                coordinator, cam_id, "night_light_sleep", "light_seconds", "mdi:power-sleep"))
            entities.append(AventNightDurationSensor(
                coordinator, cam_id, "night_awake", "awake_seconds", "mdi:sleep-off"))

    async_add_entities(entities)


class AventTemperatureSensor(CoordinatorEntity, SensorEntity):
    _attr_device_class = SensorDeviceClass.TEMPERATURE
    _attr_state_class = SensorStateClass.MEASUREMENT
    _attr_native_unit_of_measurement = UnitOfTemperature.CELSIUS
    _attr_has_entity_name = True
    _attr_translation_key = "temperature"

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_temperature"
        self._attr_device_info = build_device_info(coordinator, cam_id)

    @property
    def native_value(self) -> float | None:
        dps = self.coordinator.data
        if dps and DPS_TEMPERATURE in dps:
            return dps[DPS_TEMPERATURE] / 100.0
        return None


class AventWifiSignalSensor(CoordinatorEntity, SensorEntity):
    _attr_device_class = SensorDeviceClass.SIGNAL_STRENGTH
    _attr_state_class = SensorStateClass.MEASUREMENT
    _attr_native_unit_of_measurement = SIGNAL_STRENGTH_DECIBELS_MILLIWATT
    _attr_has_entity_name = True
    _attr_translation_key = "wifi_signal"
    _attr_icon = "mdi:wifi"
    _attr_entity_registry_enabled_default = True

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_wifi_signal"
        self._attr_device_info = build_device_info(coordinator, cam_id)

    @property
    def native_value(self) -> int | None:
        if hasattr(self.coordinator, "rssi"):
            return self.coordinator.rssi
        return None


class _SenseIQEntity(CoordinatorEntity, SensorEntity):
    _attr_has_entity_name = True

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str, key: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_{key}"
        self._attr_device_info = build_device_info(coordinator, cam_id)

    def _status(self) -> dict | None:
        return senseiq.decode_status(
            (self.coordinator.data or {}).get(DPS_SENSEIQ_STATUS)
        )

    def _session(self) -> dict | None:
        return senseiq.decode_sleep_session(
            (self.coordinator.data or {}).get(DPS_SLEEP_SESSION)
        )

    def _session_active(self) -> bool:
        return senseiq.is_session_active(self._session())


class AventBreathingSensor(_SenseIQEntity):
    """Live breathing rate from DPS 3."""

    _attr_translation_key = "breathing_rate"
    _attr_native_unit_of_measurement = "breaths/min"
    _attr_state_class = SensorStateClass.MEASUREMENT
    _attr_icon = "mdi:lungs"

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator, cam_id, "breathing_rate")

    @property
    def native_value(self) -> int | None:
        status = self._status()
        return status["breaths_per_minute"] if status else None

    @property
    def extra_state_attributes(self) -> dict | None:
        status = self._status()
        if not status:
            return None
        return {"state": status.get("state")}


class AventSenseIQStateSensor(_SenseIQEntity):
    """Raw SenseIQ presence/state flag from DPS 3 until its vocabulary is known."""

    _attr_translation_key = "senseiq_state"
    _attr_icon = "mdi:baby-face-outline"

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator, cam_id, "senseiq_state")

    @property
    def native_value(self) -> str | None:
        status = self._status()
        if not status:
            return None
        value = status.get("state")
        return str(value) if value is not None else None

    @property
    def extra_state_attributes(self) -> dict:
        session = self._session()
        end = senseiq.session_end_timestamp(session)

        attrs = {
            "session_active": senseiq.is_session_active(session),
        }

        if session:
            attrs["last_session_start"] = session.get("start")
            attrs["last_session_duration_seconds"] = session.get("duration_seconds")
            attrs["last_session_end"] = end
            attrs["last_recorded_stage"] = session.get("current_stage")

        return attrs


class AventSleepStartSensor(_SenseIQEntity):
    """Start of the CURRENT SenseIQ session."""

    _attr_translation_key = "sleeping_since"
    _attr_device_class = SensorDeviceClass.TIMESTAMP
    _attr_icon = "mdi:bed-clock"

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator, cam_id, "sleeping_since")

    @property
    def native_value(self) -> datetime | None:
        session = self._session()
        if not session or not senseiq.is_session_active(session):
            return None

        start = session.get("start")
        if start is None:
            return None

        return datetime.fromtimestamp(start, tz=timezone.utc)

    @property
    def extra_state_attributes(self) -> dict:
        session = self._session()
        return {"session_active": senseiq.is_session_active(session)}


class AventSleepDurationSensor(_SenseIQEntity):
    """Duration of the CURRENT SenseIQ session."""

    _attr_translation_key = "sleep_duration"
    _attr_device_class = SensorDeviceClass.DURATION
    _attr_native_unit_of_measurement = UnitOfTime.SECONDS
    _attr_state_class = SensorStateClass.MEASUREMENT
    _attr_icon = "mdi:sleep"
    _attr_suggested_display_precision = 0

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator, cam_id, "sleep_duration")

    @property
    def native_value(self) -> int | None:
        session = self._session()
        if not session or not senseiq.is_session_active(session):
            return None
        return session.get("duration_seconds")

    @property
    def extra_state_attributes(self) -> dict | None:
        session = self._session()
        if not session:
            return None

        active = senseiq.is_session_active(session)
        totals = session.get("totals_seconds") or {}

        return {
            "session_active": active,
            "last_session_end": senseiq.session_end_timestamp(session),
            "awake_seconds": totals.get("awake"),
            "light_sleep_seconds": totals.get("light"),
            "deep_sleep_seconds": totals.get("deep"),
            "stages": session.get("stages"),
        }


class AventSleepStageSensor(_SenseIQEntity):
    """Current stage only while DPS 4 is a live session."""

    _attr_translation_key = "sleep_stage"
    _attr_device_class = SensorDeviceClass.ENUM
    _attr_options = list(senseiq.SLEEP_STAGES)
    _attr_icon = "mdi:power-sleep"

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator, cam_id, "sleep_stage")

    @property
    def native_value(self) -> str | None:
        session = self._session()
        if not session or not senseiq.is_session_active(session):
            return None
        return session.get("current_stage")

    @property
    def extra_state_attributes(self) -> dict | None:
        session = self._session()
        if not session:
            return None

        active = senseiq.is_session_active(session)
        return {
            "session_active": active,
            "current_stage_seconds": (
                session.get("current_stage_seconds") if active else None
            ),
            "last_session_end": senseiq.session_end_timestamp(session),
        }


class AventDeviceErrorsSensor(CoordinatorEntity, SensorEntity):
    """Device error code from DPS 18 (``device_errors``): 0 means no error."""

    _attr_has_entity_name = True
    _attr_translation_key = "device_errors"
    _attr_icon = "mdi:alert-circle-outline"
    _attr_entity_category = EntityCategory.DIAGNOSTIC
    _attr_entity_registry_enabled_default = False

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_device_errors"
        self._attr_device_info = build_device_info(coordinator, cam_id)

    @property
    def native_value(self) -> int | None:
        dps = self.coordinator.data
        if not dps or DPS_DEVICE_ERRORS not in dps:
            return None
        try:
            return int(dps[DPS_DEVICE_ERRORS])
        except (TypeError, ValueError):
            return None


def _fmt_duration(seconds: int | None) -> str | None:
    """Human-readable "H h MM min" (or "M min" under an hour)."""
    if seconds is None:
        return None
    minutes = int(seconds) // 60
    hours, mins = divmod(minutes, 60)
    return f"{hours} h {mins:02d} min" if hours else f"{mins} min"


class AventNightDurationSensor(CoordinatorEntity, SensorEntity):
    """A single duration field of the SenseIQ nightly sleep summary.

    Reported in minutes (graphable/recordable). A readable "H h MM min" string
    and the raw seconds are on the attributes.
    """

    # No DURATION device_class on purpose: it makes Home Assistant convert the
    # value to a per-entity display unit (which stuck to seconds), fighting the
    # readable minutes. A plain minutes measurement shows "633 min" directly, and
    # the readable "H h MM min" string is on the `formatted` attribute.
    _attr_has_entity_name = True
    _attr_native_unit_of_measurement = UnitOfTime.MINUTES
    _attr_state_class = SensorStateClass.MEASUREMENT
    _attr_suggested_display_precision = 0

    def __init__(
        self, coordinator: PhilipsAventCoordinator, cam_id: str,
        key: str, field: str, icon: str,
    ):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._field = field
        self._attr_translation_key = key
        self._attr_icon = icon
        self._attr_unique_id = f"{cam_id}_{key}"
        self._attr_device_info = build_device_info(coordinator, cam_id)

    def _seconds(self) -> int | None:
        day = getattr(self.coordinator, "sleep_day", None)
        return day.get(self._field) if day else None

    @property
    def native_value(self) -> int | None:
        secs = self._seconds()
        return round(secs / 60) if secs is not None else None

    @property
    def extra_state_attributes(self) -> dict | None:
        secs = self._seconds()
        if secs is None:
            return None
        return {"formatted": _fmt_duration(secs), "seconds": secs}


class AventNightSleepSensor(AventNightDurationSensor):
    """Headline nightly sleep (light + deep), with the full breakdown as attrs."""

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(
            coordinator, cam_id, "night_sleep", "asleep_seconds", "mdi:weather-night"
        )

    @property
    def extra_state_attributes(self) -> dict | None:
        day = getattr(self.coordinator, "sleep_day", None)
        if not day:
            return None

        def _dt(ep):
            return datetime.fromtimestamp(ep, tz=timezone.utc) if ep else None

        return {
            "date": day.get("date"),
            # Readable strings (used by the morning-summary notification).
            "formatted": _fmt_duration(day.get("asleep_seconds")),
            "in_bed": _fmt_duration(day.get("in_bed_seconds")),
            "deep_sleep": _fmt_duration(day.get("deep_seconds")),
            "light_sleep": _fmt_duration(day.get("light_seconds")),
            "awake": _fmt_duration(day.get("awake_seconds")),
            "no_signal": _fmt_duration(day.get("no_signal_seconds")),
            # Raw seconds (for automations / templates).
            "in_bed_seconds": day.get("in_bed_seconds"),
            "deep_sleep_seconds": day.get("deep_seconds"),
            "light_sleep_seconds": day.get("light_seconds"),
            "awake_seconds": day.get("awake_seconds"),
            "no_signal_seconds": day.get("no_signal_seconds"),
            "in_bed_from": _dt(day.get("start")),
            "in_bed_to": _dt(day.get("end")),
            "session_count": day.get("session_count"),
        }
