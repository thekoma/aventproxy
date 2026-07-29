"""Config flow for Philips Avent Baby Monitor."""
from __future__ import annotations

import logging
from collections.abc import Mapping
from typing import Any

import voluptuous as vol

from homeassistant import config_entries
from homeassistant.const import CONF_COUNTRY, CONF_EMAIL, CONF_PASSWORD
from homeassistant.data_entry_flow import AbortFlow
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.selector import CountrySelector, CountrySelectorConfig

from .api import PhilipsAventAPI, TuyaAPIError, classify_login_error
from .const import (
    CONF_API_HOST, CONF_BRIDGE_HOST, CONF_BRIDGE_PORT, CONF_COUNTRY_CODE, CONF_ECODE, CONF_PARTNER,
    CONF_SID, CONF_UID, DEFAULT_BRIDGE_HOST, DEFAULT_BRIDGE_PORT, DOMAIN,
)
from .region import (
    COUNTRY_ROUTING, api_host, api_url, api_url_for_host, hosts_from_domain,
    is_wrong_data_center, login_candidates,
)

_LOGGER = logging.getLogger(__name__)

SUPPORTED_COUNTRIES = sorted(COUNTRY_ROUTING)

STEP_MFA_DATA_SCHEMA = vol.Schema(
    {
        vol.Required("mfa_code"): str,
    }
)


def _credentials_schema(default_country: str | None) -> vol.Schema:
    """Email, password and the country the Avent account belongs to.

    The country decides which Tuya data center holds the account, exactly as
    the country picker in the Philips app does. It is pre-filled from Home
    Assistant's own country setting, so in the normal case there is nothing
    extra to fill in.
    """
    country = vol.Required(CONF_COUNTRY, default=default_country) if default_country else vol.Required(CONF_COUNTRY)
    return vol.Schema(
        {
            vol.Required(CONF_EMAIL): str,
            vol.Required(CONF_PASSWORD): str,
            country: CountrySelector(CountrySelectorConfig(countries=SUPPORTED_COUNTRIES)),
        }
    )


class PhilipsAventConfigFlow(config_entries.ConfigFlow, domain=DOMAIN):
    """Handle a config flow for Philips Avent Baby Monitor."""

    VERSION = 1

    def __init__(self):
        self._email: str = ""
        self._password: str = ""
        self._country: str = ""
        self._api: PhilipsAventAPI | None = None
        self._data_center: str = ""
        self._calling_code: str = ""
        self._api_host: str = ""

    @staticmethod
    @config_entries.callback
    def async_get_options_flow(config_entry):
        return PhilipsAventOptionsFlowHandler()

    async def _async_request_mfa_code(
        self, api: PhilipsAventAPI, email: str, password: str
    ) -> None:
        """Run the pre-MFA login sequence against one data center.

        Every password submission needs its own single-use RSA token, which is
        why the token is fetched twice.
        """
        token_data = await api.get_rsa_token(email)
        encrypted = await self.hass.async_add_executor_job(
            PhilipsAventAPI.encrypt_password, password, token_data["pbKey"],
        )

        # First login attempt — expected to come back asking for MFA
        try:
            await api.login_password(email, encrypted, token_data["token"], mfa_code="")
        except TuyaAPIError as e:
            if e.code != "MFA_NEED_SEND_CODE":
                raise

        token_data2 = await api.get_rsa_token(email)
        encrypted2 = await self.hass.async_add_executor_job(
            PhilipsAventAPI.encrypt_password, password, token_data2["pbKey"],
        )
        await api.trigger_mfa(email, encrypted2, token_data2["token"])

    async def _async_begin_login(
        self, email: str, password: str, country: str | None
    ) -> None:
        """Pick the data center that holds the account, then send the MFA code.

        A Tuya session is only valid in the data center that issued it, so an
        account outside Central Europe used to fail with USER_SESSION_INVALID
        (issues #44, #58). The selected country decides the first candidate; the
        remaining data centers are tried only when Tuya's answer says the
        account is not there, never after a credential failure.
        """
        session = async_get_clientsession(self.hass)
        first_error: TuyaAPIError | None = None

        for data_center, calling_code in login_candidates(country):
            api = PhilipsAventAPI(
                session, api_url=api_url(data_center), country_code=calling_code
            )
            try:
                await self._async_request_mfa_code(api, email, password)
            except TuyaAPIError as e:
                if first_error is None:
                    first_error = e
                if is_wrong_data_center(e.code):
                    _LOGGER.debug(
                        "Tuya data center %s refused the login (%s), trying the next one",
                        data_center, e.code,
                    )
                    continue
                raise

            _LOGGER.info(
                "Using Tuya data center %s (country code %s) for this account",
                data_center, calling_code,
            )
            self._api = api
            self._data_center = data_center
            self._calling_code = calling_code
            self._api_host = api_host(data_center)
            return

        raise first_error or TuyaAPIError("UNKNOWN", "No Tuya data center accepted the login")

    async def async_step_user(self, user_input=None):
        """Step 1: Email + Password + account country."""
        errors = {}
        error_code = ""
        default_country = self._country or self.hass.config.country

        if user_input is not None:
            self._email = user_input[CONF_EMAIL]
            self._password = user_input[CONF_PASSWORD]
            self._country = user_input.get(CONF_COUNTRY, "")
            default_country = self._country or default_country

            try:
                await self._async_begin_login(
                    self._email, self._password, self._country
                )
                return await self.async_step_mfa()

            except AbortFlow:
                raise
            except TuyaAPIError as e:
                _LOGGER.error("Login failed: %s", e)
                errors["base"] = classify_login_error(e.code)
                error_code = e.code
            except Exception:
                _LOGGER.exception("Unexpected error")
                errors["base"] = "unknown"

        return self.async_show_form(
            step_id="user",
            data_schema=_credentials_schema(default_country),
            errors=errors,
            description_placeholders={"app_name": "Philips Avent Baby Monitor+", "error_code": error_code},
        )

    async def _async_complete_login(self, mfa_code: str) -> dict:
        """Finish the login with the emailed code and lock in the account hosts.

        The login response carries the account's own endpoints in its `domain`
        block, so whatever the country table guessed is corrected here with what
        Tuya actually reports.
        """
        token_data = await self._api.get_rsa_token(self._email)
        encrypted = await self.hass.async_add_executor_job(
            PhilipsAventAPI.encrypt_password, self._password, token_data["pbKey"],
        )

        result = await self._api.login_password(
            self._email, encrypted, token_data["token"], mfa_code=mfa_code
        )

        self._api.sid = result["sid"]

        hosts = hosts_from_domain(result.get("domain"))
        reported_host = hosts.get("api_host")
        if reported_host and reported_host != self._api_host:
            _LOGGER.info(
                "Tuya reports API host %s for this account (region %s); using it instead of %s",
                reported_host, hosts.get("region_code", "?"), self._api_host,
            )
        if reported_host:
            self._api_host = reported_host
            self._api.api_url = api_url_for_host(reported_host)

        return result

    def _region_data(self) -> dict[str, str]:
        """Region fields persisted in the config entry for runtime and bridge."""
        return {
            CONF_API_HOST: self._api_host or api_host(self._data_center),
            CONF_COUNTRY_CODE: self._calling_code,
            CONF_COUNTRY: self._country,
        }

    async def async_step_mfa(self, user_input=None):
        """Step 2: MFA code from email."""
        errors = {}
        error_code = ""

        if user_input is not None:
            mfa_code = user_input["mfa_code"]

            try:
                result = await self._async_complete_login(mfa_code)
                sid = result["sid"]

                user_info = await self._api.get_user_info()

                # Discover cameras while we have a live session
                cameras = []
                try:
                    discovered = await self._api.discover_cameras()
                    for cam in discovered:
                        cameras.append({
                            "id": cam.get("devId") or cam.get("deviceId"),
                            "name": cam.get("name") or cam.get("deviceName", "camera"),
                            "product_id": cam.get("productId") or cam.get("productKey") or "",
                        })
                except Exception:
                    _LOGGER.warning("Camera discovery during setup failed")

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
                        **self._region_data(),
                        "cameras": cameras,
                    },
                )

            except AbortFlow:
                raise
            except TuyaAPIError as e:
                _LOGGER.error("MFA login failed: %s", e)
                if is_wrong_data_center(e.code):
                    errors["base"] = "wrong_region"
                else:
                    errors["base"] = classify_login_error(e.code, mfa=True)
                error_code = e.code
            except Exception:
                _LOGGER.exception("Unexpected error")
                errors["base"] = "unknown"

        return self.async_show_form(
            step_id="mfa",
            data_schema=STEP_MFA_DATA_SCHEMA,
            errors=errors,
            description_placeholders={"email": self._email, "error_code": error_code},
        )

    async def async_step_reauth(self, entry_data: Mapping[str, Any]) -> config_entries.ConfigFlowResult:
        """Handle reauthentication when session expires."""
        self._email = entry_data.get(CONF_EMAIL, "")
        self._country = entry_data.get(CONF_COUNTRY, "")
        return await self.async_step_reauth_confirm()

    async def async_step_reauth_confirm(self, user_input: dict[str, Any] | None = None) -> config_entries.ConfigFlowResult:
        """Handle reauth confirmation with new credentials."""
        errors: dict[str, str] = {}
        error_code = ""
        default_country = self._country or self.hass.config.country

        if user_input is not None:
            self._email = user_input[CONF_EMAIL]
            self._password = user_input[CONF_PASSWORD]
            self._country = user_input.get(CONF_COUNTRY, "")
            default_country = self._country or default_country

            try:
                await self._async_begin_login(
                    self._email, self._password, self._country
                )
                return await self.async_step_reauth_mfa()

            except AbortFlow:
                raise
            except TuyaAPIError as e:
                _LOGGER.error("Reauth login failed: %s", e)
                errors["base"] = classify_login_error(e.code)
                error_code = e.code
            except Exception:
                _LOGGER.exception("Unexpected error during reauth")
                errors["base"] = "unknown"

        return self.async_show_form(
            step_id="reauth_confirm",
            data_schema=_credentials_schema(default_country),
            errors=errors,
            description_placeholders={"error_code": error_code},
        )

    async def async_step_reauth_mfa(self, user_input: dict[str, Any] | None = None) -> config_entries.ConfigFlowResult:
        """Handle MFA during reauthentication."""
        errors: dict[str, str] = {}
        error_code = ""

        if user_input is not None:
            mfa_code = user_input["mfa_code"]

            try:
                result = await self._async_complete_login(mfa_code)

                # Update the config entry with new credentials
                entry = self.hass.config_entries.async_get_entry(self.context["entry_id"])
                self.hass.config_entries.async_update_entry(
                    entry,
                    data={
                        **entry.data,
                        CONF_EMAIL: self._email,
                        CONF_PASSWORD: self._password,
                        CONF_SID: result["sid"],
                        CONF_ECODE: result.get("ecode", ""),
                        CONF_PARTNER: result.get("partnerIdentity", ""),
                        CONF_UID: result["uid"],
                        **self._region_data(),
                    },
                )
                await self.hass.config_entries.async_reload(entry.entry_id)
                return self.async_abort(reason="reauth_successful")

            except AbortFlow:
                raise
            except TuyaAPIError as e:
                _LOGGER.error("Reauth MFA failed: %s", e)
                if is_wrong_data_center(e.code):
                    errors["base"] = "wrong_region"
                else:
                    errors["base"] = classify_login_error(e.code, mfa=True)
                error_code = e.code
            except Exception:
                _LOGGER.exception("Unexpected error during reauth MFA")
                errors["base"] = "unknown"

        return self.async_show_form(
            step_id="reauth_mfa",
            data_schema=STEP_MFA_DATA_SCHEMA,
            errors=errors,
            description_placeholders={"email": self._email, "error_code": error_code},
        )


class PhilipsAventOptionsFlowHandler(config_entries.OptionsFlow):
    """Handle options flow for Philips Avent."""

    async def async_step_init(self, user_input: dict[str, Any] | None = None):
        """Manage the options."""
        if user_input is not None:
            return self.async_create_entry(title="", data=user_input)

        current_port = self.config_entry.options.get(
            CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT
        )
        current_host = self.config_entry.options.get(
            CONF_BRIDGE_HOST, DEFAULT_BRIDGE_HOST
        )

        return self.async_show_form(
            step_id="init",
            data_schema=vol.Schema(
                {
                    vol.Optional(CONF_BRIDGE_HOST, default=current_host): str,
                    vol.Optional(CONF_BRIDGE_PORT, default=current_port): vol.All(
                        int, vol.Range(min=1024, max=65535)
                    ),
                }
            ),
        )
