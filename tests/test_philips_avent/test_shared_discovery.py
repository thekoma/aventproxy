"""Discovery of cameras shared to the account (issue #127)."""

import asyncio

from api import PhilipsAventAPI, TuyaAPIError

OWNED = {"devId": "own1", "name": "Owned"}
SHARED = {"devId": "sh1", "name": "Shared"}


class FakeAPI(PhilipsAventAPI):
    def __init__(self, responses):
        super().__init__(session=None)
        self.responses = responses

    async def _call(self, action, version="1.0", post_data=None, extra_params=None):
        value = self.responses.get(action, TuyaAPIError("X", "no such call"))
        if isinstance(value, Exception):
            raise value
        return value


def discover(responses):
    return asyncio.run(FakeAPI(responses).discover_cameras())


def ids(cameras):
    return sorted(c["devId"] for c in cameras)


def test_shared_only_account_finds_its_camera():
    assert ids(discover({"tuya.m.my.shared.device.list": [SHARED]})) == ["sh1"]


def test_records_without_a_device_id_are_skipped():
    result = discover({"tuya.m.my.shared.device.list": [SHARED, {"name": "no id"}, "garbage", SHARED]})
    assert ids(result) == ["sh1"]


def test_a_non_list_answer_is_ignored():
    assert discover({"tuya.m.my.shared.device.list": {"unexpected": True}}) == []


def test_owner_and_shared_are_merged_and_deduplicated():
    result = discover({
        "m.life.home.space.list": [{"gid": "1"}],
        "tuya.m.location.get": [{"deviceList": [OWNED]}],
        "tuya.m.my.shared.device.list": [SHARED, OWNED],
    })
    assert ids(result) == ["own1", "sh1"]


def test_shared_failure_keeps_owned_cameras():
    result = discover({
        "m.life.home.space.list": [{"gid": "1"}],
        "tuya.m.location.get": [{"deviceList": [OWNED]}],
        "tuya.m.my.shared.device.list": TuyaAPIError("X", "boom"),
    })
    assert ids(result) == ["own1"]


def test_nothing_anywhere_returns_empty():
    assert discover({}) == []
