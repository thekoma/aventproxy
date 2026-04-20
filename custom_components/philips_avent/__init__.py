"""Philips Avent Baby Monitor integration for Home Assistant."""

import logging

import aiohttp

from homeassistant.config_entries import ConfigEntry
from homeassistant.const import CONF_EMAIL, CONF_PASSWORD, Platform
from homeassistant.core import HomeAssistant

from .api import PhilipsAventAPI
from .const import CONF_CAMERA_ID, CONF_ECODE, CONF_PARTNER, CONF_SID, CONF_UID, DOMAIN
from .coordinator import PhilipsAventCoordinator

_LOGGER = logging.getLogger(__name__)

PLATFORMS = [Platform.SENSOR, Platform.SWITCH, Platform.NUMBER, Platform.BUTTON]


async def async_setup_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Set up Philips Avent from a config entry."""
    session = aiohttp.ClientSession()
    api = PhilipsAventAPI(session, sid=entry.data[CONF_SID])

    # Discover cameras
    try:
        cameras = await api.discover_cameras()
    except Exception:
        _LOGGER.warning("Camera discovery failed, trying direct device list")
        cameras = []

    if not cameras:
        # Fallback: try to get all devices
        try:
            user_info = await api.get_user_info()
            # If we have a stored camera_id, use it
            if CONF_CAMERA_ID in entry.data:
                device = await api.get_device(entry.data[CONF_CAMERA_ID])
                cameras = [{"deviceId": device["devId"], "deviceName": device["name"]}]
        except Exception:
            _LOGGER.error("Could not find any cameras")
            await session.close()
            return False

    coordinators = {}
    for cam in cameras:
        cam_id = cam.get("deviceId") or cam.get("devId")
        cam_name = cam.get("deviceName") or cam.get("name", cam_id)

        coordinator = PhilipsAventCoordinator(hass, api, cam_id, cam_name)
        await coordinator.async_config_entry_first_refresh()
        coordinators[cam_id] = coordinator

    hass.data.setdefault(DOMAIN, {})[entry.entry_id] = {
        "api": api,
        "session": session,
        "coordinators": coordinators,
        "config": entry.data,
    }

    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)

    return True


async def async_unload_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Unload a config entry."""
    unload_ok = await hass.config_entries.async_unload_platforms(entry, PLATFORMS)

    if unload_ok:
        data = hass.data[DOMAIN].pop(entry.entry_id)
        await data["session"].close()

    return unload_ok
