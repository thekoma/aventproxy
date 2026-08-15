"""Diagnostics for Philips Avent Baby Monitor."""
from __future__ import annotations

from typing import Any

from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant

from .const import DOMAIN
from .redact import redact_secrets


async def async_get_config_entry_diagnostics(hass: HomeAssistant, entry: ConfigEntry) -> dict[str, Any]:
    """Return diagnostics for a config entry."""
    data = hass.data[DOMAIN][entry.entry_id]
    coordinators = data.get("coordinators", {})

    diag: dict[str, Any] = {
        "config_entry": redact_secrets(dict(entry.data)),
        "devices": {},
    }

    for cam_id, coordinator in coordinators.items():
        diag["devices"][cam_id] = {
            "name": coordinator.camera_name,
            "dps": coordinator.data,
            "lan_connected": coordinator.lan_connected,
            "update_interval": str(coordinator.update_interval),
            "rssi": coordinator.rssi,
            "device_info": redact_secrets(coordinator.device_info) if coordinator.device_info else None,
        }

    return diag
