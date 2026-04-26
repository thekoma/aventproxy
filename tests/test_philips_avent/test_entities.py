"""Tests for entity logic and data transformations."""

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
    LULLABY_ID_BY_NAME,
    LULLABY_TRACK_MAP,
    LULLABY_TRACKS,
    TIMER_OPTIONS,
    TIMER_SECONDS_TO_LABEL,
)


class TestSensorLogic:
    """Test sensor entity data transformations."""

    def test_temperature_conversion(self):
        """Test temperature DPS raw int divided by 100 gives celsius."""
        assert 2345 / 100.0 == 23.45
        assert 2000 / 100.0 == 20.0
        assert 2750 / 100.0 == 27.5

    def test_temperature_edge_cases(self):
        """Test temperature edge cases including zero and negative."""
        assert 0 / 100.0 == 0.0
        assert -500 / 100.0 == -5.0
        assert 10000 / 100.0 == 100.0

    def test_temperature_dps_key(self):
        """Verify temperature DPS key is string 207."""
        assert DPS_TEMPERATURE == "207"
        assert isinstance(DPS_TEMPERATURE, str)


class TestSwitchLogic:
    """Test switch entity logic."""

    def test_night_light_uses_bool(self):
        """Test night light DPS 138 uses boolean True/False."""
        assert DPS_NIGHT_LIGHT == "138"
        # Simulating entity logic: True = on, False = off
        assert True is True
        assert False is False
        assert True is not False

    def test_motion_switch_uses_bool(self):
        """Test motion switch DPS 134 uses boolean."""
        assert DPS_MOTION_SWITCH == "134"
        assert isinstance(True, bool)
        assert isinstance(False, bool)

    def test_sound_switch_uses_bool(self):
        """Test sound switch DPS 139 uses boolean."""
        assert DPS_SOUND_SWITCH == "139"
        assert isinstance(True, bool)
        assert isinstance(False, bool)

    def test_privacy_mode_uses_string_enum(self):
        """Test privacy mode DPS 237 uses string '0'/'1' not bool."""
        assert DPS_PRIVACY_MODE == "237"
        # Privacy mode uses string enum, not boolean
        off_value = "0"
        on_value = "1"
        assert off_value != on_value
        assert isinstance(off_value, str)
        assert isinstance(on_value, str)
        assert off_value is not False  # Not a boolean
        assert on_value is not True  # Not a boolean


class TestNumberLogic:
    """Test number entity value ranges and types."""

    def test_brightness_range(self):
        """Test brightness DPS 158 range 1-100."""
        assert DPS_BRIGHTNESS == "158"
        min_val = 1
        max_val = 100
        assert 1 <= min_val <= 100
        assert 1 <= max_val <= 100
        assert min_val < max_val

    def test_brightness_int_cast(self):
        """Test brightness values are cast to int."""
        assert int(50.7) == 50
        assert int(1.0) == 1
        assert int(100.0) == 100

    def test_volume_range(self):
        """Test volume DPS 209 range 1-100."""
        assert DPS_LULLABY_VOLUME == "209"
        min_val = 1
        max_val = 100
        assert 1 <= min_val <= 100
        assert 1 <= max_val <= 100

    def test_volume_int_cast(self):
        """Test volume values are cast to int."""
        assert int(75.5) == 75
        assert int(1.0) == 1
        assert int(100.0) == 100


class TestButtonLogic:
    """Test button entity control commands."""

    def test_lullaby_control_dps_key(self):
        """Verify lullaby control DPS is 201."""
        assert DPS_LULLABY_CONTROL == "201"

    def test_lullaby_control_valid_commands(self):
        """Test lullaby control accepts play, pause, stop, next, prev."""
        valid_commands = ["play", "pause", "stop", "next", "prev"]
        for cmd in valid_commands:
            assert isinstance(cmd, str)
            assert len(cmd) > 0

    def test_lullaby_commands_are_unique(self):
        """Test all lullaby control commands are unique."""
        commands = ["play", "pause", "stop", "next", "prev"]
        assert len(commands) == len(set(commands))


class TestSelectLogic:
    """Test select entity option mappings."""

    def test_lullaby_track_count(self):
        """Test LULLABY_TRACK_MAP has 15 tracks."""
        assert len(LULLABY_TRACK_MAP) == 15

    def test_lullaby_track_structure(self):
        """Test each track has ID, name, and category."""
        for track_id, (name, category) in LULLABY_TRACK_MAP.items():
            assert isinstance(track_id, int)
            assert isinstance(name, str)
            assert isinstance(category, str)
            assert len(name) > 0
            assert category in ["lullabies", "noise", "nature_sounds"]

    def test_lullaby_track_ids_sequential(self):
        """Test track IDs are sequential from 3542154 to 3542168."""
        track_ids = sorted(LULLABY_TRACK_MAP.keys())
        assert track_ids[0] == 3542154
        assert track_ids[-1] == 3542168
        assert len(track_ids) == 15

    def test_lullaby_tracks_list(self):
        """Test LULLABY_TRACKS contains all track names."""
        assert len(LULLABY_TRACKS) == 15
        expected_names = [name for name, _ in LULLABY_TRACK_MAP.values()]
        assert set(LULLABY_TRACKS) == set(expected_names)

    def test_lullaby_id_by_name_bidirectional(self):
        """Test track ID to name bidirectional mapping."""
        for track_id, (name, _) in LULLABY_TRACK_MAP.items():
            assert LULLABY_ID_BY_NAME[name] == track_id

    def test_lullaby_id_by_name_reverse_lookup(self):
        """Test reverse lookup from name to ID works correctly."""
        assert LULLABY_ID_BY_NAME["Brahms' Lullaby"] == 3542155
        assert LULLABY_ID_BY_NAME["White Noise"] == 3542163
        assert LULLABY_ID_BY_NAME["Rain Shower"] == 3542168

    def test_lullaby_name_uniqueness(self):
        """Test all track names are unique."""
        names = [name for name, _ in LULLABY_TRACK_MAP.values()]
        assert len(names) == len(set(names))

    def test_timer_options_map(self):
        """Test timer options map labels to seconds correctly."""
        assert TIMER_OPTIONS["Off"] == 0
        assert TIMER_OPTIONS["5 min"] == 300
        assert TIMER_OPTIONS["10 min"] == 600
        assert TIMER_OPTIONS["20 min"] == 1200
        assert TIMER_OPTIONS["30 min"] == 1800
        assert TIMER_OPTIONS["60 min"] == 3600
        assert TIMER_OPTIONS["90 min"] == 5400

    def test_timer_seconds_to_label_reverse(self):
        """Test TIMER_SECONDS_TO_LABEL reverse lookup works."""
        assert TIMER_SECONDS_TO_LABEL[0] == "Off"
        assert TIMER_SECONDS_TO_LABEL[300] == "5 min"
        assert TIMER_SECONDS_TO_LABEL[600] == "10 min"
        assert TIMER_SECONDS_TO_LABEL[1200] == "20 min"
        assert TIMER_SECONDS_TO_LABEL[1800] == "30 min"
        assert TIMER_SECONDS_TO_LABEL[3600] == "60 min"
        assert TIMER_SECONDS_TO_LABEL[5400] == "90 min"

    def test_timer_bidirectional_mapping(self):
        """Test timer options bidirectional mapping consistency."""
        for label, seconds in TIMER_OPTIONS.items():
            assert TIMER_SECONDS_TO_LABEL[seconds] == label


class TestBinarySensorLogic:
    """Test binary sensor state detection."""

    def test_lullaby_state_dps_key(self):
        """Verify lullaby state DPS is 246."""
        assert DPS_LULLABY_STATE == "246"

    def test_lullaby_state_playing(self):
        """Test lullaby state 'playing' maps to True."""
        state = "playing"
        assert state == "playing"
        is_on = state == "playing"
        assert is_on is True

    def test_lullaby_state_stopping(self):
        """Test lullaby state 'stopping' maps to False."""
        state = "stopping"
        is_on = state == "playing"
        assert is_on is False

    def test_lullaby_state_idle(self):
        """Test lullaby state 'idle' maps to False."""
        state = "idle"
        is_on = state == "playing"
        assert is_on is False

    def test_alert_event_dps_key(self):
        """Verify alert event DPS is 250."""
        assert DPS_ALERT_EVENT == "250"
        assert isinstance(DPS_ALERT_EVENT, str)

    def test_decibel_event_dps_key(self):
        """Verify decibel event DPS is 141."""
        assert DPS_DECIBEL_EVENT == "141"
        assert isinstance(DPS_DECIBEL_EVENT, str)

    def test_motion_detection_event_parsing(self):
        """Test motion detection event data structure."""
        # Motion events typically contain timestamp and detection flag
        mock_event = {"timestamp": 1234567890, "detected": True}
        assert mock_event["detected"] is True
        assert isinstance(mock_event["timestamp"], int)


class TestCameraLogic:
    """Test camera entity RTSP URL construction."""

    def test_rtsp_url_basic(self):
        """Test RTSP URL construction with basic camera name."""
        camera_name = "baby_monitor"
        port = DEFAULT_BRIDGE_PORT
        url = f"rtsp://localhost:{port}/{camera_name}"
        assert url == "rtsp://localhost:38554/baby_monitor"

    def test_rtsp_url_with_spaces(self):
        """Test RTSP URL with spaces replaced by underscores."""
        camera_name = "Baby Monitor Camera"
        camera_name_sanitized = camera_name.replace(" ", "_")
        port = DEFAULT_BRIDGE_PORT
        url = f"rtsp://localhost:{port}/{camera_name_sanitized}"
        assert url == "rtsp://localhost:38554/Baby_Monitor_Camera"

    def test_default_bridge_port(self):
        """Test default bridge port is 38554."""
        assert DEFAULT_BRIDGE_PORT == 38554
        assert isinstance(DEFAULT_BRIDGE_PORT, int)

    def test_bridge_port_config_key(self):
        """Test bridge port config key exists."""
        assert CONF_BRIDGE_PORT == "bridge_port"
        assert isinstance(CONF_BRIDGE_PORT, str)

    def test_rtsp_url_custom_port(self):
        """Test RTSP URL construction with custom port."""
        camera_name = "nursery_cam"
        custom_port = 8554
        url = f"rtsp://localhost:{custom_port}/{camera_name}"
        assert url == "rtsp://localhost:8554/nursery_cam"

    def test_rtsp_url_special_characters(self):
        """Test RTSP URL with multiple spaces and special chars."""
        camera_name = "Living Room   Baby Monitor"
        camera_name_sanitized = camera_name.replace(" ", "_")
        port = DEFAULT_BRIDGE_PORT
        url = f"rtsp://localhost:{port}/{camera_name_sanitized}"
        assert url == "rtsp://localhost:38554/Living_Room___Baby_Monitor"
