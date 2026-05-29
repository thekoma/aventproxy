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
from .payload import build_cameras_payload

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
        "cameras": build_cameras_payload(cameras),
    }
    bridge_path = Path(hass.config.path(f"philips_avent_bridge_{entry.entry_id}.json"))
    await hass.async_add_executor_job(
        bridge_path.write_text, json.dumps(bridge_config, indent=2)
    )
    _LOGGER.info("Bridge config written to %s (port: %d)", bridge_path, bridge_port)

    legacy_path = Path(hass.config.path("philips_avent_bridge.json"))
    if await hass.async_add_executor_job(legacy_path.exists):
        await hass.async_add_executor_job(legacy_path.unlink)
        _LOGGER.info("Removed legacy bridge config %s", legacy_path)


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
                "productId": cam.get("product_id", ""),
            })
        _LOGGER.info("Using %d cameras from config entry", len(cameras))

        # Backfill productId for entries created before this field was tracked.
        # Guard on key presence in the stored entry, NOT on the in-memory value,
        # so post-fix entries with a genuinely empty productId (e.g. a device
        # that does not expose one) don't trigger a cloud call on every restart.
        if any("product_id" not in cam for cam in stored_cameras):
            patched = 0
            try:
                discovered = await api.discover_cameras()
                by_id = {(d.get("devId") or d.get("deviceId")): d for d in discovered}
                for cam in cameras:
                    if not cam.get("productId"):
                        disc = by_id.get(cam.get("deviceId"))
                        if disc:
                            new_id = disc.get("productId") or disc.get("productKey") or ""
                            if new_id:
                                cam["productId"] = new_id
                                patched += 1
                if patched:
                    updated_stored_cameras = [
                        {**stored_cam, "product_id": cam.get("productId", "")}
                        for stored_cam, cam in zip(stored_cameras, cameras)
                    ]
                    hass.config_entries.async_update_entry(
                        entry,
                        data={**entry.data, "cameras": updated_stored_cameras},
                    )
                    _LOGGER.info("Backfilled productId for %d camera(s) and persisted to config entry", patched)
                else:
                    _LOGGER.info("Backfill ran but no productId was recovered from Tuya discovery")
            except Exception:
                _LOGGER.warning(
                    "Could not backfill productId from Tuya API; SCD951 cameras may fail "
                    "to stream until HA restarts or the integration is reconfigured"
                )
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
