"""Philips Avent Baby Monitor integration for Home Assistant."""
from __future__ import annotations

import json
import logging
from pathlib import Path

import aiohttp

from homeassistant.config_entries import ConfigEntry
from homeassistant.const import Platform
from homeassistant.core import HomeAssistant

from .api import PhilipsAventAPI
from .coordinator import PhilipsAventCoordinator
from .const import (
    CONF_BRIDGE_PORT, CONF_ECODE, CONF_PARTNER, CONF_SID, DEFAULT_BRIDGE_PORT, DOMAIN,
    TUYA_APP_KEY, TUYA_PACKAGE_NAME, TUYA_SIGNING_KEY,
)

_LOGGER = logging.getLogger(__name__)

PLATFORMS = [Platform.CAMERA, Platform.SENSOR, Platform.SWITCH, Platform.NUMBER, Platform.BUTTON, Platform.SELECT, Platform.BINARY_SENSOR]


async def _write_bridge_config(hass: HomeAssistant, entry: ConfigEntry, api: PhilipsAventAPI, cameras: list) -> None:
    """Write bridge config JSON for the add-on."""
    bridge_port = entry.options.get(CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT)
    bridge_config = {
        "signing_key": TUYA_SIGNING_KEY,
        "sid": entry.data[CONF_SID],
        "ecode": entry.data.get(CONF_ECODE, ""),
        "partner": entry.data.get(CONF_PARTNER, ""),
        "app_key": TUYA_APP_KEY,
        "device_id": api.device_id,
        "package_name": TUYA_PACKAGE_NAME,
        "bridge_port": bridge_port,
        "cameras": [
            {"camera_id": cam.get("deviceId", cam.get("devId")),
             "camera_name": cam.get("deviceName", cam.get("name", "camera"))}
            for cam in cameras
        ],
    }
    bridge_path = Path(hass.config.path("philips_avent_bridge.json"))
    await hass.async_add_executor_job(
        bridge_path.write_text, json.dumps(bridge_config, indent=2)
    )
    _LOGGER.info("Bridge config written to %s (port: %d)", bridge_path, bridge_port)


async def _async_options_updated(hass: HomeAssistant, entry: ConfigEntry) -> None:
    """Reload integration when options change."""
    await hass.config_entries.async_reload(entry.entry_id)


async def async_setup_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Set up Philips Avent from a config entry."""
    session = aiohttp.ClientSession()
    api = PhilipsAventAPI(session, sid=entry.data[CONF_SID])

    # Use cameras stored in config entry (discovered during config flow)
    cameras = []
    stored_cameras = entry.data.get("cameras", [])
    if stored_cameras:
        for cam in stored_cameras:
            cameras.append({
                "deviceId": cam["id"],
                "deviceName": cam["name"],
            })
        _LOGGER.info("Using %d cameras from config entry", len(cameras))
    else:
        # Fallback: re-discover via API
        try:
            cameras = await api.discover_cameras()
        except Exception:
            _LOGGER.exception("Camera discovery failed")
            cameras = []

    if not cameras:
        _LOGGER.error("No cameras found. Reconfigure the integration to re-discover.")
        await session.close()
        return False

    coordinators = {}
    for cam in cameras:
        cam_id = cam.get("deviceId") or cam.get("devId")
        cam_name = cam.get("deviceName") or cam.get("name", cam_id)
        local_key = cam.get("localKey")

        coordinator = PhilipsAventCoordinator(hass, api, cam_id, cam_name, local_key=local_key)
        await coordinator.async_config_entry_first_refresh()

        if not local_key:
            local_key = coordinator.device_info.get("localKey")
            if local_key:
                coordinator._local_key = local_key

        await coordinator.start_lan()
        coordinators[cam_id] = coordinator

    hass.data.setdefault(DOMAIN, {})[entry.entry_id] = {
        "api": api,
        "session": session,
        "coordinators": coordinators,
        "config": entry.data,
    }

    await _write_bridge_config(hass, entry, api, cameras)

    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)

    entry.async_on_unload(entry.add_update_listener(_async_options_updated))

    return True


async def async_unload_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Unload a config entry."""
    unload_ok = await hass.config_entries.async_unload_platforms(entry, PLATFORMS)

    if unload_ok:
        data = hass.data[DOMAIN].pop(entry.entry_id)
        for coordinator in data["coordinators"].values():
            await coordinator.stop_lan()
        await data["session"].close()

    return unload_ok
