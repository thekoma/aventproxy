"""The "WebRTC over the LAN only" option, from the options flow to the bridge JSON.

The Go side reads it as ``BridgeConfig.WebRTCLANOnly`` (``webrtc_lan_only``) in
``cmd/addon/addon.go``.
"""

import json
from pathlib import Path

import pytest
from const import CONF_WEBRTC_LAN_ONLY, DEFAULT_WEBRTC_LAN_ONLY
from payload import build_bridge_config, write_bridge_config_file

COMPONENT = Path(__file__).parent.parent.parent / "custom_components" / "philips_avent"

BASE = {
    "signing_key": "sk", "sid": "eu166", "ecode": "E", "partner": "P",
    "app_key": "AK", "device_id": "D", "package_name": "pkg",
    "api_host": "a1.tuyaeu.com", "bridge_port": 38554,
    "cameras": [{"deviceId": "abc", "deviceName": "Cam"}],
}


def config_from_options(options: dict) -> dict:
    """What ``_write_bridge_config`` builds from the entry options."""
    return build_bridge_config(
        **BASE, webrtc_lan_only=options.get(CONF_WEBRTC_LAN_ONLY, DEFAULT_WEBRTC_LAN_ONLY)
    )


class TestWebRTCLANOnly:
    def test_default_is_off(self):
        assert DEFAULT_WEBRTC_LAN_ONLY is False
        assert config_from_options({})["webrtc_lan_only"] is False
        assert build_bridge_config(**BASE)["webrtc_lan_only"] is False

    def test_option_key_is_the_bridge_key(self):
        assert CONF_WEBRTC_LAN_ONLY == "webrtc_lan_only"

    @pytest.mark.parametrize("value", [True, False])
    def test_round_trip_through_the_file(self, tmp_path, value):
        path = tmp_path / "philips_avent_bridge_x.json"
        write_bridge_config_file(path, config_from_options({CONF_WEBRTC_LAN_ONLY: value}))
        assert json.loads(path.read_text())["webrtc_lan_only"] is value

    def test_options_flow_offers_it(self):
        source = (COMPONENT / "config_flow.py").read_text(encoding="utf-8")
        assert "vol.Optional(CONF_WEBRTC_LAN_ONLY, default=current_webrtc_lan_only): bool" in source

    def test_integration_writes_it(self):
        source = (COMPONENT / "__init__.py").read_text(encoding="utf-8")
        assert "webrtc_lan_only=entry.options.get(CONF_WEBRTC_LAN_ONLY, DEFAULT_WEBRTC_LAN_ONLY)" in source

    @pytest.mark.parametrize("path", ["strings.json", "translations/en.json", "translations/fr.json"])
    def test_translated(self, path):
        step = json.loads((COMPONENT / path).read_text(encoding="utf-8"))["options"]["step"]["init"]
        assert step["data"]["webrtc_lan_only"]
        assert step["data_description"]["webrtc_lan_only"]
