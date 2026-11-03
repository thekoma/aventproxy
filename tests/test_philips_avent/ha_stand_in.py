"""Load integration modules that import Home Assistant, against a minimal stand-in.

The rest of the suite keeps Home Assistant out by testing pure helpers. A few
decisions live in the coordinator itself, where a wrong one changes what a
monitor on protocol 3.3 does, so those tests load the real module here. The
stand-in covers only the names the coordinator and the LAN client import.

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


def _stand_in_modules() -> dict[str, types.ModuleType]:
    def module(name, **attrs):
        mod = types.ModuleType(name)
        mod.__dict__.update(attrs)
        return mod

    modules = {
        "homeassistant": module("homeassistant"),
        "homeassistant.core": module("homeassistant.core", HomeAssistant=object, callback=lambda func: func),
        "homeassistant.exceptions": module("homeassistant.exceptions", ConfigEntryAuthFailed=ConfigEntryAuthFailed),
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
        "homeassistant.config_entries": module("homeassistant.config_entries", ConfigEntry=object),
        "homeassistant.const": module(
            "homeassistant.const",
            SIGNAL_STRENGTH_DECIBELS_MILLIWATT="dBm",
            EntityCategory=_Names(),
            UnitOfTemperature=_Names(),
            UnitOfTime=_Names(),
        ),
        "homeassistant.components": module("homeassistant.components"),
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
    for key, mod in _stand_in_modules().items():
        monkeypatch.setitem(sys.modules, key, mod)
    for path in PKG_DIR.glob("*.py"):
        key = f"{PKG}.{path.stem}"
        monkeypatch.setitem(sys.modules, key, None)
        monkeypatch.delitem(sys.modules, key)
    return importlib.import_module(f"{PKG}.{name}")
