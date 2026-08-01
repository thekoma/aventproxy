"""Tests for the per-account API endpoint and country code (issues #44, #58).

The client used to post every request to the Central Europe host with country
code 39 hard-coded, which is why accounts in other Tuya data centers failed
with USER_SESSION_INVALID. These tests pin the request destination and the
country code actually sent.
"""

import asyncio
import json

from api import PhilipsAventAPI, new_device_id
from const import TUYA_API_URL, TUYA_DEFAULT_COUNTRY_CODE
from region import api_url


class FakeResponse:
    def __init__(self, payload):
        self._payload = payload

    async def json(self, content_type=None):
        return self._payload

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False


class FakeSession:
    """Records the URL and form data of every post."""

    def __init__(self, payload=None):
        self.calls = []
        self._payload = payload or {"success": True, "result": {"ok": True}}

    def post(self, url, data=None, headers=None):
        self.calls.append({"url": url, "data": data, "headers": headers})
        return FakeResponse(self._payload)

    @property
    def last(self):
        return self.calls[-1]

    def post_data(self, index=-1):
        return json.loads(self.calls[index]["data"]["postData"])


def run(coro):
    return asyncio.run(coro)


class TestDefaults:
    def test_defaults_stay_on_central_europe(self):
        # Existing installs must keep working unchanged.
        api = PhilipsAventAPI(FakeSession())
        assert api.api_url == TUYA_API_URL
        assert api.country_code == TUYA_DEFAULT_COUNTRY_CODE

    def test_default_url_posts_to_the_eu_host(self):
        session = FakeSession()
        api = PhilipsAventAPI(session)
        run(api.get_user_info())
        assert session.last["url"] == "https://a1.tuyaeu.com/api.json"


class TestPerAccountEndpoint:
    def test_requests_go_to_the_configured_data_center(self):
        session = FakeSession()
        api = PhilipsAventAPI(session, api_url=api_url("us"))
        run(api.get_user_info())
        assert session.last["url"] == "https://a1.tuyaus.com/api.json"

    def test_endpoint_can_be_switched_after_login(self):
        # The login response's `domain` block may report a different host.
        session = FakeSession()
        api = PhilipsAventAPI(session, api_url=api_url("eu"))
        api.api_url = api_url("in")
        run(api.get_device("dev1"))
        assert session.last["url"] == "https://a1.tuyain.com/api.json"

    def test_every_call_uses_the_same_host(self):
        session = FakeSession()
        api = PhilipsAventAPI(session, api_url=api_url("cn"))
        run(api.get_user_info())
        run(api.get_homes())
        run(api.set_dps("dev1", {"138": True}))
        assert {call["url"] for call in session.calls} == {"https://a1.tuyacn.com/api.json"}


class TestCountryCodeInLoginCalls:
    def test_instance_country_code_is_sent(self):
        session = FakeSession()
        api = PhilipsAventAPI(session, country_code="1")
        run(api.get_rsa_token("user@example.com"))
        assert session.post_data()["countryCode"] == "1"

    def test_login_and_mfa_use_the_instance_country_code(self):
        session = FakeSession()
        api = PhilipsAventAPI(session, country_code="31")
        run(api.login_password("user@example.com", "deadbeef", "tok", mfa_code="123456"))
        assert session.post_data()["countryCode"] == "31"
        run(api.trigger_mfa("user@example.com", "deadbeef", "tok"))
        assert session.post_data()["countryCode"] == "31"

    def test_explicit_country_code_still_wins(self):
        session = FakeSession()
        api = PhilipsAventAPI(session, country_code="1")
        run(api.get_rsa_token("user@example.com", country_code="91"))
        assert session.post_data()["countryCode"] == "91"

    def test_mfa_code_is_carried_in_options(self):
        session = FakeSession()
        api = PhilipsAventAPI(session, country_code="1")
        run(api.login_password("user@example.com", "deadbeef", "tok", mfa_code="424242"))
        options = json.loads(session.post_data()["options"])
        assert options == {"group": 1, "mfaCode": "424242"}

    def test_sid_is_cleared_for_login_but_restored_on_failure(self):
        session = FakeSession({"success": False, "errorCode": "USER_SESSION_INVALID"})
        api = PhilipsAventAPI(session, sid="eu-old-session", country_code="1")
        try:
            run(api.login_password("user@example.com", "deadbeef", "tok"))
        except Exception as err:
            assert err.code == "USER_SESSION_INVALID"
        assert api.sid == "eu-old-session"


class TestStableDeviceId:
    """The phone device id must survive a restart (issue #73).

    A fresh one on every start rewrote the bridge config file, which made the
    add-on restart every time and once left it stopped with no video.
    """

    def test_generated_when_not_supplied(self):
        api = PhilipsAventAPI(FakeSession())
        assert len(api.device_id) == 32
        assert api.device_id != PhilipsAventAPI(FakeSession()).device_id

    def test_supplied_id_is_kept(self):
        stored = "a" * 32
        api = PhilipsAventAPI(FakeSession(), device_id=stored)
        assert api.device_id == stored

    def test_supplied_id_reaches_the_signed_request(self):
        stored = "b" * 32
        session = FakeSession()
        api = PhilipsAventAPI(session, device_id=stored)
        run(api.get_user_info())
        assert session.last["data"]["deviceId"] == stored

    def test_empty_or_none_falls_back_to_a_fresh_one(self):
        # An entry created before the id was persisted stores nothing.
        for missing in (None, ""):
            api = PhilipsAventAPI(FakeSession(), device_id=missing)
            assert len(api.device_id) == 32

    def test_new_device_id_shape(self):
        first, second = new_device_id(), new_device_id()
        assert first != second
        assert len(first) == 32
        assert all(c in "0123456789abcdef" for c in first)
