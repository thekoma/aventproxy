#!/usr/bin/env python3
"""
Tuya Mobile SDK API Client — Complete implementation.

Supports authentication (password+MFA), device control via DPS,
WebRTC config, and all camera features.

Usage:
    from tuya_client import TuyaClient

    client = TuyaClient(signing_key="...", app_key="...")
    sid = client.login("user@example.com", "password", country_code="39")
    device = client.get_device("device_id")
    client.set_dps("device_id", {"138": True})  # turn on night light
"""

import hashlib
import hmac
import json
import os
import time
import uuid
from dataclasses import dataclass, field
from typing import Any, Optional

import requests
from Crypto.Cipher import PKCS1_v1_5
from Crypto.PublicKey import RSA

# ---------------------------------------------------------------------------
# Signing
# ---------------------------------------------------------------------------

SIGN_PARAM_WHITELIST = frozenset([
    "a", "v", "lat", "lon", "lang", "deviceId", "appVersion", "ttid",
    "isH5", "h5Token", "os", "clientId", "postData", "time", "requestId",
    "et", "n4h5", "sid", "chKey", "sp",
])


def _swap_sign_string(s: str) -> str:
    """Rearrange 32-char hex blocks: [A][B][C][D] → [B][A][D][C]."""
    if len(s) != 32:
        return s
    return s[8:16] + s[0:8] + s[24:32] + s[16:24]


def _compute_sign(params: dict[str, str], signing_key: str) -> str:
    """HMAC-SHA256 signature over filtered, sorted params."""
    filtered = {k: v for k, v in params.items() if k in SIGN_PARAM_WHITELIST and v}
    if "postData" in filtered and filtered["postData"]:
        md5 = hashlib.md5(filtered["postData"].encode()).hexdigest()
        filtered["postData"] = _swap_sign_string(md5)
    param_str = "||".join(f"{k}={filtered[k]}" for k in sorted(filtered))
    return hmac.new(signing_key.encode(), param_str.encode(), hashlib.sha256).hexdigest()


# ---------------------------------------------------------------------------
# Client
# ---------------------------------------------------------------------------

@dataclass
class TuyaClient:
    """Tuya Mobile SDK API client with HMAC-SHA256 signing."""

    signing_key: str
    app_key: str
    device_id: str = "python_tuya_client"  # phone device identifier
    ch_key: str = "071d81fa"
    base_url: str = "https://a1.tuyaeu.com/api.json"
    app_version: str = "1.8.0"
    sdk_version: str = "6.7.0"
    sid: str = ""

    def _build_params(self, action: str, version: str = "1.0",
                      post_data: Any = None) -> dict[str, str]:
        params = {
            "a": action,
            "v": version,
            "time": str(int(time.time())),
            "appVersion": self.app_version,
            "appRnVersion": "5.92",
            "channel": "oem",
            "chKey": self.ch_key,
            "clientId": self.app_key,
            "cp": "gzip",
            "deviceCoreVersion": self.sdk_version,
            "deviceId": self.device_id,
            "et": "0.0.1",
            "nd": "1",
            "lang": "en_US",
            "os": "Android",
            "osSystem": "14",
            "platform": "tuya_client",
            "requestId": str(uuid.uuid4()),
            "sdkVersion": self.sdk_version,
            "sid": self.sid,
            "timeZoneId": "Europe/Rome",
            "ttid": f"sdk_international@{self.app_key}",
        }
        if post_data is not None:
            params["postData"] = (
                json.dumps(post_data) if not isinstance(post_data, str) else post_data
            )
        params["sign"] = _compute_sign(params, self.signing_key)
        return params

    def call(self, action: str, version: str = "1.0",
             post_data: Any = None) -> dict:
        """Make an API call. Returns the full JSON response."""
        params = self._build_params(action, version, post_data)
        r = requests.post(
            self.base_url,
            data=params,
            headers={
                "User-Agent": f"Thing-UA=APP/Android/{self.app_version}/SDK/{self.sdk_version}",
                "Content-Type": "application/x-www-form-urlencoded",
            },
            timeout=15,
        )
        resp = r.json()
        if not resp.get("success"):
            raise TuyaAPIError(resp.get("errorCode", "UNKNOWN"),
                               resp.get("errorMsg", "Unknown error"))
        return resp

    def call_result(self, action: str, version: str = "1.0",
                    post_data: Any = None) -> Any:
        """Make an API call and return just the result field."""
        return self.call(action, version, post_data)["result"]

    # -- Authentication ----------------------------------------------------

    def _get_rsa_token(self, email: str, country_code: str = "39") -> dict:
        """Get a single-use RSA token for password encryption."""
        return self.call_result(
            "thing.m.user.username.token.get", "2.0",
            {"countryCode": country_code, "username": email, "isUid": False},
        )

    def _encrypt_password(self, password: str, pb_key: str) -> str:
        """MD5-hash and RSA-encrypt a password."""
        md5_pass = hashlib.md5(password.encode()).hexdigest()
        pem = f"-----BEGIN PUBLIC KEY-----\n{pb_key}\n-----END PUBLIC KEY-----"
        rsa_key = RSA.import_key(pem)
        cipher = PKCS1_v1_5.new(rsa_key)
        return cipher.encrypt(md5_pass.encode()).hex()

    def login(self, email: str, password: str,
              country_code: str = "39", mfa_code: str = "") -> str:
        """
        Login with email + password + MFA.

        Call once with mfa_code="" — this triggers MFA_NEED_SEND_CODE.
        Then call trigger_mfa() to send the code.
        Then call login() again with the 6-digit mfa_code.

        Returns the SID on success.
        """
        token_data = self._get_rsa_token(email, country_code)
        encrypted = self._encrypt_password(password, token_data["pbKey"])

        old_sid = self.sid
        self.sid = ""  # login calls don't use SID
        try:
            result = self.call_result(
                "thing.m.user.email.password.login", "3.0",
                {
                    "countryCode": country_code,
                    "email": email,
                    "passwd": encrypted,
                    "token": token_data["token"],
                    "ifencrypt": 1,
                    "options": json.dumps({"group": 1, "mfaCode": mfa_code}),
                },
            )
            self.sid = result["sid"]
            return self.sid
        except TuyaAPIError as e:
            self.sid = old_sid
            raise

    def trigger_mfa(self, email: str, password: str,
                    country_code: str = "39") -> dict:
        """Request a MFA code to be sent to the user's email."""
        token_data = self._get_rsa_token(email, country_code)
        encrypted = self._encrypt_password(password, token_data["pbKey"])

        old_sid = self.sid
        self.sid = ""
        try:
            return self.call_result(
                "thing.m.user.username.mfa.code.get", "1.0",
                {
                    "countryCode": country_code,
                    "username": email,
                    "passwd": encrypted,
                    "token": token_data["token"],
                    "ifencrypt": 1,
                    "options": json.dumps({"group": 1, "mfaCode": "null"}),
                },
            )
        finally:
            self.sid = old_sid

    def login_otp_send(self, email: str, country_code: str = "39") -> bool:
        """Send a passwordless OTP code to the user's email (alternative login)."""
        old_sid = self.sid
        self.sid = ""
        try:
            self.call("thing.m.user.email.code.send", "1.0",
                      {"email": email, "countryCode": country_code, "type": 1})
            return True
        finally:
            self.sid = old_sid

    def login_otp(self, email: str, code: str,
                  country_code: str = "39") -> str:
        """Login with a passwordless OTP code. Returns SID."""
        old_sid = self.sid
        self.sid = ""
        try:
            result = self.call_result(
                "thing.m.user.email.code.login", "1.0",
                {"email": email, "code": code, "countryCode": country_code},
            )
            self.sid = result["sid"]
            return self.sid
        except TuyaAPIError:
            self.sid = old_sid
            raise

    # -- User & Home -------------------------------------------------------

    def get_user_info(self) -> dict:
        """Get user profile and MQTT domain URLs."""
        return self.call_result("smartlife.m.user.info.get")

    def get_homes(self) -> list:
        """List all homes/spaces."""
        return self.call_result("m.life.home.space.list")

    # -- Device ------------------------------------------------------------

    def get_device(self, dev_id: str) -> dict:
        """Get full device info including DPS values and schema."""
        return self.call_result("tuya.m.device.get", post_data={"devId": dev_id})

    def set_dps(self, dev_id: str, dps: dict) -> dict:
        """Set one or more DPS values on a device."""
        return self.call_result(
            "tuya.m.device.dp.publish", "2.0",
            {"devId": dev_id, "gwId": dev_id, "dps": dps},
        )

    # -- Camera / IPC ------------------------------------------------------

    def get_rtc_config(self, dev_id: str) -> dict:
        """Get WebRTC configuration (STUN/TURN, ICE, AES key, session)."""
        return self.call_result(
            "smartlife.m.rtc.config.get", post_data={"devId": dev_id},
        )

    def get_p2p_prelink(self, dev_id: str) -> bool:
        """Signal P2P pre-connection intent."""
        return self.call_result(
            "smartlife.m.p2p.main.pre.link.get", post_data={"devId": dev_id},
        )

    def get_mqtt_token(self) -> dict:
        """Get MQTT connection token."""
        return self.call_result("smartlife.m.token.get")


class TuyaAPIError(Exception):
    def __init__(self, code: str, message: str):
        self.code = code
        self.message = message
        super().__init__(f"{code}: {message}")
