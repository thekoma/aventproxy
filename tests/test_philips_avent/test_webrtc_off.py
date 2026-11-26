"""The "Use WebRTC" option, from the options flow to the bridge JSON and the entities.

The Go side reads it as ``BridgeConfig.WebRTC`` (``webrtc``) in
``cmd/addon/addon.go``; absent means on.
"""

import asyncio
import importlib.util
import json
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest
from const import CONF_WEBRTC, DEFAULT_WEBRTC, options_error
from payload import build_bridge_config, write_bridge_config_file

from .ha_stand_in import PKG, PKG_DIR, _Names, home_assistant_installed, load

COMPONENT = Path(__file__).parent.parent.parent / "custom_components" / "philips_avent"

BASE = {
    "signing_key": "sk", "sid": "eu166", "ecode": "E", "partner": "P",
    "app_key": "AK", "device_id": "D", "package_name": "pkg",
    "api_host": "a1.tuyaeu.com", "bridge_port": 38554,
    "cameras": [{"deviceId": "abc", "deviceName": "Cam"}],
}
stand_in = pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")


def test_default_is_on_and_writes_nothing():
    assert DEFAULT_WEBRTC is True
    assert CONF_WEBRTC == "webrtc"
    config = build_bridge_config(**BASE, talkback=True)
    assert "webrtc" not in config
    assert config["talkback"] is True


def test_off_is_written_and_turns_talkback_off(tmp_path):
    path = tmp_path / "philips_avent_bridge_x.json"
    write_bridge_config_file(path, build_bridge_config(**BASE, talkback=True, webrtc=False))
    written = json.loads(path.read_text())
    assert written["webrtc"] is False
    assert written["talkback"] is False


@pytest.mark.parametrize(
    "options, error",
    [
        ({}, None),
        ({"webrtc": False}, None),  # LAN streaming is on by default
        ({"webrtc": False, "lan_streaming": True}, None),
        ({"webrtc": True, "lan_streaming": False}, None),
        ({"webrtc": False, "lan_streaming": False}, "webrtc_needs_lan"),
    ],
)
def test_options_need_one_way_to_stream(options, error):
    assert options_error(options) == error


def _options_flow(monkeypatch, options):
    module = load(monkeypatch, "config_flow")
    flow = module.PhilipsAventOptionsFlowHandler()
    flow.config_entry = SimpleNamespace(options=options)
    return flow


def _form_values(result):
    """The form's fields: {name: (default, suggested value)}."""
    fields = {}
    for key in result["data_schema"].schema:
        default = key.default() if callable(key.default) else None
        fields[str(key)] = (default, (key.description or {}).get("suggested_value"))
    return fields


@stand_in
@pytest.mark.parametrize("options, default", [({}, True), ({"webrtc": False}, False)])
def test_options_flow_offers_it(monkeypatch, options, default):
    result = asyncio.run(_options_flow(monkeypatch, options).async_step_init())
    assert result["type"] == "form"
    assert result["errors"] == {}
    assert _form_values(result)["webrtc"] == (default, None)


@stand_in
def test_options_flow_refuses_webrtc_and_lan_streaming_off(monkeypatch):
    submitted = {"bridge_host": "bridge", "bridge_port": 29000, "talkback": True, "lan_streaming": False, "webrtc": False}
    result = asyncio.run(_options_flow(monkeypatch, {}).async_step_init(dict(submitted)))
    assert result["type"] == "form"
    assert result["errors"] == {"base": "webrtc_needs_lan"}
    # The form comes back with the user's input, not the saved options.
    fields = _form_values(result)
    assert {name: suggested for name, (_, suggested) in fields.items()} == submitted
    assert fields["webrtc"][0] is True  # the saved option is still the default


@stand_in
def test_options_flow_saves_webrtc_off_with_lan_streaming(monkeypatch):
    submitted = {"bridge_host": "bridge", "bridge_port": 29000, "talkback": True, "lan_streaming": True, "webrtc": False}
    result = asyncio.run(_options_flow(monkeypatch, {}).async_step_init(dict(submitted)))
    assert result == {"type": "create_entry", "title": "", "data": submitted}


def _integration(monkeypatch):
    """__init__.py loaded against the stand-in, as test_setup_entry.py loads it."""
    load(monkeypatch, "coordinator")
    monkeypatch.setattr(sys.modules["homeassistant.const"], "Platform", _Names(), raising=False)
    services = type(sys)(f"{PKG}.services")

    async def _no_services(hass):
        return None

    services.async_setup_services = _no_services
    monkeypatch.setitem(sys.modules, f"{PKG}.services", services)
    spec = importlib.util.spec_from_file_location(PKG, PKG_DIR / "__init__.py", submodule_search_locations=[str(PKG_DIR)])
    module = importlib.util.module_from_spec(spec)
    monkeypatch.setitem(sys.modules, PKG, module)
    spec.loader.exec_module(module)
    return module


@stand_in
@pytest.mark.parametrize(
    "options, webrtc, talkback",
    [
        ({"talkback": True}, None, True),  # the default writes nothing
        ({"talkback": True, "webrtc": True}, None, True),
        ({"talkback": True, "webrtc": False}, False, False),
    ],
)
def test_integration_writes_it(monkeypatch, tmp_path, options, webrtc, talkback):
    module = _integration(monkeypatch)
    entry = SimpleNamespace(entry_id="e1", data={"sid": "sid"}, options=options)

    async def executor(func, *args):
        return func(*args)

    hass = SimpleNamespace(
        config=SimpleNamespace(path=lambda *parts: str(tmp_path.joinpath(*parts))),
        config_entries=SimpleNamespace(async_entries=lambda domain: [entry]),
        async_add_executor_job=executor,
    )
    asyncio.run(module._write_bridge_config(hass, entry, SimpleNamespace(device_id="dev"), [], {}))
    written = json.loads((tmp_path / "philips_avent_bridge_e1.json").read_text())
    assert written.get("webrtc") == webrtc
    assert written["talkback"] is talkback


@pytest.mark.parametrize("path", ["strings.json", "translations/en.json", "translations/fr.json"])
def test_translated(path):
    options = json.loads((COMPONENT / path).read_text(encoding="utf-8"))["options"]
    step = options["step"]["init"]
    assert step["data"]["webrtc"]
    assert step["data_description"]["webrtc"]
    assert options["error"]["webrtc_needs_lan"]


COORDINATOR = SimpleNamespace(camera_name="Nursery", display_name="Nursery", rtsp_path=None, device_info={})


def _setup(monkeypatch, platform, options):
    module = load(monkeypatch, platform)
    entry = SimpleNamespace(entry_id="e", options=options)
    hass = SimpleNamespace(data={"philips_avent": {"e": {"coordinators": {"cam1": COORDINATOR}, "stream_sources": {}}}})
    added = []
    asyncio.run(module.async_setup_entry(hass, entry, added.extend))
    return added


@stand_in
@pytest.mark.parametrize("webrtc, two_way", [(True, True), (False, False)])
def test_camera_declares_two_way_audio_only_with_webrtc(monkeypatch, webrtc, two_way):
    camera = _setup(monkeypatch, "camera", {"talkback": True, "webrtc": webrtc})[0]
    declared = getattr(camera, "_attr_camera_entity_capabilities", None)
    assert bool(declared and declared.get("two_way_audio")) is two_way


@stand_in
@pytest.mark.parametrize("webrtc, offered", [(True, True), (False, False)])
def test_stream_source_only_with_webrtc(monkeypatch, webrtc, offered):
    kinds = [type(e).__name__ for e in _setup(monkeypatch, "select", {"webrtc": webrtc})]
    assert ("AventStreamSourceSelect" in kinds) is offered
