"""The bridge's /status as the "Stream path" sensor reads it.

bridge_status.py parses the document; sensor.py polls it and shows it. The Go
side is pkg/rtsp/status.go; its tests cover what the bridge writes.
"""
from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path

import aiohttp
import pytest
from bridge_status import PATH_IDLE, STREAM_PATHS, parse_status, status_url

from multidict import CIMultiDict, CIMultiDictProxy
from yarl import URL

from .ha_stand_in import home_assistant_installed, load

ROOT = Path(__file__).parent.parent.parent

DOC = {
    "cameras": [
        {"camera_id": "cam1", "path": "local", "since": "2026-11-24T21:20:05+01:00"},
        {"camera_id": "cam2", "path": "idle", "since": "2026-11-24T21:00:00+01:00",
         "last_fallback": "local stream did not start (no video), cloud signalling",
         "last_fallback_at": "2026-11-24T20:59:56+01:00"},
    ],
}


def test_url_is_the_port_after_rtsp():
    assert status_url("192.0.2.20", 38554) == "http://192.0.2.20:38555/status"
    assert status_url("localhost", 8554) == "http://localhost:8555/status"


def test_cameras_are_read():
    cams = parse_status(DOC)
    assert cams["cam1"] == {"path": "local", "since": "2026-11-24T21:20:05+01:00", "last_fallback": None, "last_fallback_at": None}
    assert cams["cam2"]["path"] == "idle"
    assert cams["cam2"]["last_fallback"].startswith("local stream did not start (no video)")
    assert cams["cam2"]["last_fallback_at"] == "2026-11-24T20:59:56+01:00"


def test_nothing_unexpected_reaches_the_sensor():
    cams = parse_status({"cameras": [
        {"camera_id": "a", "path": "<script>", "since": 5, "last_fallback": ["x"]},
        {"path": "local"},  # no id
        "junk",
    ]})
    assert list(cams) == ["a"]
    assert cams["a"] == {"path": PATH_IDLE, "since": None, "last_fallback": None, "last_fallback_at": None}
    for bad in (None, [], "x", {"cameras": "x"}):
        assert parse_status(bad) == {}


def test_paths_match_the_bridge_and_the_translations():
    go = (ROOT / "avent-webrtc-bridge" / "pkg" / "rtsp" / "status.go").read_text()
    for path in STREAM_PATHS:
        assert f'= "{path}"' in go
    for f in ("strings.json", "translations/en.json", "translations/fr.json"):
        states = json.loads((ROOT / "custom_components" / "philips_avent" / f).read_text())["entity"]["sensor"]["stream_path"]["state"]
        assert set(states) == set(STREAM_PATHS)


class FakeResponse:
    def __init__(self, document=None, status=200, error=None):
        self.document, self.status, self.error = document, status, error

    async def __aenter__(self):
        if self.error is not None:
            raise self.error
        return self

    async def __aexit__(self, *exc):
        return False

    def raise_for_status(self):
        if self.status >= 400:
            url = URL("http://localhost:38555/status")
            info = aiohttp.RequestInfo(url, "GET", CIMultiDictProxy(CIMultiDict()), url)
            raise aiohttp.ClientResponseError(info, (), status=self.status)

    async def json(self, content_type=None):
        if isinstance(self.document, Exception):
            raise self.document
        return self.document


class FakeSession:
    def __init__(self, response):
        self.response, self.urls, self.headers = response, [], []

    def get(self, url, headers=None):
        self.urls.append(url)
        self.headers.append(headers)
        return self.response


@pytest.fixture
def sensor(monkeypatch):
    if home_assistant_installed():
        pytest.skip("uses a stand-in for Home Assistant")
    return load(monkeypatch, "sensor")


class FakeCamCoordinator:
    device_info = {}
    camera_id = "cam1"


def _poll(sensor, response):
    session = FakeSession(response)
    status = sensor.BridgeStatusCoordinator(None, session, "http://localhost:38555/status")
    return status, session


def test_the_sensor_shows_what_the_bridge_reports(sensor, monkeypatch):
    monkeypatch.setattr(sensor, "build_device_info", lambda coordinator, cam_id: {})
    status, session = _poll(sensor, FakeResponse(DOC))
    status.data = asyncio.run(status._async_update_data())
    assert session.urls == ["http://localhost:38555/status"]

    local = sensor.AventStreamPathSensor(status, FakeCamCoordinator(), "cam1")
    fell_back = sensor.AventStreamPathSensor(status, FakeCamCoordinator(), "cam2")
    unknown = sensor.AventStreamPathSensor(status, FakeCamCoordinator(), "cam3")
    assert local._attr_unique_id == "cam1_stream_path"
    assert local.native_value == "local"
    assert local.extra_state_attributes["since"] == "2026-11-24T21:20:05+01:00"
    assert fell_back.native_value == "idle"
    assert fell_back.extra_state_attributes["last_fallback"].startswith("local stream did not start")
    assert unknown.native_value is None


@pytest.mark.parametrize("response", [
    FakeResponse(error=aiohttp.ClientConnectionError("refused")),
    FakeResponse(error=TimeoutError()),
    FakeResponse(DOC, status=404),
    FakeResponse(ValueError("not JSON")),
], ids=["refused", "timeout", "404", "not-json"])
def test_a_bridge_that_does_not_answer_fails_the_poll(sensor, response):
    status, _ = _poll(sensor, response)
    with pytest.raises(sensor.UpdateFailed):
        asyncio.run(status._async_update_data())


def test_the_sensor_is_a_diagnostic_and_off_by_default(sensor):
    cls = sensor.AventStreamPathSensor
    assert cls._attr_entity_category == sensor.EntityCategory.DIAGNOSTIC
    assert cls._attr_entity_registry_enabled_default is False
    assert cls._attr_options == list(STREAM_PATHS)


def test_setup_adds_one_stream_path_sensor_per_camera(sensor, monkeypatch):
    monkeypatch.setattr(sensor, "build_device_info", lambda coordinator, cam_id: {})
    monkeypatch.setattr(sensor, "add_entities_when_ready", lambda *args, **kwargs: None)

    class Entry:
        entry_id = "e1"
        options = {"bridge_host": "192.0.2.20", "bridge_port": 8554}

        def async_on_unload(self, func):
            pass

    class Hass:
        data = {sensor.DOMAIN: {"e1": {"session": FakeSession(FakeResponse(DOC)), "coordinators": {
            "cam1": FakeCamCoordinator(), "cam2": FakeCamCoordinator()}}}}

    added = []
    asyncio.run(sensor.async_setup_entry(Hass(), Entry(), lambda entities: added.extend(entities)))
    paths = [e for e in added if isinstance(e, sensor.AventStreamPathSensor)]
    assert sorted(e._cam_id for e in paths) == ["cam1", "cam2"]
    assert paths[0].coordinator is paths[1].coordinator  # one status poll per entry
    assert paths[0].coordinator._url == "http://192.0.2.20:8555/status"


def test_an_added_sensor_polls_at_once(sensor, monkeypatch):
    base = sys.modules["homeassistant.helpers.update_coordinator"].CoordinatorEntity

    async def _added(self):
        return None

    monkeypatch.setattr(base, "async_added_to_hass", _added, raising=False)
    monkeypatch.setattr(sensor, "build_device_info", lambda coordinator, cam_id: {})
    status, _ = _poll(sensor, FakeResponse(DOC))
    requests = []

    async def _request():
        requests.append(1)

    status.async_request_refresh = _request
    entity = sensor.AventStreamPathSensor(status, FakeCamCoordinator(), "cam1")
    asyncio.run(entity.async_added_to_hass())
    assert requests == [1]
