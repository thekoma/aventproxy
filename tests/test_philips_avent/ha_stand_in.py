"""Load integration modules that import Home Assistant, against a minimal stand-in.

The rest of the suite keeps Home Assistant out by testing pure helpers. A few
decisions live in the coordinator itself, where a wrong one changes what a
monitor on protocol 3.3 does, so those tests load the real module here. The
stand-in covers only the names the coordinator and the LAN client import.

Never used where Home Assistant is installed: the stand-in must not shadow the
real package. tinytuya is stubbed only when it is missing, as it is in CI.
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
    """Import `<package>.<name>` with the stand-in in place, undone by `monkeypatch`."""
    for key, mod in _stand_in_modules().items():
        monkeypatch.setitem(sys.modules, key, mod)
    for key in [k for k in sys.modules if k.startswith(f"{PKG}.")]:
        monkeypatch.delitem(sys.modules, key)
    return importlib.import_module(f"{PKG}.{name}")
