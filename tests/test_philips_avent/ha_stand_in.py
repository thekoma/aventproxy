"""Load integration modules that import Home Assistant, against a minimal stand-in.

The rest of the suite keeps Home Assistant out by testing pure helpers. A few
decisions live in the coordinator itself, where a wrong one changes what a
monitor on protocol 3.3 does, so those tests load the real module here. The
stand-in covers only the names the coordinator, the LAN client, the entities
and the options flow import.

Never used where Home Assistant is installed: the stand-in must not shadow the
real package, so those tests are reported as skipped there, not run. tinytuya
is stubbed only when it is missing, as it is in CI.
"""
from __future__ import annotations

import importlib
import importlib.util
import sys
import types
from datetime import UTC, datetime
from pathlib import Path

PKG_DIR = Path(__file__).parent.parent.parent / "custom_components" / "philips_avent"
PKG = "philips_avent_stand_in"


def home_assistant_installed() -> bool:
    return importlib.util.find_spec("homeassistant") is not None


class DataUpdateCoordinator:
    """What the coordinator uses of Home Assistant's base class, scheduling included.

    `async_set_updated_data` behaves as in Home Assistant 2026.10
    (helpers/update_coordinator.py): it moves the next refresh a full interval
    out, marks the last update successful, then notifies the listeners.
    `scheduled_for` stands for the next refresh, on the `clock` tests set.
    """

    def __init__(self, hass, logger, name=None, update_interval=None):
        self.hass = hass
        self.logger = logger
        self.name = name
        self.update_interval = update_interval
        self.data = None
        self.last_update_success = True
        self.clock = lambda: 0.0
        self.scheduled_for = None
        self.listener_updates = 0

    def _schedule_refresh(self):
        self.scheduled_for = self.clock() + self.update_interval.total_seconds()

    def async_set_updated_data(self, data):
        self._schedule_refresh()
        self.data = data
        self.last_update_success = True
        self.async_update_listeners()

    def async_update_listeners(self):
        self.listener_updates += 1


class UpdateFailed(Exception):
    pass


class CoordinatorEntity:
    """Home Assistant's CoordinatorEntity: available while the last refresh succeeded."""

    def __init__(self, coordinator, context=None):
        self.coordinator = coordinator

    @property
    def available(self) -> bool:
        return self.coordinator.last_update_success


class _Names:
    """Stands in for HA's enums and constant groups: any attribute is its own name."""

    def __getattr__(self, name):
        return name.lower()


class ConfigEntryAuthFailed(Exception):
    pass


class ConfigEntryNotReady(Exception):
    pass


class Camera:
    """Home Assistant's Camera: `stream` is None until a viewer opened one."""

    def __init__(self):
        self.hass = None
        self.stream = None

    async def async_added_to_hass(self):
        pass

    def async_on_remove(self, func):
        pass


class SelectEntity:
    """Home Assistant's SelectEntity: `writes` counts async_write_ha_state.

    `should_poll` is Entity's (helpers/entity.py, 2026.11.2): True unless the
    entity sets `_attr_should_poll`, and then HA updates it every 30 s.
    """

    hass = None
    writes = 0
    _attr_should_poll = True

    @property
    def should_poll(self) -> bool:
        return self._attr_should_poll

    def async_write_ha_state(self):
        self.writes += 1


class RestoreEntity:
    """Home Assistant's RestoreEntity: `last_state` is what the state store held."""

    last_state = None

    async def async_added_to_hass(self):
        pass

    async def async_get_last_state(self):
        return self.last_state


class ConfigFlow:
    """Home Assistant's ConfigFlow, as far as a subclass declaration needs it."""

    def __init_subclass__(cls, domain=None, **kwargs):
        super().__init_subclass__(**kwargs)


class OptionsFlow:
    """Home Assistant's OptionsFlow: forms and entries come back as dicts.

    `add_suggested_values_to_schema` is Home Assistant 2026.11.2's
    (data_entry_flow.py) without sections: a copy of the schema whose markers
    found in `suggested_values` carry them as their `suggested_value`.
    """

    config_entry = None

    def async_show_form(self, **kwargs):
        return {"type": "form", **kwargs}

    def async_create_entry(self, *, title, data):
        return {"type": "create_entry", "title": title, "data": data}

    def add_suggested_values_to_schema(self, data_schema, suggested_values):
        import copy

        import voluptuous as vol

        schema = {}
        for key, val in data_schema.schema.items():
            new_key = key
            if suggested_values and key in suggested_values and isinstance(key, vol.Marker):
                new_key = copy.copy(key)
                new_key.description = {"suggested_value": suggested_values[key.schema]}
            schema[new_key] = val
        return vol.Schema(schema)


class Dispatcher:
    """Home Assistant's dispatcher, synchronous: `sent` lists every signal sent."""

    def __init__(self):
        self.targets: dict[str, list] = {}
        self.sent: list[str] = []

    def connect(self, hass, signal, target):
        self.targets.setdefault(signal, []).append(target)
        return lambda: self.targets[signal].remove(target)

    def send(self, hass, signal, *args):
        self.sent.append(signal)
        for target in list(self.targets.get(signal, [])):
            target(*args)


dispatcher = Dispatcher()


async def _no_ffmpeg(*args, **kwargs):
    raise AssertionError("tests replace ffmpeg's async_get_image")


def _stand_in_modules() -> dict[str, types.ModuleType]:
    def module(name, **attrs):
        mod = types.ModuleType(name)
        mod.__dict__.update(attrs)
        return mod

    modules = {
        "homeassistant": module("homeassistant"),
        "homeassistant.core": module("homeassistant.core", HomeAssistant=object, callback=lambda func: func),
        "homeassistant.exceptions": module(
            "homeassistant.exceptions",
            ConfigEntryAuthFailed=ConfigEntryAuthFailed,
            ConfigEntryNotReady=ConfigEntryNotReady,
        ),
        "homeassistant.helpers": module("homeassistant.helpers"),
        "homeassistant.helpers.event": module(
            "homeassistant.helpers.event", async_call_later=lambda hass, delay, action: lambda: None
        ),
        "homeassistant.helpers.update_coordinator": module(
            "homeassistant.helpers.update_coordinator",
            DataUpdateCoordinator=DataUpdateCoordinator,
            UpdateFailed=UpdateFailed,
            CoordinatorEntity=CoordinatorEntity,
        ),
        "homeassistant.helpers.entity_platform": module("homeassistant.helpers.entity_platform", AddEntitiesCallback=object),
        "homeassistant.helpers.dispatcher": module(
            "homeassistant.helpers.dispatcher",
            async_dispatcher_connect=dispatcher.connect,
            async_dispatcher_send=dispatcher.send,
        ),
        "homeassistant.helpers.restore_state": module("homeassistant.helpers.restore_state", RestoreEntity=RestoreEntity),
        "homeassistant.components.select": module("homeassistant.components.select", SelectEntity=SelectEntity),
        "homeassistant.config_entries": module(
            "homeassistant.config_entries",
            ConfigEntry=object,
            ConfigFlow=ConfigFlow,
            OptionsFlow=OptionsFlow,
            callback=lambda func: func,
        ),
        "homeassistant.data_entry_flow": module("homeassistant.data_entry_flow", AbortFlow=type("AbortFlow", (Exception,), {})),
        "homeassistant.helpers.aiohttp_client": module(
            "homeassistant.helpers.aiohttp_client", async_get_clientsession=lambda hass: None
        ),
        "homeassistant.helpers.selector": module(
            "homeassistant.helpers.selector", CountrySelector=lambda config: str, CountrySelectorConfig=dict
        ),
        "homeassistant.const": module(
            "homeassistant.const",
            CONF_COUNTRY="country",
            CONF_EMAIL="email",
            CONF_PASSWORD="password",
            SIGNAL_STRENGTH_DECIBELS_MILLIWATT="dBm",
            EntityCategory=_Names(),
            UnitOfTemperature=_Names(),
            UnitOfTime=_Names(),
        ),
        "homeassistant.components": module("homeassistant.components"),
        "homeassistant.components.camera": module(
            "homeassistant.components.camera",
            Camera=Camera,
            CameraEntityFeature=_Names(),
            CameraEntityCapabilities=dict,
        ),
        "homeassistant.components.ffmpeg": module(
            "homeassistant.components.ffmpeg", async_get_image=_no_ffmpeg
        ),
        "homeassistant.components.binary_sensor": module(
            "homeassistant.components.binary_sensor",
            BinarySensorDeviceClass=_Names(),
            BinarySensorEntity=type("BinarySensorEntity", (), {}),
        ),
        "homeassistant.components.sensor": module(
            "homeassistant.components.sensor",
            SensorDeviceClass=_Names(),
            SensorEntity=type("SensorEntity", (), {}),
            SensorStateClass=_Names(),
        ),
        "homeassistant.util": module("homeassistant.util"),
        "homeassistant.util.dt": module(
            "homeassistant.util.dt", now=lambda: datetime.now(UTC), utcnow=lambda: datetime.now(UTC)
        ),
    }
    if importlib.util.find_spec("tinytuya") is None:
        modules["tinytuya"] = module("tinytuya", Device=object, deviceScan=lambda **_: {})
    package = module(PKG)
    package.__path__ = [str(PKG_DIR)]
    modules[PKG] = package
    return modules


def load(monkeypatch, name: str) -> types.ModuleType:
    """Import `<package>.<name>` with the stand-in in place, undone by `monkeypatch`.

    Every module of the package is registered with `monkeypatch` before the
    import, so the ones it pulls in are removed again afterwards too.
    """
    dispatcher.__init__()
    for key, mod in _stand_in_modules().items():
        monkeypatch.setitem(sys.modules, key, mod)
    for path in PKG_DIR.glob("*.py"):
        key = f"{PKG}.{path.stem}"
        monkeypatch.setitem(sys.modules, key, None)
        monkeypatch.delitem(sys.modules, key)
    return importlib.import_module(f"{PKG}.{name}")
