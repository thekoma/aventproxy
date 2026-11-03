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


class FakeLan:
    def __init__(self, version: float | None, delivered: bool = True):
        self.connected = version is not None
        self.session_version = version
        self.delivered = delivered
        self.sent: list[dict] = []

    async def set_dps(self, dps):
        self.sent.append(dps)
        return {"success": True} if self.delivered else None

    async def stop(self):
        pass


class FakeApi:
    def __init__(self, rssi_error: Exception | None = None):
        self.published: list[dict] = []
        self.device_calls = 0
        self.rssi_calls = 0
        self.rssi_error = rssi_error

    async def set_dps(self, dev_id, dps):
        self.published.append(dps)
        return {}

    async def get_device(self, dev_id):
        self.device_calls += 1
        return {"dps": {**STATE, "207": 2400}}

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
