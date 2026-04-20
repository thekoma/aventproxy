"""Tuya Mobile SDK API client for Philips Avent."""

import hashlib
import hmac
import json
import logging
import time
import uuid
from typing import Any

import aiohttp

from .const import (
    TUYA_API_URL,
    TUYA_APP_KEY,
    TUYA_CH_KEY,
    TUYA_SIGNING_KEY,
)

_LOGGER = logging.getLogger(__name__)

SIGN_PARAM_WHITELIST = frozenset([
    "a", "v", "lat", "lon", "lang", "deviceId", "appVersion", "ttid",
    "isH5", "h5Token", "os", "clientId", "postData", "time", "requestId",
    "et", "n4h5", "sid", "chKey", "sp",
])


def _swap(s: str) -> str:
    if len(s) != 32:
        return s
    return s[8:16] + s[0:8] + s[24:32] + s[16:24]


def _sign(params: dict[str, str]) -> str:
    filtered = {k: v for k, v in params.items() if k in SIGN_PARAM_WHITELIST and v}
    if "postData" in filtered and filtered["postData"]:
        md5 = hashlib.md5(filtered["postData"].encode()).hexdigest()
        filtered["postData"] = _swap(md5)
    param_str = "||".join(f"{k}={filtered[k]}" for k in sorted(filtered))
    return hmac.new(
        TUYA_SIGNING_KEY.encode(), param_str.encode(), hashlib.sha256
    ).hexdigest()


class TuyaAPIError(Exception):
    def __init__(self, code: str, message: str):
        self.code = code
        self.message = message
        super().__init__(f"{code}: {message}")


class PhilipsAventAPI:
    """Async Tuya Mobile SDK client."""

    def __init__(self, session: aiohttp.ClientSession, sid: str = ""):
        self._session = session
        self.sid = sid
        self.device_id = uuid.uuid4().hex[:40]

    def _build_params(
        self, action: str, version: str = "1.0", post_data: Any = None
    ) -> dict[str, str]:
        params = {
            "a": action,
            "v": version,
            "time": str(int(time.time())),
            "appVersion": "1.8.0",
            "appRnVersion": "5.92",
            "channel": "oem",
            "chKey": TUYA_CH_KEY,
            "clientId": TUYA_APP_KEY,
            "cp": "gzip",
            "deviceCoreVersion": "6.7.0",
            "deviceId": self.device_id,
            "et": "0.0.1",
            "nd": "1",
            "lang": "en_US",
            "os": "Android",
            "osSystem": "14",
            "platform": "ha_integration",
            "requestId": str(uuid.uuid4()),
            "sdkVersion": "6.7.0",
            "sid": self.sid,
            "timeZoneId": "Europe/Rome",
            "ttid": f"sdk_international@{TUYA_APP_KEY}",
        }
        if post_data is not None:
            params["postData"] = (
                json.dumps(post_data) if not isinstance(post_data, str) else post_data
            )
        params["sign"] = _sign(params)
        return params

    async def _call(
        self, action: str, version: str = "1.0", post_data: Any = None
    ) -> dict:
        params = self._build_params(action, version, post_data)
        async with self._session.post(
            TUYA_API_URL,
            data=params,
            headers={
                "User-Agent": "Thing-UA=APP/Android/1.8.0/SDK/6.7.0",
                "Content-Type": "application/x-www-form-urlencoded",
            },
        ) as resp:
            result = await resp.json()
        if not result.get("success"):
            raise TuyaAPIError(
                result.get("errorCode", "UNKNOWN"),
                result.get("errorMsg", "Unknown error"),
            )
        return result.get("result")

    # -- Login flow --------------------------------------------------------

    async def get_rsa_token(self, email: str, country_code: str = "39") -> dict:
        return await self._call(
            "thing.m.user.username.token.get",
            "2.0",
            {"countryCode": country_code, "username": email, "isUid": False},
        )

    async def login_password(
        self, email: str, encrypted_password: str, token: str,
        country_code: str = "39", mfa_code: str = "",
    ) -> dict:
        old_sid = self.sid
        self.sid = ""
        try:
            return await self._call(
                "thing.m.user.email.password.login",
                "3.0",
                {
                    "countryCode": country_code,
                    "email": email,
                    "passwd": encrypted_password,
                    "token": token,
                    "ifencrypt": 1,
                    "options": json.dumps({"group": 1, "mfaCode": mfa_code}),
                },
            )
        except TuyaAPIError:
            self.sid = old_sid
            raise

    async def trigger_mfa(
        self, email: str, encrypted_password: str, token: str,
        country_code: str = "39",
    ) -> dict:
        old_sid = self.sid
        self.sid = ""
        try:
            return await self._call(
                "thing.m.user.username.mfa.code.get",
                "1.0",
                {
                    "countryCode": country_code,
                    "username": email,
                    "passwd": encrypted_password,
                    "token": token,
                    "ifencrypt": 1,
                    "options": json.dumps({"group": 1, "mfaCode": "null"}),
                },
            )
        finally:
            self.sid = old_sid

    @staticmethod
    def encrypt_password(password: str, pb_key: str) -> str:
        from Crypto.Cipher import PKCS1_v1_5
        from Crypto.PublicKey import RSA

        md5_pass = hashlib.md5(password.encode()).hexdigest()
        pem = f"-----BEGIN PUBLIC KEY-----\n{pb_key}\n-----END PUBLIC KEY-----"
        rsa_key = RSA.import_key(pem)
        cipher = PKCS1_v1_5.new(rsa_key)
        return cipher.encrypt(md5_pass.encode()).hex()

    # -- Device / Camera ---------------------------------------------------

    async def get_user_info(self) -> dict:
        return await self._call("smartlife.m.user.info.get")

    async def get_homes(self) -> list:
        return await self._call("m.life.home.space.list")

    async def get_device(self, dev_id: str) -> dict:
        return await self._call("tuya.m.device.get", post_data={"devId": dev_id})

    async def set_dps(self, dev_id: str, dps: dict) -> dict:
        return await self._call(
            "tuya.m.device.dp.publish",
            "2.0",
            {"devId": dev_id, "gwId": dev_id, "dps": dps},
        )

    async def get_rtc_config(self, dev_id: str) -> dict:
        return await self._call(
            "smartlife.m.rtc.config.get", post_data={"devId": dev_id}
        )

    async def discover_cameras(self) -> list[dict]:
        """Find all IPC cameras in the account."""
        homes = await self.get_homes()
        cameras = []
        for home in homes:
            try:
                rooms = await self._call(
                    "tuya.m.location.get",
                    post_data={"gid": str(home["gid"])},
                )
                if isinstance(rooms, list):
                    for room in rooms:
                        for dev in room.get("deviceList", []):
                            if dev.get("category") in ("sp", "dghsxj"):
                                cameras.append(dev)
            except TuyaAPIError:
                pass
        if not cameras:
            user_info = await self.get_user_info()
            uid = user_info["id"]
            try:
                result = await self._call(
                    "tuya.m.my.group.device.list", post_data={"uid": uid}
                )
                if isinstance(result, list):
                    for dev in result:
                        if dev.get("category") in ("sp", "dghsxj"):
                            cameras.append(dev)
            except TuyaAPIError:
                pass
        return cameras

    # -- MQTT credentials --------------------------------------------------

    @staticmethod
    def derive_mqtt_password(ecode: str) -> str:
        md5_key = hashlib.md5(TUYA_SIGNING_KEY.encode()).hexdigest()
        full = hashlib.md5((md5_key + ecode).encode()).hexdigest()
        return full[8:24]

    @staticmethod
    def derive_mqtt_username(
        sid: str, ecode: str, partner_identity: str
    ) -> str:
        md5_appkey = hashlib.md5(TUYA_APP_KEY.encode()).hexdigest()
        tail = hashlib.md5((md5_appkey + ecode).encode()).hexdigest()[-16:]
        return f"{partner_identity}_v1_{TUYA_APP_KEY}_{TUYA_CH_KEY}_mb_{sid}{tail}"

    @staticmethod
    def derive_mqtt_client_id(uid: str, device_id: str) -> str:
        uid_hash = hashlib.md5((uid + "sdkfasodifca").encode()).hexdigest()
        from .const import TUYA_PACKAGE_NAME
        return f"{TUYA_PACKAGE_NAME}_mb_{device_id}_{uid_hash}_DEFAULT"
