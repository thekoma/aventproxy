"""Tests for the config flow logic (without Home Assistant runtime)."""

import json

from api import TuyaAPIError, classify_login_error
from const import CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT


class TestTuyaAPIError:
    def test_mfa_error_code(self):
        err = TuyaAPIError("MFA_NEED_SEND_CODE", "Please update")
        assert err.code == "MFA_NEED_SEND_CODE"
        assert "MFA" in err.code

    def test_password_error_code(self):
        err = TuyaAPIError("USER_PASSWD_WRONG", "Wrong password")
        assert err.code == "USER_PASSWD_WRONG"
        assert "PASSWD" in err.code

    def test_error_message(self):
        err = TuyaAPIError("SOME_CODE", "Some message")
        assert str(err) == "SOME_CODE: Some message"

    def test_generic_error(self):
        err = TuyaAPIError("UNKNOWN", "Unknown error")
        assert err.code == "UNKNOWN"
        assert err.message == "Unknown error"


class TestClassifyLoginError:
    def test_password_error_maps_to_invalid_auth(self):
        assert classify_login_error("USER_PASSWD_WRONG") == "invalid_auth"

    def test_mfa_code_maps_to_invalid_mfa(self):
        assert classify_login_error("MFA_CODE_INVALID", mfa=True) == "invalid_mfa"
        assert classify_login_error("WRONG_CODE", mfa=True) == "invalid_mfa"

    def test_unknown_code_surfaces_as_tuya_error(self):
        # USER_SESSION_INVALID used to be masked as cannot_connect (issue #44).
        assert classify_login_error("USER_SESSION_INVALID") == "tuya_error"
        assert classify_login_error("USER_SESSION_INVALID", mfa=True) == "tuya_error"
        assert classify_login_error("RATE_LIMIT") == "tuya_error"

    def test_password_not_treated_as_mfa(self):
        # In the MFA step, a PASSWD code is not an MFA problem; surface the code.
        assert classify_login_error("USER_PASSWD_WRONG", mfa=True) == "tuya_error"

    def test_mfa_keyword_only_applies_in_mfa_step(self):
        # "MFA_NEED_SEND_CODE" in the user step is handled before classification;
        # if it ever reaches here without the mfa flag it is surfaced, not masked.
        assert classify_login_error("MFA_NEED_SEND_CODE") == "tuya_error"


class TestLoginResponse:
    def test_login_result_fields(self):
        mock_result = {
            "sid": "eu16619test",
            "ecode": "11u99u4test",
            "partnerIdentity": "p1234",
            "uid": "eu1661test",
            "email": "test@test.com",
            "nickname": "Test",
        }
        assert "sid" in mock_result
        assert "ecode" in mock_result
        assert "partnerIdentity" in mock_result
        assert "uid" in mock_result

    def test_mfa_triggers_on_empty_code(self):
        options = json.dumps({"group": 1, "mfaCode": ""})
        parsed = json.loads(options)
        assert parsed["mfaCode"] == ""

    def test_mfa_code_in_options(self):
        options = json.dumps({"group": 1, "mfaCode": "123456"})
        parsed = json.loads(options)
        assert parsed["mfaCode"] == "123456"


class TestOptionsFlow:
    def test_default_bridge_port(self):
        assert DEFAULT_BRIDGE_PORT == 38554

    def test_bridge_port_from_options(self):
        options = {"bridge_port": 29000}
        assert options.get(CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT) == 29000

    def test_bridge_port_fallback(self):
        options = {}
        assert options.get(CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT) == 38554

    def test_bridge_port_range_valid(self):
        assert 1024 <= DEFAULT_BRIDGE_PORT <= 65535
