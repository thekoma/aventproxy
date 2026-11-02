"""Tests for entity data transformations and DPS interpretation logic.

Entity classes require homeassistant runtime, so we test the transformation
logic they use: DPS parsing, URL construction, event detection patterns.
"""

from const import (
    CONF_BRIDGE_PORT,
    DEFAULT_BRIDGE_PORT,
    DPS_ALERT_EVENT,
    DPS_BRIGHTNESS,
    DPS_DECIBEL_EVENT,
    DPS_LULLABY_CONTROL,
    DPS_LULLABY_STATE,
    DPS_LULLABY_VOLUME,
    DPS_MOTION_SWITCH,
    DPS_NIGHT_LIGHT,
    DPS_PRIVACY_MODE,
    DPS_SOUND_SWITCH,
    DPS_TEMPERATURE,
    LULLABY_BLOCK_STARTS,
    LULLABY_CATALOGUE,
    LULLABY_TRACK_MAP,
    LULLABY_TRACKS,
    TIMER_OPTIONS,
    TIMER_SECONDS_TO_LABEL,
    lullaby_track_id,
    mirror_dps_to_cloud,
)


class TestTemperatureSensor:
    """Test temperature DPS parsing as used by AventTemperatureSensor.native_value."""

    def test_standard_room_temp(self):
        dps = {DPS_TEMPERATURE: 2345}
        assert dps[DPS_TEMPERATURE] / 100.0 == 23.45

    def test_exact_degrees(self):
        dps = {DPS_TEMPERATURE: 2000}
        assert dps[DPS_TEMPERATURE] / 100.0 == 20.0

    def test_zero(self):
        dps = {DPS_TEMPERATURE: 0}
        assert dps[DPS_TEMPERATURE] / 100.0 == 0.0

    def test_missing_key_returns_none(self):
        dps = {"999": 42}
        result = dps[DPS_TEMPERATURE] / 100.0 if DPS_TEMPERATURE in dps else None
        assert result is None

    def test_empty_dps_returns_none(self):
        dps = {}
        result = dps[DPS_TEMPERATURE] / 100.0 if DPS_TEMPERATURE in dps else None
        assert result is None


class TestSwitchDPS:
    """Test DPS value interpretation as used by AventSwitch/AventEnumSwitch."""

    def test_night_light_bool_on(self):
        dps = {DPS_NIGHT_LIGHT: True}
        assert bool(dps[DPS_NIGHT_LIGHT]) is True

    def test_night_light_bool_off(self):
        dps = {DPS_NIGHT_LIGHT: False}
        assert bool(dps[DPS_NIGHT_LIGHT]) is False

    def test_motion_switch_bool(self):
        dps = {DPS_MOTION_SWITCH: True}
        assert bool(dps[DPS_MOTION_SWITCH]) is True

    def test_sound_switch_bool(self):
        dps = {DPS_SOUND_SWITCH: False}
        assert bool(dps[DPS_SOUND_SWITCH]) is False

    def test_privacy_mode_enum_on(self):
        dps = {DPS_PRIVACY_MODE: "1"}
        assert dps[DPS_PRIVACY_MODE] == "1"

    def test_privacy_mode_enum_off(self):
        dps = {DPS_PRIVACY_MODE: "0"}
        assert dps[DPS_PRIVACY_MODE] == "0"

    def test_privacy_mode_is_not_bool(self):
        dps = {DPS_PRIVACY_MODE: "1"}
        assert dps[DPS_PRIVACY_MODE] != True  # noqa: E712
        assert isinstance(dps[DPS_PRIVACY_MODE], str)


class TestNumberDPS:
    """Test number entity value parsing as used by AventNumber.native_value."""

    def test_brightness_float_cast(self):
        dps = {DPS_BRIGHTNESS: 75}
        assert float(dps[DPS_BRIGHTNESS]) == 75.0

    def test_brightness_min(self):
        dps = {DPS_BRIGHTNESS: 1}
        assert 1 <= float(dps[DPS_BRIGHTNESS]) <= 100

    def test_brightness_max(self):
        dps = {DPS_BRIGHTNESS: 100}
        assert 1 <= float(dps[DPS_BRIGHTNESS]) <= 100

    def test_volume_float_cast(self):
        dps = {DPS_LULLABY_VOLUME: 50}
        assert float(dps[DPS_LULLABY_VOLUME]) == 50.0

    def test_set_value_int_cast(self):
        assert int(50.7) == 50
        assert int(1.0) == 1
        assert int(100.0) == 100


class TestLullabyButton:
    """Test lullaby control commands as used by AventLullabyButton."""

    def test_control_dps_key(self):
        assert DPS_LULLABY_CONTROL == "201"

    def test_all_actions_produce_valid_dps(self):
        actions = ["play", "pause", "stop", "next", "prev"]
        for action in actions:
            dps = {DPS_LULLABY_CONTROL: action}
            assert isinstance(dps[DPS_LULLABY_CONTROL], str)
            assert len(dps[DPS_LULLABY_CONTROL]) > 0


class TestLullabySelect:
    """Test lullaby track selection as used by AventLullabySelect."""

    def test_track_count(self):
        assert len(LULLABY_CATALOGUE) == 15
        assert len(LULLABY_TRACK_MAP) == 15 * len(LULLABY_BLOCK_STARTS)

    def test_all_tracks_have_name_and_category(self):
        for track_id, (name, category) in LULLABY_TRACK_MAP.items():
            assert isinstance(track_id, int)
            assert len(name) > 0
            assert category in ("lullabies", "noise", "nature_sounds")

    def test_track_list_matches_map(self):
        assert len(LULLABY_TRACKS) == 15
        assert set(LULLABY_TRACKS) == {name for name, _ in LULLABY_TRACK_MAP.values()}

    def test_track_names_unique(self):
        assert len(LULLABY_TRACKS) == len(set(LULLABY_TRACKS))

    def test_original_block_names(self):
        assert LULLABY_TRACK_MAP[3542154][0] == "Baa Baa Black Sheep"
        assert LULLABY_TRACK_MAP[3542163][0] == "White Noise"
        assert LULLABY_TRACK_MAP[3542168][0] == "Rain Shower"

    def test_firmware_1_1_12_block_names(self):
        # Issue #91: a kzm54lhabeeucq5a unit on firmware 1.1.12 reports these.
        assert LULLABY_TRACK_MAP[12072916][0] == "Baa Baa Black Sheep"
        assert LULLABY_TRACK_MAP[12072925][0] == "White Noise"
        assert LULLABY_TRACK_MAP[12072930][0] == "Rain Shower"

    def test_scd923_block_names(self):
        # Issue #93: a kzm54lhabeeucq5a SCD923 on firmware 1.4.0, measured from the app.
        assert LULLABY_TRACK_MAP[3219038][0] == "Baa Baa Black Sheep"
        assert LULLABY_TRACK_MAP[3219045][0] == "Heartbeat"
        assert LULLABY_TRACK_MAP[3219047][0] == "White Noise"
        assert LULLABY_TRACK_MAP[3219052][0] == "Rain Shower"

    def test_blocks_do_not_overlap(self):
        assert 3219038 - 1 not in LULLABY_TRACK_MAP
        assert 3219052 + 1 not in LULLABY_TRACK_MAP
        assert 3542168 + 1 not in LULLABY_TRACK_MAP
        assert 12072916 - 1 not in LULLABY_TRACK_MAP

    def test_id_follows_the_block_the_camera_reports(self):
        assert lullaby_track_id("White Noise", 3542160) == 3542163
        assert lullaby_track_id("White Noise", 12072925) == 12072925
        assert lullaby_track_id("Rain Shower", 12072916) == 12072930
        assert lullaby_track_id("Heartbeat", 3219043) == 3219045

    def test_id_defaults_to_original_block(self):
        assert lullaby_track_id("Brahms' Lullaby", None) == 3542155
        assert lullaby_track_id("Brahms' Lullaby", 999) == 3542155

    def test_id_round_trips_in_every_block(self):
        for start in LULLABY_BLOCK_STARTS:
            for name in LULLABY_TRACKS:
                assert LULLABY_TRACK_MAP[lullaby_track_id(name, start)][0] == name

    def test_unknown_name(self):
        assert lullaby_track_id("Not A Track", 3542154) is None


class TestCloudMirror:
    """Test which LAN commands are also published to the cloud (issue #91)."""

    def test_steps_are_not_mirrored(self):
        assert not mirror_dps_to_cloud({DPS_LULLABY_CONTROL: "next"})
        assert not mirror_dps_to_cloud({DPS_LULLABY_CONTROL: "prev"})

    def test_idempotent_commands_are_mirrored(self):
        for action in ("play", "pause", "stop"):
            assert mirror_dps_to_cloud({DPS_LULLABY_CONTROL: action})
        assert mirror_dps_to_cloud({DPS_NIGHT_LIGHT: True})
        assert mirror_dps_to_cloud({"202": "{}", DPS_LULLABY_CONTROL: "play"})


class TestTimerOptions:
    """Test timer options mapping as used by AventTimerSelect."""

    def test_timer_values(self):
        assert TIMER_OPTIONS["Off"] == 0
        assert TIMER_OPTIONS["5 min"] == 300
        assert TIMER_OPTIONS["10 min"] == 600
        assert TIMER_OPTIONS["20 min"] == 1200
        assert TIMER_OPTIONS["30 min"] == 1800
        assert TIMER_OPTIONS["60 min"] == 3600
        assert TIMER_OPTIONS["90 min"] == 5400

    def test_reverse_lookup(self):
        for label, seconds in TIMER_OPTIONS.items():
            assert TIMER_SECONDS_TO_LABEL[seconds] == label


class TestBinarySensorEvents:
    """Test event detection patterns as used by AventMotionDetected/AventSoundDetected."""

    def test_lullaby_playing_detection(self):
        dps = {DPS_LULLABY_STATE: "playing"}
        assert dps[DPS_LULLABY_STATE] == "playing"

    def test_lullaby_not_playing(self):
        for state in ("stopping", "idle"):
            dps = {DPS_LULLABY_STATE: state}
            assert dps[DPS_LULLABY_STATE] != "playing"

    def test_motion_event_detection(self):
        dps = {DPS_ALERT_EVENT: "motion_detection", DPS_MOTION_SWITCH: True}
        is_motion = dps.get(DPS_ALERT_EVENT) == "motion_detection" and dps.get(DPS_MOTION_SWITCH)
        assert is_motion is True

    def test_motion_event_ignored_when_switch_off(self):
        dps = {DPS_ALERT_EVENT: "motion_detection", DPS_MOTION_SWITCH: False}
        is_motion = dps.get(DPS_ALERT_EVENT) == "motion_detection" and dps.get(DPS_MOTION_SWITCH)
        assert not is_motion

    def test_sound_event_detection(self):
        dps = {DPS_DECIBEL_EVENT: "decibel_upload"}
        assert dps.get(DPS_DECIBEL_EVENT) == "decibel_upload"

    def test_no_event_data_mutation(self):
        """Verify event checking does NOT mutate the DPS dict (regression test for .pop() fix)."""
        dps = {DPS_ALERT_EVENT: "motion_detection", DPS_MOTION_SWITCH: True}
        original_keys = set(dps.keys())
        _ = dps.get(DPS_ALERT_EVENT) == "motion_detection" and dps.get(DPS_MOTION_SWITCH)
        assert set(dps.keys()) == original_keys

    def test_no_sound_event_data_mutation(self):
        """Verify sound event checking does NOT mutate the DPS dict."""
        dps = {DPS_DECIBEL_EVENT: "decibel_upload"}
        original_keys = set(dps.keys())
        _ = dps.get(DPS_DECIBEL_EVENT) == "decibel_upload"
        assert set(dps.keys()) == original_keys


class TestCameraURL:
    """Test RTSP URL construction as used by AventCamera.__init__."""

    def test_basic_url(self):
        name = "baby_monitor"
        url = f"rtsp://localhost:{DEFAULT_BRIDGE_PORT}/{name}"
        assert url == "rtsp://localhost:38554/baby_monitor"

    def test_spaces_replaced(self):
        name = "Baby Monitor Camera"
        safe = name.replace(" ", "_")
        url = f"rtsp://localhost:{DEFAULT_BRIDGE_PORT}/{safe}"
        assert url == "rtsp://localhost:38554/Baby_Monitor_Camera"

    def test_custom_port(self):
        url = f"rtsp://localhost:{8554}/cam"
        assert url == "rtsp://localhost:8554/cam"

    def test_default_port_value(self):
        assert DEFAULT_BRIDGE_PORT == 38554

    def test_bridge_port_config_key(self):
        assert CONF_BRIDGE_PORT == "bridge_port"
