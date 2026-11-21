"""The lullaby and night light timers' time left: counted down here, not by the monitor."""

import asyncio
import json
from pathlib import Path

import pytest

from const import Countdown, lullaby_running, night_light_running, seconds_left

from .ha_stand_in import home_assistant_installed, load

COMPONENT = Path(__file__).parent.parent.parent / "custom_components" / "philips_avent"


class TestTimeLeft:
    def test_running_and_idle(self):
        assert seconds_left(5400) == 5400
        assert seconds_left("90") == 90
        assert seconds_left(-1) == 0      # the monitor's "not running"
        assert seconds_left(0) == 0

    def test_unknown(self):
        assert seconds_left(None) is None
        assert seconds_left("") is None


def test_translations():
    for name in ("strings.json", "translations/en.json", "translations/fr.json"):
        sensors = json.loads((COMPONENT / name).read_text())["entity"]["sensor"]
        for key in ("lullaby_time_left", "night_light_time_left"):
            assert sensors[key]["name"], (name, key)


class TestCountdown:
    """The monitor holds the set length until the timer ends; the countdown is ours."""

    def test_counts_down_from_the_first_report(self):
        c = Countdown()
        c.seen(1800, now=1000.0)
        assert c.left(1000.0) == 1800
        assert c.left(1000.0 + 22 * 60) == 480        # the case of 02-11: 1800 still reported
        c.seen(1800, now=1000.0 + 22 * 60)            # the same length again keeps the end
        assert c.left(1000.0 + 22 * 60) == 480
        assert c.left(1000.0 + 1800) == 0
        assert c.left(1000.0 + 5000) == 0

    def test_rounds_up(self):
        c = Countdown()
        c.seen(300, now=0.0)
        assert c.left(299.2) == 1

    def test_end_and_new_timer(self):
        c = Countdown()
        c.seen(300, now=0.0)
        c.seen(-1, now=100.0)                         # ended (or switched off): 0, no end
        assert c.left(100.0) == 0 and c.ends_at is None
        c.seen(300, now=200.0)                        # the same length again after an end: a new timer
        assert c.left(200.0) == 300

    def test_another_length_restarts(self):
        c = Countdown()
        c.seen(5400, now=0.0)
        c.seen(1800, now=60.0)
        assert c.left(60.0) == 1800

    def test_restored_end_kept_while_the_length_is_the_same(self):
        c = Countdown(1800, 1800.0)                   # saved before a restart
        c.seen(1800, now=900.0)
        assert c.left(900.0) == 900
        c = Countdown(1800, 1800.0)
        c.seen(5400, now=900.0)                       # changed while Home Assistant was down
        assert c.left(900.0) == 5400

    def test_a_start_with_the_same_length_counts_down(self):
        # 02-11-2026: 245 read 5400 with the lullaby stopped, the saved end of
        # an earlier run had passed, and a start at the same length showed 0.
        c = Countdown(5400, 1000.0)                   # restored, the end passed
        c.seen(5400, now=2000.0, running=False)       # reported while stopped
        assert c.left(2000.0) == 0 and c.ends_at is None
        c.seen(5400, now=3000.0, running=True)        # 246 "playing" (245 a moment later)
        assert c.left(3000.0) == 5400
        c.seen(5400, now=3000.2, running=True)        # the length again: same end
        assert c.left(3600.0) == 4800

    def test_a_stop_ends_the_countdown(self):
        c = Countdown()
        c.seen(1800, now=0.0, running=True)
        c.seen(1800, now=600.0, running=False)        # stopped before the timer ran out
        assert c.left(600.0) == 0
        c.seen(None, now=900.0, running=True)         # on again: a new run of the length
        assert c.left(900.0) == 1800

    def test_restored_end_kept_while_still_running(self):
        c = Countdown(1800, 1800.0)
        c.seen(1800, now=900.0, running=True)
        assert c.left(900.0) == 900

    def test_unknown_running_keeps_the_old_rule(self):
        c = Countdown(1800, 1800.0)
        c.seen(1800, now=900.0, running=None)         # 246 not reported
        assert c.left(900.0) == 900

    def test_a_reported_pause_after_the_optimistic_stop_starts_nothing(self):
        # The Pause button writes "stopping" (coordinator.LULLABY_STATE_MAP);
        # a monitor reporting "paused" after it plays nothing, so no countdown.
        c = Countdown()
        c.seen(1800, now=0.0, running=lullaby_running({"245": 1800, "246": "playing"}))
        c.seen(1800, now=60.0, running=lullaby_running({"245": 1800, "246": "stopping"}))
        c.seen(1800, now=90.0, running=lullaby_running({"245": 1800, "246": "paused"}))
        assert c.left(90.0) == 0 and c.ends_at is None

    def test_lullaby_and_night_light_count_down_apart(self):
        # Both on at once: each countdown follows its own data points.
        lullaby, light = Countdown(), Countdown()
        dps = {"245": 1800, "246": "playing", "242": 600, "138": True}
        lullaby.seen(dps["245"], now=0.0, running=lullaby_running(dps))
        light.seen(dps["242"], now=0.0, running=night_light_running(dps))
        assert (lullaby.left(300.0), light.left(300.0)) == (1500, 300)
        dps = {**dps, "138": False}                   # the light ends first
        lullaby.seen(dps["245"], now=600.0, running=lullaby_running(dps))
        light.seen(dps["242"], now=600.0, running=night_light_running(dps))
        assert (lullaby.left(600.0), light.left(600.0)) == (1200, 0)

    def test_running_from_the_data_points(self):
        assert lullaby_running({"246": "playing"}) is True
        assert lullaby_running({"246": "stopping"}) is False
        assert lullaby_running({"246": "paused"}) is False
        assert lullaby_running({}) is None and lullaby_running(None) is None
        assert night_light_running({"138": True}) is True
        assert night_light_running({"138": False}) is False
        assert night_light_running({"138": "1"}) is None

    def test_no_countdown_with_the_timer_switched_off(self):
        # 245 and 242 may hold the set length whatever the timer switch says; with
        # the switch off the lullaby plays and the light stays on until stopped.
        assert lullaby_running({"246": "playing", "243": False}) is False
        assert lullaby_running({"246": "playing", "243": True}) is True
        assert night_light_running({"138": True, "241": False}) is False
        assert night_light_running({"138": True, "241": True}) is True
        c = Countdown()
        c.seen(5400, now=0.0, running=lullaby_running({"246": "playing", "243": False}))
        assert c.left(60.0) == 0

    def test_unknown_before_any_report(self):
        c = Countdown()
        assert c.left(0.0) is None
        c.seen(None, now=0.0)
        c.seen("", now=0.0)
        assert c.left(0.0) is None


class TestRestored:
    """The countdown saved before a Home Assistant restart."""

    def test_an_end_still_ahead_is_kept(self):
        c = Countdown.restored({"length": 1800, "ends_at": 1800.0}, now=900.0)
        assert (c.length, c.ends_at) == (1800, 1800.0)
        c.seen(1800, now=900.0, running=True)
        assert c.left(900.0) == 900

    def test_an_end_that_passed_while_down_is_dropped(self):
        # A new run of the same length then counts its whole length, not 0.
        c = Countdown.restored({"length": 5400, "ends_at": 1000.0}, now=2000.0)
        assert (c.length, c.ends_at) == (5400, None)
        c.seen(5400, now=2000.0, running=True)
        assert c.left(2000.0) == 5400

    def test_nothing_or_garbage_saved(self):
        for saved in ({}, {"length": None, "ends_at": None}, {"length": "1800", "ends_at": 5.0}, {"length": 1800}):
            c = Countdown.restored(saved, now=0.0)
            assert (c.length, c.ends_at) == (None, None), saved


@pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")
class TestTheSensor:
    """The entity around the countdown: restore, coordinator updates and its state."""

    class Coordinator:
        camera_name = "Nursery"
        device_info = None

        def __init__(self, data):
            self.data = data

    class Saved:
        def __init__(self, saved):
            self._saved = saved

        def as_dict(self):
            return self._saved

    def _sensor(self, monkeypatch, data, saved=None, now=1000.0):
        sensor = load(monkeypatch, "sensor")
        clock = {"now": now}
        monkeypatch.setattr(sensor.time, "time", lambda: clock["now"])
        entity = sensor.AventTimeLeftSensor(
            self.Coordinator(data), "cam", "245", "lullaby_time_left", "mdi:timer-music-outline", lullaby_running
        )
        entity.hass = None
        entity.writes = 0
        entity.async_write_ha_state = lambda: setattr(entity, "writes", entity.writes + 1)
        entity.async_on_remove = lambda unsub: None

        async def last_extra_data():
            return None if saved is None else self.Saved(saved)

        entity.async_get_last_extra_data = last_extra_data
        ticks = []
        monkeypatch.setattr(
            sensor, "async_track_time_interval",
            lambda hass, action, interval: ticks.append((action, interval)) or (lambda: None),
        )
        asyncio.run(entity.async_added_to_hass())
        assert [interval for _, interval in ticks] == [sensor.COUNTDOWN_TICK]
        entity.tick = lambda: ticks[0][0](None)
        return entity, clock

    def test_a_run_counts_down_and_a_stop_ends_it(self, monkeypatch):
        entity, clock = self._sensor(monkeypatch, {"245": 1800, "246": "playing"})
        assert entity._attr_unique_id == "cam_lullaby_time_left"
        assert entity.native_value == 1800
        assert entity.extra_state_attributes == {"ends_at": "1970-01-01T00:46:40+00:00"}
        clock["now"] = 1600.0
        entity._handle_coordinator_update()              # the same length again keeps the end
        assert entity.native_value == 1200 and entity.writes == 1
        entity.coordinator.data = {"245": 1800, "246": "stopping"}
        entity._handle_coordinator_update()
        assert entity.native_value == 0 and entity.extra_state_attributes is None

    def test_the_end_comes_back_after_a_restart(self, monkeypatch):
        entity, clock = self._sensor(
            monkeypatch, {"245": 1800, "246": "playing"}, saved={"length": 1800, "ends_at": 1900.0}
        )
        assert entity.native_value == 900
        assert entity.extra_restore_state_data.as_dict() == {"length": 1800, "ends_at": 1900.0}

    def test_the_state_moves_on_the_tick_only_while_a_timer_runs(self, monkeypatch):
        entity, clock = self._sensor(monkeypatch, {"245": 1800, "246": "playing"})
        clock["now"] = 1030.0
        entity.tick()
        assert entity.writes == 1 and entity.native_value == 1770
        entity.coordinator.data = {"245": 1800, "246": "stopping"}
        entity._handle_coordinator_update()
        entity.tick()
        assert entity.writes == 2                        # the stop's own write, no tick after it
