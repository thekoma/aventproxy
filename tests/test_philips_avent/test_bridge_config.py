"""Tests for the bridge-config camera payload builder.

Imports the real ``build_cameras_payload`` so a contract drift (e.g. someone
changing it to omit product_id) is caught here, not just in the Go integration
tests.

The import below uses the leaf form ``from payload import ...`` to match the
project convention (see ``conftest.py``): the full path
``custom_components.philips_avent.payload`` cannot be used because it triggers
``custom_components/philips_avent/__init__.py``, which imports ``homeassistant``
— a runtime dependency we deliberately keep out of unit tests.
"""

import json

from payload import (
    bridge_config_filename,
    build_bridge_config,
    build_cameras_payload,
    dps_delta,
    orphan_bridge_configs,
    truncated_dps,
)


class TestBuildCamerasPayloadShape:
    def test_product_id_propagated_for_scd951(self):
        cameras = [
            {"deviceId": "abc123", "deviceName": "Baby", "productId": "selj2idknqhjnids"},
        ]
        payload = build_cameras_payload(cameras)
        assert len(payload) == 1
        assert payload[0] == {
            "camera_id": "abc123",
            "camera_name": "Baby",
            "product_id": "selj2idknqhjnids",
            "local_key": "",
            "p2p_password": "",
            "lan_ip": "",
        }

    def test_product_id_empty_for_scd973(self):
        """No regression for devices without a productId — empty string passes through."""
        cameras = [{"deviceId": "def456", "deviceName": "Cam"}]
        assert build_cameras_payload(cameras)[0]["product_id"] == ""

    def test_mixed_cameras(self):
        cameras = [
            {"deviceId": "abc123", "deviceName": "Baby SCD951", "productId": "selj2idknqhjnids"},
            {"deviceId": "def456", "deviceName": "Baby SCD973", "productId": ""},
        ]
        payload = build_cameras_payload(cameras)
        assert payload[0]["product_id"] == "selj2idknqhjnids"
        assert payload[1]["product_id"] == ""

    def test_devid_fallback_key(self):
        """Raw Tuya discovery dicts use 'devId' not 'deviceId'."""
        cameras = [{"devId": "xyz789", "deviceName": "Cam", "productId": "someproduct"}]
        assert build_cameras_payload(cameras)[0]["camera_id"] == "xyz789"

    def test_product_key_fallback(self):
        """Some Tuya endpoints return 'productKey' instead of 'productId'."""
        cameras = [{"deviceId": "abc", "deviceName": "Cam", "productKey": "alt-product-key"}]
        assert build_cameras_payload(cameras)[0]["product_id"] == "alt-product-key"

    def test_snake_case_product_id_fallback(self):
        """A stored-entry dict (pre-conversion) uses 'product_id'."""
        cameras = [{"id": "abc", "name": "Cam", "product_id": "stored-pid"}]
        payload = build_cameras_payload(cameras)
        assert payload[0]["camera_id"] == "abc"
        assert payload[0]["camera_name"] == "Cam"
        assert payload[0]["product_id"] == "stored-pid"

    def test_name_fallback_to_default(self):
        """A camera with no name at all gets the literal default 'camera'."""
        cameras = [{"deviceId": "abc"}]
        assert build_cameras_payload(cameras)[0]["camera_name"] == "camera"

    def test_empty_input(self):
        assert build_cameras_payload([]) == []


class TestBuildCamerasPayloadPrecedence:
    """When multiple keys are present, the documented precedence applies."""

    def test_device_id_wins_over_devid(self):
        cameras = [{"deviceId": "first", "devId": "second"}]
        assert build_cameras_payload(cameras)[0]["camera_id"] == "first"

    def test_device_name_wins_over_name(self):
        cameras = [{"deviceId": "abc", "deviceName": "first", "name": "second"}]
        assert build_cameras_payload(cameras)[0]["camera_name"] == "first"

    def test_product_id_wins_over_product_key(self):
        cameras = [
            {"deviceId": "abc", "deviceName": "Cam", "productId": "id-form", "productKey": "key-form"},
        ]
        assert build_cameras_payload(cameras)[0]["product_id"] == "id-form"


class TestBuildBridgeConfig:
    """The JSON contract with ``cmd/addon/addon.go::BridgeConfig``."""

    BASE = {
        "signing_key": "sk",
        "sid": "az1661958",
        "ecode": "E",
        "partner": "P",
        "app_key": "AK",
        "device_id": "D",
        "package_name": "com.philips.ph.babymonitorplus",
        "api_host": "a1.tuyaus.com",
        "bridge_port": 38554,
        "cameras": [{"deviceId": "abc123", "deviceName": "Erik", "productId": "p1"}],
    }

    def test_api_host_is_written_for_the_bridge(self):
        # Without this the add-on would talk to the EU data center whatever the
        # account's region is (issues #44, #58).
        config = build_bridge_config(**self.BASE)
        assert config["api_host"] == "a1.tuyaus.com"

    def test_keys_match_the_go_struct(self):
        config = build_bridge_config(**self.BASE)
        assert set(config) == {
            "signing_key", "sid", "ecode", "partner", "app_key", "device_id",
            "package_name", "api_host", "talkback", "uid", "lan_streaming", "bridge_port",
            "cameras",
        }

    def test_cameras_use_the_shared_payload_builder(self):
        config = build_bridge_config(**self.BASE)
        assert config["cameras"] == [
            {
                "camera_id": "abc123", "camera_name": "Erik", "product_id": "p1",
                "local_key": "", "p2p_password": "", "lan_ip": "",
            },
        ]

    def test_cameras_carry_lan_fields(self):
        lan = {"abc123": {"local_key": "lk", "p2p_password": "pw", "lan_ip": "192.168.1.9"}}
        cameras = [*self.BASE["cameras"], {"deviceId": "other", "deviceName": "Two"}]
        config = build_bridge_config(**{**self.BASE, "cameras": cameras}, uid="u1", lan=lan)
        assert config["uid"] == "u1"
        assert config["cameras"][0]["local_key"] == "lk"
        assert config["cameras"][0]["p2p_password"] == "pw"
        assert config["cameras"][0]["lan_ip"] == "192.168.1.9"
        assert [config["cameras"][1][k] for k in ("local_key", "p2p_password", "lan_ip")] == ["", "", ""]

    def test_lan_streaming_defaults_on(self):
        assert build_bridge_config(**self.BASE)["lan_streaming"] is True

    def test_lan_streaming_written_when_off(self):
        assert build_bridge_config(**self.BASE, lan_streaming=False)["lan_streaming"] is False

    def test_unchanged_inputs_give_identical_json(self):
        # run.sh restarts the bridge on any md5 change (issue #73).
        lan = {"abc123": {"local_key": "lk", "p2p_password": "pw", "lan_ip": "192.168.1.9"}}
        first = json.dumps(build_bridge_config(**self.BASE, uid="u1", lan=lan), indent=2)
        second = json.dumps(build_bridge_config(**self.BASE, uid="u1", lan=lan), indent=2)
        assert first == second

    def test_config_is_json_serialisable(self):
        assert json.loads(json.dumps(build_bridge_config(**self.BASE))) == build_bridge_config(**self.BASE)


class TestOrphanBridgeConfigs:
    """Leftover per-entry config files (issue #52).

    Deleting and re-adding the integration mints a new entry id, so the old file
    stayed in the config directory. The add-on then had two to choose from and
    could keep using the dead entry's session and camera id, which Tuya answers
    with "No access".
    """

    LIVE = "01ABCLIVEENTRY"
    DEAD = "01XYZDEADENTRY"

    def test_filename_is_named_after_the_entry(self):
        assert bridge_config_filename(self.LIVE) == f"philips_avent_bridge_{self.LIVE}.json"

    def test_file_of_a_missing_entry_is_an_orphan(self):
        names = [bridge_config_filename(self.LIVE), bridge_config_filename(self.DEAD)]
        assert orphan_bridge_configs(names, {self.LIVE}) == [bridge_config_filename(self.DEAD)]

    def test_live_entries_are_kept(self):
        names = [bridge_config_filename(self.LIVE), bridge_config_filename(self.DEAD)]
        assert orphan_bridge_configs(names, {self.LIVE, self.DEAD}) == []

    def test_multiple_accounts_keep_their_own_files(self):
        second = "01SECONDACCOUNT"
        names = [bridge_config_filename(x) for x in (self.LIVE, second, self.DEAD)]
        assert orphan_bridge_configs(names, {self.LIVE, second}) == [bridge_config_filename(self.DEAD)]

    def test_unrelated_files_are_never_touched(self):
        names = [
            "philips_avent_bridge.json",       # the legacy single-entry file
            "configuration.yaml",
            "philips_avent_bridge_.json",      # no entry id in the name
            "something_philips_avent_bridge_x.json",
            bridge_config_filename(self.DEAD),
        ]
        assert orphan_bridge_configs(names, {self.LIVE}) == [bridge_config_filename(self.DEAD)]

    def test_no_entries_at_all_means_every_file_is_an_orphan(self):
        names = [bridge_config_filename(self.LIVE), bridge_config_filename(self.DEAD)]
        assert sorted(orphan_bridge_configs(names, set())) == sorted(names)

    def test_empty_directory(self):
        assert orphan_bridge_configs([], {self.LIVE}) == []


class TestDpsDelta:
    """The debug line that says what a poll changed (issues #42, #61)."""

    def test_only_changed_and_new_keys(self):
        old = {"207": 2310, "141": "", "212": "same"}
        new = {"207": 2600, "141": "", "212": "same", "250": "motion_detection"}
        assert dps_delta(old, new) == {"207": 2600, "250": "motion_detection"}

    def test_no_previous_state_reports_everything(self):
        assert dps_delta(None, {"207": 2310}) == {"207": 2310}
        assert dps_delta({}, {"207": 2310}) == {"207": 2310}

    def test_nothing_changed_is_empty(self):
        assert dps_delta({"207": 2310}, {"207": 2310}) == {}
        assert dps_delta({"207": 2310}, {}) == {}
        assert dps_delta({"207": 2310}, None) == {}

    def test_a_value_going_empty_still_counts(self):
        # An alert DPS clearing is a change worth seeing in the log.
        assert dps_delta({"141": "decibel_upload"}, {"141": ""}) == {"141": ""}

    def test_long_alarm_record_is_cut_but_readable(self):
        record = "e" * 900
        out = dps_delta({}, {"212": record})["212"]
        assert out.startswith("e" * 300)
        assert "+600 chars" in out
        assert len(out) < 400

    def test_short_values_are_untouched(self):
        assert dps_delta({}, {"201": "play"}) == {"201": "play"}

    def test_truncated_dps_keeps_repeated_pushes(self):
        # A push repeating a value is still an event worth logging.
        assert truncated_dps({"141": "decibel_upload"}) == {"141": "decibel_upload"}
        assert truncated_dps(None) == {}


class TestTalkbackOption:
    """Two-way audio must be opt-in (issue #72)."""

    BASE = {
        "signing_key": "sk", "sid": "eu166", "ecode": "E", "partner": "P",
        "app_key": "AK", "device_id": "D", "package_name": "pkg",
        "api_host": "a1.tuyaeu.com", "bridge_port": 38554,
        "cameras": [{"deviceId": "abc", "deviceName": "Erik"}],
    }

    def test_default_is_off(self):
        # Watching the camera must not claim the speaker.
        assert build_bridge_config(**self.BASE)["talkback"] is False

    def test_can_be_turned_on(self):
        assert build_bridge_config(**self.BASE, talkback=True)["talkback"] is True

    def test_key_is_part_of_the_go_contract(self):
        assert "talkback" in build_bridge_config(**self.BASE)


class _Coordinator:
    def __init__(self, device_info, stored_key="", lan_ip=None):
        self.device_info = device_info
        self._local_key = stored_key
        self.lan_ip = lan_ip


class _Api:
    def __init__(self, result=None, error=None):
        self._result, self._error = result, error

    async def get_rtc_config(self, dev_id):
        if self._error:
            raise self._error
        return self._result


class TestLanCredentials:
    def test_lan_credentials_prefer_fresh_key(self):
        import asyncio

        from lan_credentials import lan_credentials

        coordinators = {"cam": _Coordinator({"localKey": "new"}, stored_key="old", lan_ip="10.0.0.5")}
        out = asyncio.run(lan_credentials(_Api({"password": "pw"}), coordinators))
        assert out == {"cam": {"local_key": "new", "p2p_password": "pw", "lan_ip": "10.0.0.5"}}

    def test_failed_rtc_config_leaves_password_empty(self):
        import asyncio

        from lan_credentials import lan_credentials

        coordinators = {"cam": _Coordinator({"localKey": "new"})}
        out = asyncio.run(lan_credentials(_Api(error=RuntimeError("boom")), coordinators))
        assert out["cam"]["p2p_password"] == ""

    def test_only_strings_never_none(self):
        import asyncio

        from lan_credentials import lan_credentials

        coordinators = {"cam": _Coordinator({}, stored_key=None)}
        out = asyncio.run(lan_credentials(_Api({"password": None}), coordinators))
        assert out["cam"] == {"local_key": "", "p2p_password": "", "lan_ip": ""}

    def test_stored_key_used_when_cloud_has_none(self):
        import asyncio

        from lan_credentials import lan_credentials

        coordinators = {"cam": _Coordinator({}, stored_key="old")}
        out = asyncio.run(lan_credentials(_Api({"password": "pw"}), coordinators))
        assert out["cam"]["local_key"] == "old"
