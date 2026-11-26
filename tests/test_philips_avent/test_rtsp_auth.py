"""The "Password on the RTSP streams" option, from the options flow to the stream URL and /status.

The Go side (pkg/rtsp/auth.go, status.go) checks the Digest answer and the
bearer token; these tests cover what the integration makes, writes and sends.
"""
from __future__ import annotations

import asyncio
import json
import logging
from pathlib import Path
from types import SimpleNamespace

import pytest
from const import (
    CONF_BRIDGE_API_TOKEN,
    CONF_BRIDGE_RTSP_PASSWORD,
    CONF_RTSP_AUTH,
    DEFAULT_RTSP_AUTH,
    RTSP_USER,
    active_api_token,
    active_rtsp_password,
    build_rtsp_url,
    redact_rtsp_url,
    rtsp_auth_options,
)
from payload import build_bridge_config
from redact import redact_secrets

from .ha_stand_in import home_assistant_installed, load

COMPONENT = Path(__file__).parent.parent.parent / "custom_components" / "philips_avent"

BASE = {
    "signing_key": "sk", "sid": "eu166", "ecode": "E", "partner": "P",
    "app_key": "AK", "device_id": "D", "package_name": "pkg",
    "api_host": "a1.tuyaeu.com", "bridge_port": 38554,
    "cameras": [{"deviceId": "abc", "deviceName": "Cam"}],
}


class TestOptions:
    def test_off_by_default(self):
        assert CONF_RTSP_AUTH == "rtsp_auth"
        assert DEFAULT_RTSP_AUTH is False
        assert rtsp_auth_options({}, {}) == {}
        assert active_rtsp_password({}) is None and active_api_token({}) is None

    def test_turning_it_on_makes_two_secrets(self):
        options = rtsp_auth_options({CONF_RTSP_AUTH: True, "bridge_port": 38554}, {})
        password, token = options[CONF_BRIDGE_RTSP_PASSWORD], options[CONF_BRIDGE_API_TOKEN]
        assert len(password) >= 43 and len(token) >= 43 and password != token
        assert active_rtsp_password(options) == password
        assert active_api_token(options) == token
        assert options["bridge_port"] == 38554

    def test_a_later_save_keeps_them(self):
        first = rtsp_auth_options({CONF_RTSP_AUTH: True}, {})
        again = rtsp_auth_options({CONF_RTSP_AUTH: True, "talkback": True}, first)
        assert again[CONF_BRIDGE_RTSP_PASSWORD] == first[CONF_BRIDGE_RTSP_PASSWORD]
        assert again[CONF_BRIDGE_API_TOKEN] == first[CONF_BRIDGE_API_TOKEN]

    def test_turning_it_off_drops_them(self):
        first = rtsp_auth_options({CONF_RTSP_AUTH: True}, {})
        off = rtsp_auth_options({CONF_RTSP_AUTH: False}, first)
        assert CONF_BRIDGE_RTSP_PASSWORD not in off and CONF_BRIDGE_API_TOKEN not in off
        assert active_rtsp_password(first | {CONF_RTSP_AUTH: False}) is None
        # Back on: new ones, so a URL that leaked earlier stops working.
        on = rtsp_auth_options({CONF_RTSP_AUTH: True}, off)
        assert on[CONF_BRIDGE_RTSP_PASSWORD] != first[CONF_BRIDGE_RTSP_PASSWORD]

    def test_options_flow_offers_it_and_adds_the_secrets(self):
        src = " ".join((COMPONENT / "config_flow.py").read_text(encoding="utf-8").split())
        assert "vol.Optional(CONF_RTSP_AUTH, default=current_rtsp_auth): bool" in src
        assert "data=rtsp_auth_options(user_input, self.config_entry.options)" in src

    @pytest.mark.parametrize("path", ["strings.json", "translations/en.json", "translations/fr.json"])
    def test_translated(self, path):
        step = json.loads((COMPONENT / path).read_text(encoding="utf-8"))["options"]["step"]["init"]
        assert step["data"]["rtsp_auth"] and step["data_description"]["rtsp_auth"]


class TestBridgeConfig:
    def test_written_only_while_on(self):
        assert "rtsp_password" not in build_bridge_config(**BASE)
        assert "api_token" not in build_bridge_config(**BASE)
        config = build_bridge_config(**BASE, rtsp_password="pw", api_token="tok")
        assert config["rtsp_password"] == "pw" and config["api_token"] == "tok"

    def test_integration_writes_the_active_ones(self):
        src = (COMPONENT / "__init__.py").read_text(encoding="utf-8")
        assert "rtsp_password=active_rtsp_password(entry.options)" in src
        assert "api_token=active_api_token(entry.options)" in src


class TestRedaction:
    def test_the_secrets_are_redacted(self):
        options = rtsp_auth_options({CONF_RTSP_AUTH: True}, {})
        config = {"rtsp_password": "pw-0123456789", "api_token": "tk-0123456789"}
        dump = json.dumps(redact_secrets({**options, **config}))
        for secret in (options[CONF_BRIDGE_RTSP_PASSWORD], options[CONF_BRIDGE_API_TOKEN], *config.values()):
            assert secret not in dump


class TestStreamURL:
    def test_without_a_password_it_is_unchanged(self):
        assert build_rtsp_url("localhost", 38554, "Nursery", "bf1") == "rtsp://localhost:38554/Nursery"

    def test_the_password_is_quoted_and_redacted(self):
        url = build_rtsp_url("localhost", 38554, "Nursery", "bf1", password="a/b:c@d")
        assert url == f"rtsp://{RTSP_USER}:a%2Fb%3Ac%40d@localhost:38554/Nursery"
        assert redact_rtsp_url(url) == "rtsp://***@localhost:38554/Nursery"
        assert redact_rtsp_url("rtsp://localhost:38554/Nursery") == "rtsp://localhost:38554/Nursery"
        url = build_rtsp_url("localhost", 38554, "Nursery", "bf1", "Nursery_bf1", password="pw")
        assert url == f"rtsp://{RTSP_USER}:pw@localhost:38554/Nursery_bf1"


@pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")
class TestEntities:
    def test_the_camera_streams_with_the_password_and_logs_without(self, monkeypatch, caplog):
        module = load(monkeypatch, "camera")

        async def ffmpeg_get_image(hass, input_source, width=None, height=None):
            raise RuntimeError("no ffmpeg here")

        monkeypatch.setattr(module, "ffmpeg_get_image", ffmpeg_get_image)
        entity = module.AventCamera(
            SimpleNamespace(camera_name="Nursery", display_name="Nursery", rtsp_path=None, device_info={}),
            "bf1camera",
            rtsp_password="secret-pw",
        )
        assert asyncio.run(entity.stream_source()) == f"rtsp://{RTSP_USER}:secret-pw@localhost:38554/Nursery"
        with caplog.at_level(logging.ERROR):
            assert asyncio.run(entity.async_camera_image()) is None
        assert "ffmpeg snapshot failed" in caplog.text and "secret-pw" not in caplog.text

    def test_the_camera_platform_passes_the_active_password(self, monkeypatch):
        module = load(monkeypatch, "camera")
        options = rtsp_auth_options({CONF_RTSP_AUTH: True}, {})
        coordinator = SimpleNamespace(camera_name="Nursery", display_name="Nursery", rtsp_path=None, device_info={})
        hass = SimpleNamespace(data={module.DOMAIN: {"e": {"coordinators": {"bf1": coordinator}}}})
        added = []
        entry = SimpleNamespace(entry_id="e", options=options)
        asyncio.run(module.async_setup_entry(hass, entry, added.extend))
        assert options[CONF_BRIDGE_RTSP_PASSWORD] in asyncio.run(added[0].stream_source())

    def test_the_status_poll_sends_the_token(self, monkeypatch):
        from .test_bridge_status import DOC, FakeResponse, FakeSession

        sensor = load(monkeypatch, "sensor")
        for token, header in ((None, None), ("tok", {"Authorization": "Bearer tok"})):
            session = FakeSession(FakeResponse(DOC))
            status = sensor.BridgeStatusCoordinator(None, session, "http://localhost:38555/status", token=token)
            asyncio.run(status._async_update_data())
            assert session.headers == [header]

    def test_the_sensor_platform_polls_with_the_active_token(self, monkeypatch):
        from .test_bridge_status import DOC, FakeCamCoordinator, FakeResponse, FakeSession

        sensor = load(monkeypatch, "sensor")
        monkeypatch.setattr(sensor, "build_device_info", lambda coordinator, cam_id: {})
        monkeypatch.setattr(sensor, "add_entities_when_ready", lambda *args, **kwargs: None)
        options = rtsp_auth_options({CONF_RTSP_AUTH: True}, {})
        entry = SimpleNamespace(entry_id="e1", options=options, async_on_unload=lambda func: None)
        hass = SimpleNamespace(data={sensor.DOMAIN: {"e1": {
            "session": FakeSession(FakeResponse(DOC)), "coordinators": {"cam1": FakeCamCoordinator()}}}})
        added = []
        asyncio.run(sensor.async_setup_entry(hass, entry, added.extend))
        status = next(e for e in added if isinstance(e, sensor.AventStreamPathSensor)).coordinator
        assert status._headers == {"Authorization": f"Bearer {options[CONF_BRIDGE_API_TOKEN]}"}
