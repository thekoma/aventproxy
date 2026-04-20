"""Config flow for Philips Avent Baby Monitor."""

import logging

import aiohttp
import voluptuous as vol

from homeassistant import config_entries
from homeassistant.const import CONF_EMAIL, CONF_PASSWORD

from .api import PhilipsAventAPI, TuyaAPIError
from .const import CONF_ECODE, CONF_PARTNER, CONF_SID, CONF_UID, DOMAIN

_LOGGER = logging.getLogger(__name__)

STEP_USER_DATA_SCHEMA = vol.Schema(
    {
        vol.Required(CONF_EMAIL): str,
        vol.Required(CONF_PASSWORD): str,
    }
)

STEP_MFA_DATA_SCHEMA = vol.Schema(
    {
        vol.Required("mfa_code"): str,
    }
)


class PhilipsAventConfigFlow(config_entries.ConfigFlow, domain=DOMAIN):
    """Handle a config flow for Philips Avent Baby Monitor."""

    VERSION = 1

    def __init__(self):
        self._email: str = ""
        self._password: str = ""
        self._api: PhilipsAventAPI | None = None
        self._session: aiohttp.ClientSession | None = None

    async def async_step_user(self, user_input=None):
        """Step 1: Email + Password."""
        errors = {}

        if user_input is not None:
            self._email = user_input[CONF_EMAIL]
            self._password = user_input[CONF_PASSWORD]

            self._session = aiohttp.ClientSession()
            self._api = PhilipsAventAPI(self._session)

            try:
                # Get RSA token
                token_data = await self._api.get_rsa_token(self._email)
                encrypted = await self.hass.async_add_executor_job(
                    PhilipsAventAPI.encrypt_password,
                    self._password, token_data["pbKey"],
                )

                # First login attempt — triggers MFA
                try:
                    await self._api.login_password(
                        self._email, encrypted, token_data["token"], mfa_code=""
                    )
                except TuyaAPIError as e:
                    if e.code != "MFA_NEED_SEND_CODE":
                        raise

                # Get fresh token and trigger MFA code
                token_data2 = await self._api.get_rsa_token(self._email)
                encrypted2 = await self.hass.async_add_executor_job(
                    PhilipsAventAPI.encrypt_password,
                    self._password, token_data2["pbKey"],
                )
                await self._api.trigger_mfa(
                    self._email, encrypted2, token_data2["token"]
                )

                return await self.async_step_mfa()

            except TuyaAPIError as e:
                _LOGGER.error("Login failed: %s", e)
                if "PASSWD" in e.code:
                    errors["base"] = "invalid_auth"
                else:
                    errors["base"] = "cannot_connect"
            except Exception:
                _LOGGER.exception("Unexpected error")
                errors["base"] = "unknown"

        return self.async_show_form(
            step_id="user",
            data_schema=STEP_USER_DATA_SCHEMA,
            errors=errors,
            description_placeholders={"app_name": "Philips Avent Baby Monitor+"},
        )

    async def async_step_mfa(self, user_input=None):
        """Step 2: MFA code from email."""
        errors = {}

        if user_input is not None:
            mfa_code = user_input["mfa_code"]

            try:
                # Get fresh RSA token for final login
                token_data = await self._api.get_rsa_token(self._email)
                encrypted = await self.hass.async_add_executor_job(
                    PhilipsAventAPI.encrypt_password,
                    self._password, token_data["pbKey"],
                )

                # Login with MFA code
                result = await self._api.login_password(
                    self._email, encrypted, token_data["token"], mfa_code=mfa_code
                )

                sid = result["sid"]
                self._api.sid = sid

                user_info = await self._api.get_user_info()

                # Discover cameras while we have a live session
                cameras = []
                try:
                    discovered = await self._api.discover_cameras()
                    for cam in discovered:
                        cameras.append({
                            "id": cam.get("devId") or cam.get("deviceId"),
                            "name": cam.get("name") or cam.get("deviceName", "camera"),
                        })
                except Exception:
                    _LOGGER.warning("Camera discovery during setup failed")

                if self._session:
                    await self._session.close()

                await self.async_set_unique_id(result["uid"])
                self._abort_if_unique_id_configured()

                return self.async_create_entry(
                    title=f"Avent - {user_info.get('nickname', self._email)}",
                    data={
                        CONF_EMAIL: self._email,
                        CONF_PASSWORD: self._password,
                        CONF_SID: sid,
                        CONF_ECODE: result.get("ecode", ""),
                        CONF_PARTNER: result.get("partnerIdentity", ""),
                        CONF_UID: result["uid"],
                        "cameras": cameras,
                    },
                )

            except TuyaAPIError as e:
                _LOGGER.error("MFA login failed: %s", e)
                if "MFA" in e.code or "CODE" in e.code:
                    errors["base"] = "invalid_mfa"
                else:
                    errors["base"] = "cannot_connect"
            except Exception:
                _LOGGER.exception("Unexpected error")
                errors["base"] = "unknown"

        return self.async_show_form(
            step_id="mfa",
            data_schema=STEP_MFA_DATA_SCHEMA,
            errors=errors,
            description_placeholders={"email": self._email},
        )
