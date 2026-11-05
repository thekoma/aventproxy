"""Actions for Philips Avent Baby Monitor."""
from __future__ import annotations

from datetime import date

import aiohttp
import voluptuous as vol
from homeassistant.core import HomeAssistant, ServiceCall, ServiceResponse, SupportsResponse
from homeassistant.exceptions import HomeAssistantError, ServiceValidationError
from homeassistant.helpers import config_validation as cv
from homeassistant.helpers import device_registry as dr
from homeassistant.util import dt as dt_util

from . import senseiq
from .api import TuyaAPIError
from .const import DOMAIN

SERVICE_GET_SLEEP_HISTORY = "get_sleep_history"

# The range is clipped to the cloud's 30 days anyway; this only rejects nonsense.
MAX_HISTORY_DAYS = 400

GET_SLEEP_HISTORY_SCHEMA = vol.Schema(
    {
        vol.Required("device_id"): cv.string,
        vol.Required("start_date"): cv.date,
        vol.Optional("end_date"): cv.date,
    }
)


def _coordinator_for_device(hass: HomeAssistant, device_id: str):
    """The coordinator of the monitor behind a device registry id."""
    device = dr.async_get(hass).async_get(device_id)
    cam_ids = {ident for domain, ident in (device.identifiers if device else ()) if domain == DOMAIN}
    for data in hass.data.get(DOMAIN, {}).values():
        coordinators = data.get("coordinators", {}) if isinstance(data, dict) else {}
        for cam_id in cam_ids:
            if cam_id in coordinators:
                return coordinators[cam_id]
    raise ServiceValidationError(f"{device_id} is not a Philips Avent monitor that is set up")


def check_range(start: date, end: date) -> None:
    """Reject a range the action will not fetch."""
    if end < start:
        raise ServiceValidationError("end_date is before start_date")
    if (end - start).days + 1 > MAX_HISTORY_DAYS:
        raise ServiceValidationError(f"At most {MAX_HISTORY_DAYS} days per call")


async def async_setup_services(hass: HomeAssistant) -> None:
    """Register the actions once, for every config entry."""
    if hass.services.has_service(DOMAIN, SERVICE_GET_SLEEP_HISTORY):
        return

    async def get_sleep_history(call: ServiceCall) -> ServiceResponse:
        coordinator = _coordinator_for_device(hass, call.data["device_id"])
        today = dt_util.now().date()
        requested: date = call.data["start_date"]
        end: date = min(call.data.get("end_date") or today, today)
        check_range(requested, end)
        start = senseiq.sleep_history_start(requested, today)
        days: dict[int, dict] = {}
        for begin, finish in senseiq.sleep_history_windows(start, end):
            try:
                result = await coordinator.api.get_sleep_days(coordinator.camera_id, begin, finish)
            except (TuyaAPIError, aiohttp.ClientError, TimeoutError) as err:
                raise HomeAssistantError(f"Sleep history for {begin}..{finish} failed: {err}") from err
            for day in senseiq.decode_sleep_days(result):
                days[day["date"]] = day
        return {
            "available_from": start.isoformat(),
            "days": [days[key] for key in sorted(days)],
        }

    hass.services.async_register(
        DOMAIN,
        SERVICE_GET_SLEEP_HISTORY,
        get_sleep_history,
        schema=GET_SLEEP_HISTORY_SCHEMA,
        supports_response=SupportsResponse.ONLY,
    )
