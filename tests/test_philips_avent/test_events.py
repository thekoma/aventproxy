"""Tests for alert event payloads (issues #40, #42, #59, #61).

The base64 sample is the real DPS 212 value from the diagnostics attached to #61
by an SCD951 owner whose motion sensor never fired.
"""

import base64
import json

from events import (
    EVENT_MAX_AGE_SECONDS,
    decode_event_payload,
    event_timestamp,
    is_new_event,
    motion_event_timestamp,
    sound_event_timestamp,
)

# Verbatim from the #61 dump, DPS 212.
SCD951_MOTION = (
    "eyJ2IjoiNC4wIiwiYnVja2V0IjoidHktZXUtc3RvcmFnZTMwIiwiY21kIjoiaXBjX21vdGlvbiIsInR5cGUiOiJpbWFnZSIs"
    "IndpdGgiOiJyZXNvdXJjZSIsImFsYXJtIjp0cnVlLCJ0aW1lIjoxNzgzNjg2NTkxLCJmaWxlcyI6W1siL2M5YWE3Mi0yOTcz"
    "MjM3MzMtdGNkejUwYzlmNDdmYzM1ZWNjOGMvdW5pZnkvMTc4MzY4NjU5MS5qcGVnIiwiNmYyYzQxNzIxOGYyNDA4MiJdXX0="
)
MOTION_STAMP = 1783686591


def encoded(payload: dict) -> str:
    return base64.b64encode(json.dumps(payload).encode()).decode()


class TestRealPayloadFromTheIssue:
    def test_decodes_the_scd951_record(self):
        payload = decode_event_payload(SCD951_MOTION)
        assert payload["cmd"] == "ipc_motion"
        assert payload["alarm"] is True
        assert payload["time"] == MOTION_STAMP

    def test_reads_it_as_a_motion_event(self):
        assert motion_event_timestamp(SCD951_MOTION) == MOTION_STAMP

    def test_is_not_read_as_a_sound_event(self):
        assert sound_event_timestamp(SCD951_MOTION) is None


class TestDecoding:
    def test_plain_json_is_accepted(self):
        assert decode_event_payload('{"cmd":"ipc_motion","time":5}')["time"] == 5

    def test_dict_passes_through(self):
        assert decode_event_payload({"cmd": "ipc_motion"})["cmd"] == "ipc_motion"

    def test_unreadable_values_yield_nothing(self):
        for value in ("", "   ", None, 0, [], "not base64 and not json", "eyJ==="):
            assert decode_event_payload(value) is None

    def test_base64_of_non_json_yields_nothing(self):
        assert decode_event_payload(base64.b64encode(b"hello").decode()) is None

    def test_json_that_is_not_an_object_yields_nothing(self):
        assert decode_event_payload("[1, 2, 3]") is None


class TestTimestamps:
    def test_seconds(self):
        assert event_timestamp({"time": MOTION_STAMP}) == MOTION_STAMP

    def test_milliseconds_are_converted(self):
        assert event_timestamp({"time": MOTION_STAMP * 1000}) == MOTION_STAMP

    def test_numeric_string(self):
        assert event_timestamp({"time": str(MOTION_STAMP)}) == MOTION_STAMP

    def test_missing_or_junk(self):
        assert event_timestamp({}) is None
        assert event_timestamp({"time": "yesterday"}) is None
        assert event_timestamp({"time": True}) is None
        assert event_timestamp({"time": 0}) is None
        assert event_timestamp(None) is None


class TestCommandMatching:
    def test_alarm_false_is_not_an_event(self):
        assert motion_event_timestamp(encoded({"cmd": "ipc_motion", "alarm": False, "time": 5})) is None

    def test_alarm_absent_still_counts(self):
        # Not every firmware sets the flag; the command is the signal.
        assert motion_event_timestamp(encoded({"cmd": "ipc_motion", "time": 5})) == 5

    def test_sound_commands(self):
        for cmd in ("ipc_sound", "ipc_decibel", "sound", "decibel"):
            assert sound_event_timestamp(encoded({"cmd": cmd, "time": 7})) == 7

    def test_command_is_case_insensitive(self):
        assert motion_event_timestamp(encoded({"cmd": "IPC_Motion", "time": 9})) == 9

    def test_unrelated_commands_are_ignored(self):
        # DPS 205 and 206 on the SCD860 carry payloads in the same shape.
        assert motion_event_timestamp(encoded({"v": "1.0", "power_off": "1"})) is None
        assert motion_event_timestamp(encoded({"v": "1.0", "ota": "1"})) is None
        assert sound_event_timestamp(encoded({"cmd": "ipc_motion", "time": 5})) is None


class TestFreshness:
    NOW = 1783686591.0

    def test_a_recent_unseen_event_fires(self):
        assert is_new_event(self.NOW, None, self.NOW)

    def test_the_same_event_does_not_fire_twice(self):
        assert not is_new_event(self.NOW, self.NOW, self.NOW)

    def test_an_older_event_does_not_fire(self):
        assert not is_new_event(self.NOW - 10, self.NOW, self.NOW)

    def test_a_newer_event_fires(self):
        assert is_new_event(self.NOW + 5, self.NOW, self.NOW + 5)

    def test_history_does_not_fire_on_restart(self):
        # DPS 212 keeps the last alarm indefinitely, so a fresh install polling
        # for the first time must not report motion from last week.
        old = self.NOW - EVENT_MAX_AGE_SECONDS - 1
        assert not is_new_event(old, None, self.NOW)

    def test_the_age_boundary(self):
        assert is_new_event(self.NOW - EVENT_MAX_AGE_SECONDS, None, self.NOW)
        assert not is_new_event(self.NOW - EVENT_MAX_AGE_SECONDS - 0.1, None, self.NOW)

    def test_a_clock_running_ahead_still_reports(self):
        assert is_new_event(self.NOW + 3600, None, self.NOW)

    def test_no_timestamp_never_fires(self):
        assert not is_new_event(None, None, self.NOW)
        assert not is_new_event(None, self.NOW - 1000, self.NOW)
