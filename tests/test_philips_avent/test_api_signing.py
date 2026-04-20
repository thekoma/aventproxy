"""Tests for the Tuya API signing algorithm."""

import hashlib
import hmac

import pytest

# Import the signing internals
from custom_components.philips_avent.api import _sign, _swap, PhilipsAventAPI
from custom_components.philips_avent.const import TUYA_SIGNING_KEY, TUYA_APP_KEY


class TestSwapSignString:
    def test_swap_32_chars(self):
        assert _swap("AAAAAAAABBBBBBBBCCCCCCCCDDDDDDDD") == "BBBBBBBBAAAAAAAADDDDDDDDCCCCCCCC"

    def test_swap_preserves_non_32(self):
        assert _swap("short") == "short"
        assert _swap("") == ""

    def test_swap_known_value(self):
        # From Frida capture: 18634a7fd67b6366cca3137388c3a9fc → d67b636618634a7f88c3a9fccca31373
        assert _swap("18634a7fd67b6366cca3137388c3a9fc") == "d67b636618634a7f88c3a9fccca31373"


class TestSign:
    def test_basic_sign(self):
        params = {
            "a": "smartlife.p.time.get",
            "v": "1.0",
            "et": "0.0.1",
            "time": "1776696747",
            "requestId": "test-1234",
            "sid": "eu166195w4946940N8j46e73ef27267bf8e9b205b8e052debb5e1683",
            "deviceId": "4ea1245dcfe5ebfaae4fe06ed2c7344fd1aab9698a1f",
            "appVersion": "1.8.0",
            "ttid": f"sdk_international@{TUYA_APP_KEY}",
            "os": "Android",
            "lang": "en_US",
            "chKey": "071d81fa",
        }
        result = _sign(params)
        assert result == "02995e4c41e8bdd0d81d0b041d864668aecca6abef3576cbfdfa8add16b816d8"

    def test_sign_filters_non_whitelist(self):
        params_with_extra = {
            "a": "test",
            "v": "1.0",
            "time": "12345",
            "extraField": "should_be_ignored",
            "anotherOne": "also_ignored",
        }
        params_without = {
            "a": "test",
            "v": "1.0",
            "time": "12345",
        }
        assert _sign(params_with_extra) == _sign(params_without)

    def test_sign_with_postdata(self):
        params = {
            "a": "tuya.m.device.get",
            "v": "1.0",
            "time": "12345",
            "postData": '{"devId":"test123"}',
        }
        result = _sign(params)
        assert len(result) == 64  # SHA-256 hex
        # postData should be MD5+swapped in the sign string
        assert result != _sign({**params, "postData": ""})

    def test_sign_empty_postdata_ignored(self):
        params1 = {"a": "test", "v": "1.0", "time": "1"}
        params2 = {"a": "test", "v": "1.0", "time": "1", "postData": ""}
        assert _sign(params1) == _sign(params2)


class TestEncryptPassword:
    def test_encrypt_produces_hex(self):
        # Use a known PEM key
        pb_key = (
            "MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDV4UCuZgEhQ/bPa47kicAa"
            "+lqpvC7Zwh3KBD2Ar92AlgzbDaXCDwHXyi2VDSQtHhxvCXMBQwhBNsknIFsd2L"
            "rxMhCsLbTOmJ5+yWkbJZJS/S5to1HdDbGEyJTqnvpeMe6xE5k+g4yj2a8IQUH"
            "G4FrIorJayuIsAT33selsHylVjQIDAQAB"
        )
        result = PhilipsAventAPI.encrypt_password("TestPass123", pb_key)
        assert len(result) == 256  # RSA 1024-bit output = 128 bytes = 256 hex


class TestMQTTDerivation:
    def test_mqtt_password(self):
        # Known: MD5(MD5(signing_key) + ecode) middle 16
        ecode = "11u99u416646946e"
        md5_key = hashlib.md5(TUYA_SIGNING_KEY.encode()).hexdigest()
        full = hashlib.md5((md5_key + ecode).encode()).hexdigest()
        expected = full[8:24]

        result = PhilipsAventAPI.derive_mqtt_password(ecode)
        assert result == expected
        assert len(result) == 16

    def test_mqtt_username_format(self):
        result = PhilipsAventAPI.derive_mqtt_username(
            sid="test_sid",
            ecode="test_ecode",
            partner_identity="p1234",
        )
        assert result.startswith("p1234_v1_")
        assert TUYA_APP_KEY in result
        assert "_mb_test_sid" in result
        assert len(result.split("_mb_")[1]) > len("test_sid")  # has hash tail

    def test_mqtt_client_id_format(self):
        result = PhilipsAventAPI.derive_mqtt_client_id(
            uid="test_uid",
            device_id="test_device",
        )
        assert result.startswith("com.philips.ph.babymonitorplus_mb_")
        assert "test_device" in result
        assert result.endswith("_DEFAULT")
