"""Select entities for Philips Avent Baby Monitor."""

import json
import logging

from homeassistant.components.select import SelectEntity
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import (
    DOMAIN,
    DPS_LULLABY_CONTROL,
    DPS_LULLABY_MODE,
    DPS_LULLABY_TIMER,
    DPS_LULLABY_TIMER_SWITCH,
    DPS_LIGHT_TIMER,
    DPS_LIGHT_TIMER_SWITCH,
    LULLABY_ID_BY_NAME,
    LULLABY_TRACK_MAP,
    LULLABY_TRACKS,
    TIMER_OPTIONS,
    TIMER_SECONDS_TO_LABEL,
)
from .coordinator import PhilipsAventCoordinator

_LOGGER = logging.getLogger(__name__)

PLAY_MODES = ["loop", "loop1", "shuffle"]
PLAY_MODE_LABELS = {"loop": "Loop All", "loop1": "Repeat One", "shuffle": "Shuffle"}


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        entities.append(AventLullabySelect(coordinator, cam_id))
        entities.append(AventPlayModeSelect(coordinator, cam_id))
        entities.append(AventTimerSelect(
            coordinator, cam_id,
            "Lullaby Timer", "mdi:timer-music-outline",
            DPS_LULLABY_TIMER, DPS_LULLABY_TIMER_SWITCH,
        ))
        entities.append(AventTimerSelect(
            coordinator, cam_id,
            "Night Light Timer", "mdi:timer-outline",
            DPS_LIGHT_TIMER, DPS_LIGHT_TIMER_SWITCH,
        ))
    async_add_entities(entities)


class AventLullabySelect(CoordinatorEntity, SelectEntity):
    _attr_has_entity_name = True
    _attr_name = "Lullaby Track"
    _attr_icon = "mdi:music-note"
    _attr_options = LULLABY_TRACKS

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_lullaby_track"
        self._attr_device_info = {
            "identifiers": {(DOMAIN, cam_id)},
            "name": coordinator.camera_name,
            "manufacturer": "Philips",
            "model": "Avent SCD973",
        }

    @property
    def current_option(self) -> str | None:
        dps = self.coordinator.data
        if not dps or "248" not in dps:
            return None
        try:
            info = json.loads(dps["248"])
            track_id = info.get("id")
            if track_id in LULLABY_TRACK_MAP:
                return LULLABY_TRACK_MAP[track_id][0]
        except (json.JSONDecodeError, TypeError):
            pass
        return None

    async def async_select_option(self, option: str) -> None:
        track_id = LULLABY_ID_BY_NAME.get(option)
        if track_id is None:
            return
        await self.coordinator.set_dps({
            "202": json.dumps({"bizcode": "phi-no-bm", "id": track_id}),
            DPS_LULLABY_CONTROL: "play",
        })
        self.coordinator.data["248"] = json.dumps(
            {"bizcode": "phi-no-bm", "id": track_id, "errcode": 0}
        )
        self.async_write_ha_state()


class AventPlayModeSelect(CoordinatorEntity, SelectEntity):
    _attr_has_entity_name = True
    _attr_name = "Play Mode"
    _attr_icon = "mdi:repeat"
    _attr_options = list(PLAY_MODE_LABELS.values())

    def __init__(self, coordinator: PhilipsAventCoordinator, cam_id: str):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_play_mode"
        self._attr_device_info = {
            "identifiers": {(DOMAIN, cam_id)},
            "name": coordinator.camera_name,
            "manufacturer": "Philips",
            "model": "Avent SCD973",
        }

    @property
    def current_option(self) -> str | None:
        dps = self.coordinator.data
        if dps and DPS_LULLABY_MODE in dps:
            return PLAY_MODE_LABELS.get(dps[DPS_LULLABY_MODE])
        return None

    async def async_select_option(self, option: str) -> None:
        mode = next((k for k, v in PLAY_MODE_LABELS.items() if v == option), None)
        if mode is None:
            return
        await self.coordinator.set_dps({DPS_LULLABY_MODE: mode})
        self.coordinator.data[DPS_LULLABY_MODE] = mode
        self.async_write_ha_state()


class AventTimerSelect(CoordinatorEntity, SelectEntity):
    _attr_has_entity_name = True
    _attr_options = list(TIMER_OPTIONS.keys())

    def __init__(
        self, coordinator: PhilipsAventCoordinator, cam_id: str,
        name: str, icon: str, dps_timer: str, dps_switch: str,
    ):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._dps_timer = dps_timer
        self._dps_switch = dps_switch
        self._attr_name = name
        self._attr_icon = icon
        self._attr_unique_id = f"{cam_id}_{dps_timer}_timer"
        self._attr_device_info = {
            "identifiers": {(DOMAIN, cam_id)},
            "name": coordinator.camera_name,
            "manufacturer": "Philips",
            "model": "Avent SCD973",
        }

    @property
    def current_option(self) -> str | None:
        dps = self.coordinator.data
        if not dps:
            return None
        switch_on = dps.get(self._dps_switch)
        if not switch_on:
            return "Off"
        seconds = dps.get(self._dps_timer, 0)
        return TIMER_SECONDS_TO_LABEL.get(seconds, f"{seconds // 60} min")

    async def async_select_option(self, option: str) -> None:
        seconds = TIMER_OPTIONS.get(option, 0)
        if seconds == 0:
            await self.coordinator.set_dps({self._dps_switch: False})
            self.coordinator.data[self._dps_switch] = False
        else:
            await self.coordinator.set_dps({self._dps_switch: True, self._dps_timer: seconds})
            self.coordinator.data[self._dps_switch] = True
            self.coordinator.data[self._dps_timer] = seconds
        self.async_write_ha_state()
