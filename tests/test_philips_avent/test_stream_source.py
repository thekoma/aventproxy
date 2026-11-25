"""The "Stream source" select moves the camera entity between the bridge's paths.

"auto" is /<camera> (the LAN first, the cloud as fallback), "local" is
/<camera>/local (the LAN only). Loads the real select and camera entities
against the Home Assistant stand-in.
"""
from __future__ import annotations

import asyncio
import json
from types import SimpleNamespace

import pytest

from .ha_stand_in import PKG_DIR, dispatcher, home_assistant_installed, load

pytestmark = pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")

COORDINATOR = SimpleNamespace(camera_name="Nursery", display_name="Nursery", rtsp_path=None, device_info={})


def test_url_per_source():
    from const import build_rtsp_url

    assert build_rtsp_url("h", 1, "Nursery", "c") == "rtsp://h:1/Nursery"
    assert build_rtsp_url("h", 1, "Nursery", "c", source="auto") == "rtsp://h:1/Nursery"
    assert build_rtsp_url("h", 1, "Nursery", "c", source="local") == "rtsp://h:1/Nursery/local"
    assert build_rtsp_url("h", 1, "Nursery", "c", "Nursery_c", source="local") == "rtsp://h:1/Nursery_c/local"


def _select(monkeypatch, sources, last_state=None):
    module = load(monkeypatch, "select")
    entity = module.AventStreamSourceSelect(COORDINATOR, "cam1", sources)
    entity.last_state = last_state
    return entity


def test_select_restores_its_option(monkeypatch):
    sources: dict[str, str] = {}
    entity = _select(monkeypatch, sources, SimpleNamespace(state="local"))
    asyncio.run(entity.async_added_to_hass())
    assert sources == {"cam1": "local"}
    assert entity.current_option == "local"
    assert dispatcher.sent == ["philips_avent_stream_source_cam1"]


@pytest.mark.parametrize("state", ["unknown", "unavailable", "webrtc"])
def test_select_ignores_a_state_it_does_not_offer(monkeypatch, state):
    sources: dict[str, str] = {}
    entity = _select(monkeypatch, sources, SimpleNamespace(state=state))
    asyncio.run(entity.async_added_to_hass())
    assert sources == {}
    assert entity.current_option == "auto"


def test_select_is_not_polled(monkeypatch):
    # Its state is Home Assistant's own: a poll every 30 s would only rewrite it.
    assert _select(monkeypatch, {}).should_poll is False


@pytest.mark.parametrize(
    "path", ["strings.json", "translations/en.json", "translations/fr.json"], ids=lambda p: p.split("/")[-1]
)
def test_select_strings_name_every_option(path):
    from const import STREAM_SOURCES

    strings = json.loads((PKG_DIR / path).read_text())
    select = strings["entity"]["select"]["stream_source"]
    assert select["name"]
    assert sorted(select["state"]) == sorted(STREAM_SOURCES)
    assert all(select["state"].values())


def test_select_option_moves_the_camera(monkeypatch):
    sources: dict[str, str] = {}
    entity = _select(monkeypatch, sources)
    asyncio.run(entity.async_select_option("local"))
    assert sources == {"cam1": "local"}
    assert entity.writes == 1
    assert dispatcher.sent == ["philips_avent_stream_source_cam1"]

    asyncio.run(entity.async_select_option("local"))  # unchanged: nothing to move
    assert dispatcher.sent == ["philips_avent_stream_source_cam1"]


def test_select_options_are_its_own(monkeypatch):
    # A list shared with the constant: a change to one entity's options would
    # change every camera's, and the constant.
    module = load(monkeypatch, "select")
    first = module.AventStreamSourceSelect(COORDINATOR, "cam1", {})
    second = module.AventStreamSourceSelect(COORDINATOR, "cam2", {})
    assert first._attr_options == ["auto", "local"]
    assert first._attr_options is not second._attr_options
    assert first._attr_options is not module.STREAM_SOURCES


@pytest.mark.parametrize("lan_streaming, offered", [(True, True), (False, False), (None, True)])
def test_select_only_with_lan_streaming(monkeypatch, lan_streaming, offered):
    module = load(monkeypatch, "select")
    options = {} if lan_streaming is None else {"lan_streaming": lan_streaming}
    entry = SimpleNamespace(entry_id="e", options=options)
    hass = SimpleNamespace(data={"philips_avent": {"e": {"coordinators": {"cam1": COORDINATOR}, "stream_sources": {}}}})
    added = []
    asyncio.run(module.async_setup_entry(hass, entry, added.extend))
    kinds = [type(e).__name__ for e in added]
    assert ("AventStreamSourceSelect" in kinds) is offered


class FakeStream:
    """Home Assistant's Stream, with a worker thread that runs or not."""

    def __init__(self, source, running):
        self.source = source
        self._thread = SimpleNamespace(is_alive=lambda: running) if running is not None else None
        self.updates: list[str] = []

    def update_source(self, url):
        self.updates.append(url)
        self.source = url


def _camera(monkeypatch, sources):
    module = load(monkeypatch, "camera")
    camera = module.AventCamera(COORDINATOR, "cam1", 38554, "localhost", False, sources)
    return camera


def test_camera_streams_from_the_selected_path(monkeypatch):
    sources: dict[str, str] = {}
    camera = _camera(monkeypatch, sources)
    assert asyncio.run(camera.stream_source()) == "rtsp://localhost:38554/Nursery"
    sources["cam1"] = "local"
    assert asyncio.run(camera.stream_source()) == "rtsp://localhost:38554/Nursery/local"


def test_camera_restarts_a_running_worker_on_the_new_path(monkeypatch):
    sources: dict[str, str] = {}
    camera = _camera(monkeypatch, sources)
    asyncio.run(camera.async_added_to_hass())
    camera.stream = FakeStream("rtsp://localhost:38554/Nursery", running=True)

    sources["cam1"] = "local"
    dispatcher.send(None, "philips_avent_stream_source_cam1")

    assert camera.stream.updates == ["rtsp://localhost:38554/Nursery/local"]


@pytest.mark.parametrize("running", [False, None])
def test_camera_only_repoints_a_stopped_stream(monkeypatch, running):
    sources: dict[str, str] = {}
    camera = _camera(monkeypatch, sources)
    asyncio.run(camera.async_added_to_hass())
    camera.stream = FakeStream("rtsp://localhost:38554/Nursery", running=running)

    sources["cam1"] = "local"
    dispatcher.send(None, "philips_avent_stream_source_cam1")

    assert camera.stream.updates == []  # update_source would leave the fast-restart flag set
    assert camera.stream.source == "rtsp://localhost:38554/Nursery/local"


def test_camera_without_a_stream_has_nothing_to_move(monkeypatch):
    sources: dict[str, str] = {"cam1": "local"}
    camera = _camera(monkeypatch, sources)
    asyncio.run(camera.async_added_to_hass())
    dispatcher.send(None, "philips_avent_stream_source_cam1")  # no stream yet: no error
    camera.stream = FakeStream("rtsp://localhost:38554/Nursery/local", running=True)
    dispatcher.send(None, "philips_avent_stream_source_cam1")
    assert camera.stream.updates == []  # already on the path


def test_camera_setup_shares_the_selects_options(monkeypatch):
    module = load(monkeypatch, "camera")
    sources = {"cam1": "local"}
    entry = SimpleNamespace(entry_id="e", options={})
    hass = SimpleNamespace(data={"philips_avent": {"e": {"coordinators": {"cam1": COORDINATOR}, "stream_sources": sources}}})
    added = []
    asyncio.run(module.async_setup_entry(hass, entry, added.extend))
    assert asyncio.run(added[0].stream_source()) == "rtsp://localhost:38554/Nursery/local"
