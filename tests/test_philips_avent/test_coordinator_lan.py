"""How the coordinator uses the cloud around a LAN session (fork a59b77f, 619981b).

A session that negotiated 3.4 or up pushes state and alarms on its own, so the
cloud poll pauses and a delivered command is not mirrored. Below 3.4 everything
stays as it was: the maintainer's SCD973 on firmware 1.4.0 speaks 3.3.
"""
from __future__ import annotations

import asyncio

import aiohttp
import pytest

from .ha_stand_in import home_assistant_installed, load

pytestmark = pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")

STATE = {"138": False, "207": 2345}


class FakeEntry:
    def __init__(self):
        self.reauths = 0

    def async_start_reauth(self, hass):
        self.reauths += 1


class FakeLan:
    def __init__(self, version: float | None, delivered: bool = True, protocol: str = ""):
        self.connected = version is not None
        # A connected fake has proven its key unless a test says otherwise.
        self.proven = self.connected
        self.session_version = version
        self.protocol = protocol
        self.delivered = delivered
        self.sent: list[dict] = []
        self.keys: list[str] = []
        self.ip = "10.0.0.7"

    def set_local_key(self, key):
        self.keys.append(key)

    async def set_dps(self, dps):
        self.sent.append(dps)
        return {"success": True} if self.delivered else None

    async def stop(self):
        pass


class FakeApi:
    def __init__(
        self, rssi_error: Exception | None = None, local_key: str | None = None, device_error: Exception | None = None
    ):
        self.local_key = local_key
        self.device_error = device_error
        self.published: list[dict] = []
        self.device_calls = 0
        self.rssi_calls = 0
        self.rssi_error = rssi_error

    async def set_dps(self, dev_id, dps):
        self.published.append(dps)
        return {}

    async def get_device(self, dev_id):
        self.device_calls += 1
        if self.device_error:
            raise self.device_error
        device = {"dps": {**STATE, "207": 2400}}
        if self.local_key:
            device["localKey"] = self.local_key
        return device

    async def get_rssi(self, dev_id):
        self.rssi_calls += 1
        if self.rssi_error:
            raise self.rssi_error
        return {"value": -61}

    async def get_sleep_day(self, dev_id, date):
        return []


@pytest.fixture
def make(monkeypatch):
    module = load(monkeypatch, "coordinator")

    def _make(version: float | None, *, delivered: bool = True, data: dict | None = STATE, api=None):
        coordinator = module.PhilipsAventCoordinator(object(), api or FakeApi(), "cam", "Nursery")
        coordinator.data = dict(data) if data is not None else None
        coordinator._lan_client = FakeLan(version, delivered) if version is not None else None
        return coordinator

    return _make


class TestCommands:
    def test_a_3_3_session_still_mirrors_to_the_cloud(self, make):
        coordinator = make(3.3)
        asyncio.run(coordinator.set_dps({"138": True}))
        assert coordinator._lan_client.sent == [{"138": True}]
        assert coordinator.api.published == [{"138": True}]

    def test_a_3_3_session_still_keeps_lullaby_steps_off_the_cloud(self, make):
        coordinator = make(3.3)
        asyncio.run(coordinator.set_dps({"201": "next"}))
        assert coordinator.api.published == []

    def test_a_3_5_session_keeps_a_delivered_command_local(self, make):
        coordinator = make(3.5)
        asyncio.run(coordinator.set_dps({"138": True}))
        assert coordinator._lan_client.sent == [{"138": True}]
        assert coordinator.api.published == []
        assert coordinator.data["138"] is True

    def test_a_command_the_lan_could_not_deliver_goes_through_the_cloud(self, make):
        for version in (3.3, 3.5):
            coordinator = make(version, delivered=False)
            asyncio.run(coordinator.set_dps({"201": "next"}))
            assert coordinator.api.published == [{"201": "next"}]

    def test_without_a_session_the_cloud_carries_the_command(self, make):
        coordinator = make(None)
        asyncio.run(coordinator.set_dps({"138": True}))
        assert coordinator.api.published == [{"138": True}]


class TestPoll:
    def test_a_3_3_session_keeps_polling(self, make):
        coordinator = make(3.3)
        data = asyncio.run(coordinator._async_update_data())
        assert coordinator.api.device_calls == 1
        assert data["207"] == 2400

    def test_a_3_5_session_pauses_the_poll_and_keeps_the_state(self, make):
        coordinator = make(3.5)
        data = asyncio.run(coordinator._async_update_data())
        assert coordinator.api.device_calls == 0
        assert data == STATE

    def test_the_first_refresh_always_asks_the_cloud(self, make):
        coordinator = make(3.5, data=None)
        asyncio.run(coordinator._async_update_data())
        assert coordinator.api.device_calls == 1

    def test_the_poll_resumes_once_the_session_is_gone(self, make):
        coordinator = make(3.5)
        asyncio.run(coordinator._async_update_data())
        coordinator._lan_client.connected = False
        coordinator._lan_client.session_version = None
        asyncio.run(coordinator._async_update_data())
        assert coordinator.api.device_calls == 1

    def test_a_paused_poll_still_refreshes_the_wifi_signal(self, make):
        coordinator = make(3.5)
        asyncio.run(coordinator._async_update_data())
        assert coordinator.api.rssi_calls == 1
        assert coordinator.rssi == -61

    def test_a_failed_signal_reading_keeps_the_entities_available(self, make):
        # An exception out of the update would mark every entity unavailable.
        coordinator = make(3.5, api=FakeApi(rssi_error=aiohttp.ClientError("down")))
        assert asyncio.run(coordinator._async_update_data()) == STATE

    def test_an_html_error_page_keeps_the_entities_available(self, make):
        import json

        error = json.JSONDecodeError("Expecting value", "<html>502 Bad Gateway</html>", 0)
        coordinator = make(3.5, api=FakeApi(rssi_error=error))
        assert asyncio.run(coordinator._async_update_data()) == STATE

    def test_an_empty_signal_answer_keeps_the_last_reading(self, make):
        coordinator = make(3.5)
        coordinator.rssi = -70

        async def null_answer(dev_id):
            return None

        coordinator.api.get_rssi = null_answer
        assert asyncio.run(coordinator._async_update_data()) == STATE
        assert coordinator.rssi == -70

    def test_a_refused_session_still_reaches_reauth_behind_a_paused_poll(self, make):
        import sys

        coordinator = make(3.5)
        coordinator.config_entry = FakeEntry()
        module = sys.modules[type(coordinator).__module__]
        coordinator.api.rssi_error = module.TuyaAPIError("SID_INVALID", "session invalid")
        # The live session keeps the entities available; the reauth flow still starts.
        assert asyncio.run(coordinator._async_update_data()) == STATE
        assert coordinator.config_entry.reauths == 1


class TestBaseline:
    def test_a_baseline_updates_state_without_counting_as_a_push(self, make):
        coordinator = make(3.5)
        coordinator._on_lan_dps_update({"250": "motion_detection", "207": 2500}, True)
        assert coordinator.data["207"] == 2500
        assert coordinator.lan_update_sequence == 0
        assert coordinator.last_lan_dps == {}

    def test_a_push_counts(self, make):
        coordinator = make(3.5)
        coordinator._on_lan_dps_update({"250": "motion_detection"})
        assert coordinator.lan_update_sequence == 1
        assert coordinator.last_lan_dps == {"250": "motion_detection"}


class TestAlertDetectors:
    """Pairing the alert switches follows the protocol the monitor confirmed, not the live session."""

    def test_a_3_5_monitor_runs_one_detector(self, make):
        coordinator = make(None)
        coordinator._lan_client = FakeLan(None, protocol="3.5")
        assert coordinator.alert_detectors_exclusive

    def test_a_3_3_monitor_keeps_its_switches_independent(self, make):
        coordinator = make(3.3)
        coordinator._lan_client.protocol = "3.3"
        assert not coordinator.alert_detectors_exclusive

    def test_without_a_lan_client_nothing_is_paired(self, make):
        assert not make(None).alert_detectors_exclusive


class TestOptimisticEcho:
    """Entities never write into coordinator.data; the coordinator echoes a command itself."""

    def test_a_track_choice_shows_under_the_key_the_monitor_reports_it(self, make):
        for version in (None, 3.3, 3.5):
            coordinator = make(version)
            before = coordinator.data
            echo = {"248": '{"bizcode":"phi-no-bm","id":3542155,"errcode":0}'}
            asyncio.run(coordinator.set_dps({"202": "{}", "201": "play"}, optimistic=echo))
            assert coordinator.data["248"] == echo["248"]
            assert coordinator.data["246"] == "playing"
            assert "248" not in before


class TestLocalKey:
    """Entries store no local key; the coordinator adopts the one the first refresh returns."""

    def _capture(self, coordinator, monkeypatch):
        import sys

        started = []

        class Client:
            def __init__(self, hass, device_id, local_key, *args, **kwargs):
                started.append(local_key)

            async def start(self):
                pass

        monkeypatch.setattr(sys.modules[type(coordinator).__module__], "TuyaLANClient", Client)
        return started

    def test_the_key_comes_from_the_first_refresh(self, make, monkeypatch):
        coordinator = make(None)
        coordinator.device_info = {"localKey": "fake-local-key"}
        started = self._capture(coordinator, monkeypatch)
        asyncio.run(coordinator.start_lan())
        assert started == ["fake-local-key"]
        assert coordinator.local_key == "fake-local-key"

    def test_without_a_key_there_is_no_lan_session(self, make, monkeypatch):
        coordinator = make(None)
        started = self._capture(coordinator, monkeypatch)
        asyncio.run(coordinator.start_lan())
        assert started == []
        assert coordinator.local_key is None


class TestPushesLeaveThePollAlone:
    """Home Assistant's async_set_updated_data reschedules the next refresh a full interval out.

    Called on every push, SenseIQ statuses every few seconds kept a 3.3 monitor's
    cloud poll, its only source of alarms, from ever running.
    """

    def _scheduled(self, coordinator):
        clock = [0.0]
        coordinator.clock = lambda: clock[0]
        coordinator._schedule_refresh()  # what Home Assistant does after a refresh
        return clock, coordinator.scheduled_for

    def test_the_stand_in_reschedules_as_home_assistant_does(self, make):
        coordinator = make(3.3)
        clock, due = self._scheduled(coordinator)
        clock[0] = 10
        coordinator.async_set_updated_data(dict(STATE))
        assert coordinator.scheduled_for == due + 10

    def test_pushes_faster_than_the_poll_do_not_delay_it(self, make):
        coordinator = make(3.3)
        clock, due = self._scheduled(coordinator)
        for second in range(3, 300, 3):
            clock[0] = second
            coordinator._on_lan_dps_update({"3": f'{{"r":"b","br":{20 + second % 3}}}', "207": 2300 + second})
        assert coordinator.scheduled_for == due
        assert coordinator.data["207"] == 2300 + 297
        assert coordinator.listener_updates == 99

    def test_a_settled_lullaby_state_does_not_delay_it_either(self, make):
        import time

        coordinator = make(3.3)
        clock, due = self._scheduled(coordinator)
        clock[0] = 10
        coordinator._pending_lullaby = "playing"
        coordinator._pending_lullaby_since = time.monotonic() - 60
        coordinator._commit_lullaby_state()
        assert coordinator.data["246"] == "playing"
        assert coordinator.scheduled_for == due

    def test_a_push_restores_availability_after_a_failed_poll(self, make):
        # A push proves the LAN session is live (round 3: LAN whenever possible).
        coordinator = make(3.3)
        coordinator.last_update_success = False
        coordinator._on_lan_dps_update({"207": 2500})
        assert coordinator.last_update_success is True
        assert coordinator.data["207"] == 2500


class TestRotatedLocalKey:
    """A key the cloud rotated reaches the running LAN client and the bridge config."""

    def _with_key(self, make, api_key, version=3.3):
        coordinator = make(version, api=FakeApi(local_key=api_key))
        coordinator._local_key = "fake-old-key"
        rewrites = []
        coordinator._rewrite_bridge_config = rewrites.append
        return coordinator, rewrites

    def test_a_new_key_is_handed_over_and_the_bridge_config_rewritten(self, make):
        coordinator, rewrites = self._with_key(make, "fake-new-key")
        asyncio.run(coordinator._async_update_data())
        assert coordinator._lan_client.keys == ["fake-new-key"]
        assert coordinator.local_key == "fake-new-key"
        assert rewrites == ["10.0.0.7"]
        asyncio.run(coordinator._async_update_data())
        assert coordinator._lan_client.keys == ["fake-new-key"]
        assert rewrites == ["10.0.0.7"]

    def test_the_same_key_changes_nothing(self, make):
        coordinator, rewrites = self._with_key(make, "fake-old-key")
        asyncio.run(coordinator._async_update_data())
        assert coordinator._lan_client.keys == []
        assert rewrites == []

    def test_a_device_record_without_a_key_keeps_the_one_in_use(self, make):
        coordinator, rewrites = self._with_key(make, None)
        asyncio.run(coordinator._async_update_data())
        assert coordinator.local_key == "fake-old-key"
        assert rewrites == []


class TestCloudFailureBehindALanSession:
    """LAN whenever possible: a live session keeps the entities available through a cloud failure."""

    def _module(self, coordinator):
        import sys

        return sys.modules[type(coordinator).__module__]

    def _messages(self, caplog, level):
        return [r.getMessage() for r in caplog.records if r.levelname == level and "Cloud poll" in r.getMessage()]

    def test_failures_keep_the_state_and_log_once_each_way(self, make, caplog):
        caplog.set_level("DEBUG")
        coordinator = make(3.3, api=FakeApi(device_error=aiohttp.ClientError("cloud down")))
        for _ in range(3):
            assert asyncio.run(coordinator._async_update_data()) == STATE
        assert len(self._messages(caplog, "WARNING")) == 1
        coordinator.api.device_error = None
        assert asyncio.run(coordinator._async_update_data())["207"] == 2400
        asyncio.run(coordinator._async_update_data())
        assert len(self._messages(caplog, "INFO")) == 1
        assert len(self._messages(caplog, "WARNING")) == 1

    def test_every_kind_of_cloud_failure_is_covered(self, make):
        coordinator = make(3.3)
        module = self._module(coordinator)
        for error in (
            aiohttp.ClientError("down"), TimeoutError(), ValueError("not json"),
            module.TuyaAPIError("SYSTEM_ERROR", "busy"),
        ):
            coordinator.api.device_error = error
            assert asyncio.run(coordinator._async_update_data()) == STATE

    def test_a_refused_session_starts_reauth_once_and_stays_available(self, make):
        coordinator = make(3.3)
        coordinator.config_entry = FakeEntry()
        coordinator.api.device_error = self._module(coordinator).TuyaAPIError("SID_INVALID", "session invalid")
        for _ in range(3):
            assert asyncio.run(coordinator._async_update_data()) == STATE
        assert coordinator.config_entry.reauths == 1

    def test_without_a_lan_session_the_failure_raises_as_before(self, make, caplog):
        caplog.set_level("DEBUG")
        coordinator = make(None, api=FakeApi(device_error=aiohttp.ClientError("cloud down")))
        with pytest.raises(aiohttp.ClientError):
            asyncio.run(coordinator._async_update_data())
        module = self._module(coordinator)
        coordinator.api.device_error = module.TuyaAPIError("SYSTEM_ERROR", "busy")
        with pytest.raises(module.UpdateFailed):
            asyncio.run(coordinator._async_update_data())
        coordinator.api.device_error = module.TuyaAPIError("SID_INVALID", "session invalid")
        with pytest.raises(module.ConfigEntryAuthFailed):
            asyncio.run(coordinator._async_update_data())
        assert self._messages(caplog, "WARNING") == []

    def test_a_dropped_session_hands_availability_back_to_the_poll(self, make):
        coordinator = make(3.3, api=FakeApi(device_error=aiohttp.ClientError("cloud down")))
        assert asyncio.run(coordinator._async_update_data()) == STATE
        coordinator._lan_client.connected = coordinator._lan_client.proven = False
        with pytest.raises(aiohttp.ClientError):
            asyncio.run(coordinator._async_update_data())

    def test_the_first_refresh_still_needs_the_cloud(self, make):
        coordinator = make(3.3, data=None, api=FakeApi(device_error=aiohttp.ClientError("cloud down")))
        with pytest.raises(aiohttp.ClientError):
            asyncio.run(coordinator._async_update_data())

    def test_a_session_coming_up_restores_availability_at_once(self, make):
        coordinator = make(3.3)
        coordinator.last_update_success = False
        before = coordinator.listener_updates
        coordinator._mark_lan_live()
        assert coordinator.last_update_success is True
        assert coordinator.listener_updates == before + 1

    def test_an_available_coordinator_is_left_alone(self, make):
        coordinator = make(3.3)
        before = coordinator.listener_updates
        coordinator._mark_lan_live()
        assert coordinator.listener_updates == before


class TestOnlyAProvenSessionCounts:
    """A bare 3.3 connect proves neither the key nor the version (review M7, M3)."""

    def test_an_unproven_session_does_not_cover_a_failing_poll(self, make):
        coordinator = make(3.3, api=FakeApi(device_error=aiohttp.ClientError("cloud down")))
        coordinator._lan_client.proven = False
        with pytest.raises(aiohttp.ClientError):
            asyncio.run(coordinator._async_update_data())
        coordinator._lan_client.proven = True
        assert asyncio.run(coordinator._async_update_data()) == STATE

    def test_an_unproven_session_does_not_restore_availability(self, make):
        coordinator = make(3.3)
        coordinator._lan_client.proven = False
        coordinator.last_update_success = False
        coordinator._mark_lan_live()
        assert coordinator.last_update_success is False

    def test_a_lullaby_settling_after_the_session_dropped_leaves_availability_alone(self, make):
        import time

        coordinator = make(3.3)
        coordinator._lan_client.connected = coordinator._lan_client.proven = False
        coordinator.last_update_success = False
        coordinator._pending_lullaby = "playing"
        coordinator._pending_lullaby_since = time.monotonic() - 60
        coordinator._commit_lullaby_state()
        assert coordinator.data["246"] == "playing"
        assert coordinator.last_update_success is False
