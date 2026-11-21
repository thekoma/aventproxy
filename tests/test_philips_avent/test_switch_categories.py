"""Which switches sit under Configuration and which stay controls."""

import asyncio
import sys
import types

import pytest

from .ha_stand_in import home_assistant_installed, load


class _Coordinator:
    """Stands in for the coordinator: enough for switch.async_setup_entry."""

    def __init__(self, data):
        self.data = data
        self.device_info = {}
        self.camera_name = "Nursery"

    def async_add_listener(self, listener):
        return lambda: None


def _switch_module(monkeypatch):
    monkeypatch.setitem(sys.modules, "homeassistant.components.switch", types.SimpleNamespace(
        SwitchEntity=type("SwitchEntity", (), {}),
    ))
    return load(monkeypatch, "switch")


@pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")
def test_detection_switches_are_settings(monkeypatch):
    """Detect motion, sound and crying sit under Configuration and keep their
    unique ids; the night light and privacy mode stay controls."""
    switch = _switch_module(monkeypatch)
    coordinator = _Coordinator({"1": True, "11": False, "12": True, "134": True, "139": False})
    hass = types.SimpleNamespace(data={"philips_avent": {"entry": {"coordinators": {"cam": coordinator}}}})
    entry = types.SimpleNamespace(entry_id="entry", async_on_unload=lambda remove: None)
    added = []
    asyncio.run(switch.async_setup_entry(hass, entry, added.extend))
    category = {e._attr_unique_id: getattr(e, "_attr_entity_category", None) for e in added}
    assert category == {
        "cam_138": None, "cam_237": None,  # night light, privacy mode
        "cam_1": None, "cam_11": None,  # SenseIQ, awake alert
        "cam_134": "config", "cam_139": "config",  # detect motion, detect sound
        "cam_12": "config",  # detect crying
    }
