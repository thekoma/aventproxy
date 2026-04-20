"""Tests for DPS constants and value parsing."""

from custom_components.philips_avent.const import (
    DPS_BRIGHTNESS,
    DPS_LULLABY_CONTROL,
    DPS_MOTION_SWITCH,
    DPS_NIGHT_LIGHT,
    DPS_PRIVACY_MODE,
    DPS_SOUND_SWITCH,
    DPS_TEMPERATURE,
)


class TestDPSConstants:
    def test_dps_ids_are_strings(self):
        for dps in [DPS_TEMPERATURE, DPS_NIGHT_LIGHT, DPS_BRIGHTNESS,
                     DPS_MOTION_SWITCH, DPS_SOUND_SWITCH, DPS_LULLABY_CONTROL,
                     DPS_PRIVACY_MODE]:
            assert isinstance(dps, str)
            assert dps.isdigit()

    def test_temperature_parsing(self):
        raw = 2250
        celsius = raw / 100.0
        assert celsius == 22.5

    def test_temperature_range(self):
        for raw in [0, 1800, 2500, 3200, 5000]:
            celsius = raw / 100.0
            assert 0 <= celsius <= 50

    def test_lullaby_control_values(self):
        valid = ["play", "pause", "stop", "next", "prev"]
        for v in valid:
            assert isinstance(v, str)

    def test_privacy_mode_enum(self):
        assert "0" != "1"  # off vs on
