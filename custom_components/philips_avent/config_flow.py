"""Config flow for Philips Avent Baby Monitor."""
from __future__ import annotations

import logging
from collections.abc import Mapping
from typing import Any

import aiohttp
import voluptuous as vol

from homeassistant import config_entries
from homeassistant.const import CONF_EMAIL, CONF_PASSWORD
from homeassistant.helpers.aiohttp_client import async_get_clientsession

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

    async def async_step_reauth(self, entry_data: Mapping[str, Any]) -> config_entries.ConfigFlowResult:
        """Handle reauthentication when session expires."""
        return await self.async_step_reauth_confirm()

    async def async_step_reauth_confirm(self, user_input: dict[str, Any] | None = None) -> config_entries.ConfigFlowResult:
        """Handle reauth confirmation with new credentials."""
        errors: dict[str, str] = {}

        if user_input is not None:
            try:
                session = async_get_clientsession(self.hass)
                api = PhilipsAventAPI(session)

                # Get RSA token
                token_data = await api.get_rsa_token(user_input[CONF_EMAIL])
                encrypted = await self.hass.async_add_executor_job(
                    PhilipsAventAPI.encrypt_password,
                    user_input[CONF_PASSWORD], token_data["pbKey"],
                )

                # First login attempt — triggers MFA
                try:
                    await api.login_password(
                        user_input[CONF_EMAIL], encrypted, token_data["token"], mfa_code=""
                    )
                except TuyaAPIError as e:
                    if e.code != "MFA_NEED_SEND_CODE":
                        raise

                # Get fresh token and trigger MFA code
                token_data2 = await api.get_rsa_token(user_input[CONF_EMAIL])
                encrypted2 = await self.hass.async_add_executor_job(
                    PhilipsAventAPI.encrypt_password,
                    user_input[CONF_PASSWORD], token_data2["pbKey"],
                )
                await api.trigger_mfa(
                    user_input[CONF_EMAIL], encrypted2, token_data2["token"]
                )

                # Store credentials for MFA step
                self._email = user_input[CONF_EMAIL]
                self._password = user_input[CONF_PASSWORD]
                self._api = api
                return await self.async_step_reauth_mfa()

            except TuyaAPIError as e:
                _LOGGER.error("Reauth login failed: %s", e)
                if "PASSWD" in e.code:
                    errors["base"] = "invalid_auth"
                else:
                    errors["base"] = "cannot_connect"
            except Exception:
                _LOGGER.exception("Unexpected error during reauth")
                errors["base"] = "unknown"

        return self.async_show_form(
            step_id="reauth_confirm",
            data_schema=vol.Schema({
                vol.Required(CONF_EMAIL): str,
                vol.Required(CONF_PASSWORD): str,
            }),
            errors=errors,
        )

    async def async_step_reauth_mfa(self, user_input: dict[str, Any] | None = None) -> config_entries.ConfigFlowResult:
        """Handle MFA during reauthentication."""
        errors: dict[str, str] = {}

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

                # Update the config entry with new credentials
                entry = self.hass.config_entries.async_get_entry(self.context["entry_id"])
                self.hass.config_entries.async_update_entry(
                    entry,
                    data={
                        **entry.data,
                        CONF_EMAIL: self._email,
                        CONF_PASSWORD: self._password,
                        CONF_SID: sid,
                        CONF_ECODE: result.get("ecode", ""),
                        CONF_PARTNER: result.get("partnerIdentity", ""),
                        CONF_UID: result["uid"],
                    },
                )
                await self.hass.config_entries.async_reload(entry.entry_id)
                return self.async_abort(reason="reauth_successful")

            except TuyaAPIError as e:
                _LOGGER.error("Reauth MFA failed: %s", e)
                if "MFA" in e.code or "CODE" in e.code:
                    errors["base"] = "invalid_mfa"
                else:
                    errors["base"] = "cannot_connect"
            except Exception:
                _LOGGER.exception("Unexpected error during reauth MFA")
                errors["base"] = "unknown"

        return self.async_show_form(
            step_id="reauth_mfa",
            data_schema=STEP_MFA_DATA_SCHEMA,
            errors=errors,
            description_placeholders={"email": self._email},
        )
