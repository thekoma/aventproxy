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
        self.ip, self.version, self.local_key = ip, version, local_key
        self.closed = False
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

    def receive(self):
        return None

    def close(self):
        self.closed = True


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

    def test_a_late_reply_never_repeats_a_step(self, lan):
        client = self._connected(lan, 3.3)
        FakeDevice.write_results = [{"Error": "Timeout Waiting for Device", "Err": "902"}]
        assert asyncio.run(client.set_dps({"201": "next"}, repeatable=False)) == {"success": True}

    def test_a_late_reply_sends_an_absolute_value_again(self, lan):
        client = self._connected(lan, 3.5)
        FakeDevice.write_results = [{"Error": "Timeout Waiting for Device", "Err": "902"}]
        assert asyncio.run(client.set_dps({"138": True})) is None


class TestRotatedKey:
    def test_commands_use_the_new_key(self, lan):
        client = make_client(lan)
        client._ip, client._version = "10.0.0.7", 3.3
        client.set_local_key("fake-new-key")
        asyncio.run(client.set_dps({"138": True}))
        assert FakeDevice.instances[-1].local_key == "fake-new-key"

    def test_a_session_opened_with_the_old_key_is_dropped_once(self, lan):
        client = make_client(lan)
        old = client._device = FakeDevice("dev", "10.0.0.7", "key")
        client.connected = True
        client.set_local_key("fake-new-key")
        client._drop_session_if_rekeyed()
        assert old.closed
        assert client._device is None
        assert not client.connected
        assert client._local_key == "fake-new-key"
        fresh = client._device = FakeDevice("dev", "10.0.0.7", "fake-new-key")
        client._drop_session_if_rekeyed()
        assert client._device is fresh

    def test_without_a_session_nothing_is_dropped(self, lan):
        client = make_client(lan)
        client.set_local_key("fake-new-key")
        client._drop_session_if_rekeyed()
        assert client._device is None


class TestSessionUp:
    def test_the_coordinator_hears_when_a_session_comes_up(self, lan, monkeypatch):
        announced = []
        client = lan.TuyaLANClient(
            FakeHass(), "dev", "key", lambda dps, baseline=False: None,
            on_session_up=lambda: (announced.append(True), client._stop_event.set()),
        )

        async def connect():
            client._device = FakeDevice("dev", "10.0.0.7", "key")
            client.connected = True
            return True

        monkeypatch.setattr(client, "_connect", connect)
        asyncio.run(client._run())
        assert announced == [True]

    def test_no_callback_is_fine(self, lan):
        make_client(lan)._announce_session()


class TestProven:
    """Below 3.4 a connect is bare TCP; a decrypted frame proves the key (review M7)."""

    def _run_once(self, lan, monkeypatch, version, frames):
        seen = []
        client = lan.TuyaLANClient(FakeHass(), "dev", "key", lambda dps, baseline=False: seen.append(client.proven))
        device = FakeDevice("dev", "10.0.0.7", "key", version)
        queue = list(frames)

        def receive():
            if not queue:
                client._stop_event.set()
                return None
            return queue.pop(0)

        device.receive = receive

        async def connect():
            client._device, client.connected, client._version = device, True, version
            return True

        monkeypatch.setattr(client, "_connect", connect)
        asyncio.run(client._run())
        return client, seen

    def test_a_3_3_connect_proves_nothing_until_a_frame_decrypts(self, lan, monkeypatch):
        client, _ = self._run_once(lan, monkeypatch, 3.3, [{"Error": "Unexpected Payload", "Err": "904"}])
        assert client.connected and not client.proven

    def test_a_decrypted_push_proves_a_3_3_session_before_it_is_published(self, lan, monkeypatch):
        client, seen = self._run_once(lan, monkeypatch, 3.3, [{"dps": {"207": 2500}}])
        assert seen == [True]
        assert client.proven

    def test_a_keyed_handshake_is_proof_enough(self, lan, monkeypatch):
        client, _ = self._run_once(lan, monkeypatch, 3.5, [])
        assert client.proven

    def test_a_dropped_session_has_to_prove_itself_again(self, lan):
        client = make_client(lan)
        client._device, client.connected, client._version, client._exchanged = (
            FakeDevice("dev", "10.0.0.7", "key"), True, 3.3, True,
        )
        assert client.proven
        client._disconnect()
        client.connected, client._version = True, 3.3
        assert not client.proven
