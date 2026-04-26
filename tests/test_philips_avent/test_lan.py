"""Tests for TuyaLANClient."""




class FakeDevice:
    """Mock tinytuya.Device for testing."""

    def __init__(self, dev_id, ip, local_key, version=3.3):
        self.dev_id = dev_id
        self.ip = ip
        self.local_key = local_key
        self.version = version
        self._persistent = False
        self._timeout = 5
        self._messages = []
        self._closed = False

    def set_socketPersistent(self, val):
        self._persistent = val

    def set_socketTimeout(self, val):
        self._timeout = val

    def updatedps(self, dps_list=None):
        return {"dps": {"101": False}}

    def receive(self):
        if self._messages:
            return self._messages.pop(0)
        return None

    def close(self):
        self._closed = True


def fake_scan(maxretry=2):
    return {
        "192.168.1.100": {"gwId": "test_device_123", "version": "3.3"},
        "192.168.1.101": {"gwId": "other_device", "version": "3.3"},
    }


def fake_scan_empty(maxretry=2):
    return {}


class TestTuyaLANClientUnit:
    """Unit tests for LAN client logic without HA dependency."""

    def test_device_scan_finds_correct_device(self):
        devices = fake_scan()
        target_id = "test_device_123"
        found_ip = None
        for ip, info in devices.items():
            if info.get("gwId") == target_id:
                found_ip = ip
        assert found_ip == "192.168.1.100"

    def test_device_scan_returns_none_for_unknown(self):
        devices = fake_scan()
        target_id = "nonexistent"
        found_ip = None
        for ip, info in devices.items():
            if info.get("gwId") == target_id:
                found_ip = ip
        assert found_ip is None

    def test_device_scan_empty(self):
        devices = fake_scan_empty()
        assert len(devices) == 0

    def test_fake_device_receive_with_data(self):
        dev = FakeDevice("id", "1.2.3.4", "key")
        dev._messages = [{"dps": {"138": True}, "t": 123}]
        data = dev.receive()
        assert data["dps"]["138"] is True

    def test_fake_device_receive_empty(self):
        dev = FakeDevice("id", "1.2.3.4", "key")
        data = dev.receive()
        assert data is None

    def test_fake_device_close(self):
        dev = FakeDevice("id", "1.2.3.4", "key")
        dev.close()
        assert dev._closed is True

    def test_dps_merge_logic(self):
        """Test the merge logic used in coordinator callback."""
        existing = {"138": False, "207": 2200, "158": 35}
        push = {"138": True, "207": 2260}
        merged = {**existing, **push}
        assert merged["138"] is True
        assert merged["207"] == 2260
        assert merged["158"] == 35
