"""TuyaLANClient against fake tinytuya devices: connecting, keep-alive, baseline and writes."""
from __future__ import annotations

import asyncio
from typing import ClassVar

import pytest

from .ha_stand_in import home_assistant_installed, load

pytestmark = pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")


class FakeHass:
    async def async_add_executor_job(self, func, *args):
        return func(*args)


class FakeDevice:
    """The parts of tinytuya.Device the client uses."""

    instances: ClassVar[list[FakeDevice]] = []
    write_results: ClassVar[list] = []

    def __init__(self, dev_id, ip, local_key, version=3.3):
        self.ip, self.version = ip, version
        self.written: list[tuple[str, object]] = []
        self.heartbeat_result = None
        self.status_result = None
        self.status_calls = 0
        FakeDevice.instances.append(self)

    def set_socketPersistent(self, value):
        pass

    def set_socketTimeout(self, value):
        pass

    def heartbeat(self, nowait=True):
        return self.heartbeat_result

    def status(self):
        self.status_calls += 1
        return self.status_result

    def set_value(self, key, value):
        self.written.append((key, value))
        return FakeDevice.write_results.pop(0) if FakeDevice.write_results else None

    def close(self):
        pass


@pytest.fixture
def lan(monkeypatch):
    module = load(monkeypatch, "lan")
    FakeDevice.instances = []
    FakeDevice.write_results = []
    monkeypatch.setattr(module.tinytuya, "Device", FakeDevice, raising=False)
    return module


def make_client(lan, *, last_ip=None, last_protocol=""):
    published: list[tuple[dict, bool]] = []
    client = lan.TuyaLANClient(
        FakeHass(), "dev", "key", lambda dps, baseline=False: published.append((dps, baseline)),
        None, last_ip, last_protocol,
    )
    client.published = published
    return client


def script_connect(monkeypatch, client, *, accept, scan=("10.0.0.7", 3.5)):
    """Record connection attempts; `accept` decides which (ip, version) pairs work."""
    attempts, scans = [], []

    def try_direct(ip, version):
        attempts.append((ip, version))
        return FakeDevice("dev", ip, "key", version) if accept(ip, version) else None

    async def discover():
        scans.append(True)
        return scan

    monkeypatch.setattr(client, "_try_direct_connect", try_direct)
    monkeypatch.setattr(client, "_discover_device", discover)
    return attempts, scans


class TestFirstConnect:
    def test_a_keyed_seed_is_dialled_without_a_scan(self, lan, monkeypatch):
        client = make_client(lan, last_ip="10.0.0.7", last_protocol="3.5")
        attempts, scans = script_connect(monkeypatch, client, accept=lambda ip, v: v == 3.5)
        assert asyncio.run(client._connect())
        assert attempts == [("10.0.0.7", 3.5)]
        assert scans == []
        assert client.session_version == 3.5

    def test_a_stale_seed_falls_back_to_the_scan(self, lan, monkeypatch):
        client = make_client(lan, last_ip="10.0.0.99", last_protocol="3.5")
        attempts, scans = script_connect(monkeypatch, client, accept=lambda ip, v: ip == "10.0.0.7" and v == 3.5)
        assert asyncio.run(client._connect())
        # No bare 3.3 session with whatever answers at the old address.
        assert attempts == [("10.0.0.99", 3.5), ("10.0.0.7", 3.5)]
        assert scans == [True]

    def test_a_3_3_monitor_scans_first_as_before(self, lan, monkeypatch):
        client = make_client(lan, last_ip="10.0.0.7", last_protocol="3.3")
        attempts, scans = script_connect(monkeypatch, client, accept=lambda ip, v: True, scan=("10.0.0.7", 3.3))
        assert asyncio.run(client._connect())
        assert scans == [True]
        assert attempts == [("10.0.0.7", 3.3)]

    def test_a_3_3_fallback_does_not_pin_later_reconnects(self, lan, monkeypatch):
        client = make_client(lan)
        works = {"3.5": False}
        attempts, _ = script_connect(
            monkeypatch, client, accept=lambda ip, v: v == 3.3 or works["3.5"], scan=("10.0.0.7", 3.5)
        )
        assert asyncio.run(client._connect())
        assert client.session_version == 3.3
        client._disconnect()
        works["3.5"] = True
        attempts.clear()
        assert asyncio.run(client._connect())
        assert attempts[0] == ("10.0.0.7", 3.5)
        assert client.session_version == 3.5


class TestKeepAlive:
    def test_a_push_read_by_the_keep_alive_is_returned(self, lan):
        client = make_client(lan)
        client._device = FakeDevice("dev", "10.0.0.7", "key")
        client._device.heartbeat_result = {"dps": {"212": "record"}}
        assert client._send_heartbeat() == (True, {"212": "record"})

    def test_a_dead_socket_is_reported(self, lan):
        client = make_client(lan)
        client._device = FakeDevice("dev", "10.0.0.7", "key")
        client._device.heartbeat_result = {"Error": "Network Error", "Err": "905"}
        assert client._send_heartbeat() == (False, None)


class TestBaseline:
    def test_the_answer_is_published_as_a_baseline(self, lan):
        client = make_client(lan)
        client._device = FakeDevice("dev", "10.0.0.7", "key", 3.5)
        client._device.status_result = {"dps": {"207": 2570}}
        asyncio.run(client._load_baseline())
        assert client.published == [({"207": 2570}, True)]

    def test_no_answer_publishes_nothing(self, lan):
        client = make_client(lan)
        client._device = FakeDevice("dev", "10.0.0.7", "key", 3.5)
        client._device.status_result = {"Error": "Timeout Waiting for Device", "Err": "902"}
        asyncio.run(client._load_baseline())
        assert client.published == []


class TestWrites:
    def _connected(self, lan, version=3.5):
        client = make_client(lan)
        client._ip, client._version = "10.0.0.7", version
        return client

    def test_values_go_out_one_by_one_in_order(self, lan):
        client = self._connected(lan)
        assert asyncio.run(client.set_dps({"139": False, "134": True})) == {"success": True}
        assert FakeDevice.instances[-1].written == [("139", False), ("134", True)]
        assert FakeDevice.instances[-1].version == 3.5

    def test_a_write_that_never_left_reports_failure(self, lan):
        client = self._connected(lan)
        FakeDevice.write_results = [{"Error": "Network Error: Device Unreachable", "Err": "905"}]
        assert asyncio.run(client.set_dps({"138": True})) is None

    def test_a_late_reply_still_counts_as_sent(self, lan):
        client = self._connected(lan, 3.3)
        FakeDevice.write_results = [{"Error": "Timeout Waiting for Device", "Err": "902"}]
        assert asyncio.run(client.set_dps({"201": "next"})) == {"success": True}
