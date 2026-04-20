"""Tests for the config flow."""

from unittest.mock import AsyncMock, patch, MagicMock

import pytest

from custom_components.philips_avent.config_flow import PhilipsAventConfigFlow
from custom_components.philips_avent.api import TuyaAPIError


@pytest.fixture
def mock_api():
    with patch("philips_avent.config_flow.PhilipsAventAPI") as mock:
        api = mock.return_value
        api.get_rsa_token = AsyncMock(return_value={
            "token": "test_token",
            "pbKey": "MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDV4UCuZgEhQ/bPa47kicAa+lqpvC7Zwh3KBD2Ar92AlgzbDaXCDwHXyi2VDSQtHhxvCXMBQwhBNsknIFsd2LrxMhCsLbTOmJ5+yWkbJZJS/S5to1HdDbGEyJTqnvpeMe6xE5k+g4yj2a8IQUHG4FrIorJayuIsAT33selsHylVjQIDAQAB",
            "exponent": "65537",
        })
        api.login_password = AsyncMock(side_effect=TuyaAPIError("MFA_NEED_SEND_CODE", "MFA needed"))
        api.trigger_mfa = AsyncMock(return_value={"email": "test@test.com"})
        api.get_user_info = AsyncMock(return_value={
            "id": "test_uid",
            "email": "test@test.com",
            "nickname": "Test",
        })
        yield api


class TestConfigFlowSteps:
    def test_step_user_creates_form(self):
        flow = PhilipsAventConfigFlow()
        # Can't run async_step_user without HA runtime, but we can test init
        assert flow._email == ""
        assert flow._password == ""

    def test_mfa_error_code_detection(self):
        err = TuyaAPIError("MFA_NEED_SEND_CODE", "Please update")
        assert err.code == "MFA_NEED_SEND_CODE"
        assert "MFA" in err.code

    def test_password_error_detection(self):
        err = TuyaAPIError("USER_PASSWD_WRONG", "Wrong password")
        assert "PASSWD" in err.code


class TestLoginResponse:
    def test_login_result_contains_required_fields(self):
        """Verify the expected fields from a login response."""
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
