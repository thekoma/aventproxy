"""A camera discovery that fails on the network at setup is retried.

Setup loads against the stand-in for Home Assistant, with the few extra names
the package's __init__ and services import.
"""
from __future__ import annotations

import asyncio
import importlib.util
import logging
import sys
import types

import aiohttp
import pytest

from .ha_stand_in import PKG, PKG_DIR, home_assistant_installed, load


class ConfigEntryNotReady(Exception):
    pass


def _load_setup(monkeypatch):
    load(monkeypatch, "const")
    exceptions = sys.modules["homeassistant.exceptions"]
    for name, cls in (("ConfigEntryNotReady", ConfigEntryNotReady), ("HomeAssistantError", Exception),
                      ("ServiceValidationError", Exception)):
        monkeypatch.setattr(exceptions, name, cls, raising=False)
    core = sys.modules["homeassistant.core"]
    for name in ("ServiceCall", "ServiceResponse", "SupportsResponse"):
        monkeypatch.setattr(core, name, object, raising=False)
    monkeypatch.setattr(sys.modules["homeassistant.const"], "Platform", types.SimpleNamespace(
        CAMERA="camera", SENSOR="sensor", SWITCH="switch", NUMBER="number", BUTTON="button",
        SELECT="select", BINARY_SENSOR="binary_sensor"), raising=False)
    helpers = {
        "homeassistant.helpers.config_validation": types.SimpleNamespace(string=str, date=str),
        "homeassistant.helpers.device_registry": types.SimpleNamespace(),
    }
    for key, mod in helpers.items():
        monkeypatch.setitem(sys.modules, key, mod)
    spec = importlib.util.spec_from_file_location(f"{PKG}.setup", PKG_DIR / "__init__.py")
    module = importlib.util.module_from_spec(spec)
    monkeypatch.setitem(sys.modules, spec.name, module)
    spec.loader.exec_module(module)
    return module


class FakeSession:
    closed = False

    async def close(self):
        self.closed = True


class FakeConfigEntries:
    def async_update_entry(self, entry, data):
        entry.data = data


class SetupHass:
    def __init__(self):
        self.data = {}
        self.config_entries = FakeConfigEntries()


class SetupEntry:
    entry_id = "abc"

    def __init__(self):
        self.data = {"sid": "fake-sid", "device_id": "fake-device"}
        self.options = {}


def _setup_with_discovery(monkeypatch, discover):
    setup = _load_setup(monkeypatch)
    session = FakeSession()
    monkeypatch.setattr(setup.aiohttp, "ClientSession", lambda **_: session)
    monkeypatch.setattr(setup.PhilipsAventAPI, "discover_cameras", discover)
    return setup, session


@pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")
@pytest.mark.parametrize("error", [
    aiohttp.ClientConnectionError("cloud unreachable"),
    TimeoutError(),
    ValueError("Expecting value: line 1 column 1 (char 0)"),  # an HTML error page instead of JSON
])
def test_a_failed_discovery_retries_the_entry(monkeypatch, caplog, error):
    # Returning False left the entry unloaded until a restart or reload.
    async def unreachable(self):
        raise error

    setup, session = _setup_with_discovery(monkeypatch, unreachable)
    with pytest.raises(ConfigEntryNotReady, match=type(error).__name__):
        asyncio.run(setup.async_setup_entry(SetupHass(), SetupEntry()))
    assert session.closed
    # Home Assistant shows the reason on the entry; no warning or traceback of our own on each retry.
    assert not [r for r in caplog.records if r.levelno >= logging.WARNING]


@pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")
def test_an_account_without_cameras_is_still_not_retried(monkeypatch):
    # Discovery that answers with no devices is not a hiccup: retrying would never end.
    async def none_found(self):
        return []

    setup, session = _setup_with_discovery(monkeypatch, none_found)
    assert asyncio.run(setup.async_setup_entry(SetupHass(), SetupEntry())) is False
    assert session.closed


@pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")
def test_a_bug_in_discovery_is_still_logged_and_not_retried(monkeypatch, caplog):
    # Only network and HTTP-level failures are retried; a parsing bug keeps its traceback.
    async def broken(self):
        raise TypeError("'NoneType' object is not iterable")

    setup, session = _setup_with_discovery(monkeypatch, broken)
    assert asyncio.run(setup.async_setup_entry(SetupHass(), SetupEntry())) is False
    assert session.closed
    assert any(r.levelno == logging.ERROR and r.exc_info for r in caplog.records)
