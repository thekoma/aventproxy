"""Data update coordinator for Philips Avent."""
from __future__ import annotations

import contextlib
import logging
from datetime import timedelta
from typing import Any

from homeassistant.core import HomeAssistant, callback
from homeassistant.exceptions import ConfigEntryAuthFailed
from homeassistant.helpers.update_coordinator import DataUpdateCoordinator, UpdateFailed
from homeassistant.util.dt import utcnow

from .api import PhilipsAventAPI, TuyaAPIError
from .const import DPS_ALARM_RECORD, DPS_LULLABY_CONTROL, DPS_LULLABY_STATE
from .events import poll_should_stay_fast
from .lan import TuyaLANClient
from .payload import dps_delta, truncated_dps

LULLABY_STATE_MAP = {"play": "playing", "pause": "stopping", "stop": "stopping"}

_LOGGER = logging.getLogger(__name__)

POLL_FAST = timedelta(seconds=30)
POLL_SLOW = timedelta(seconds=120)

# RSSI moves slowly and costs a second API call per poll, so it is refreshed on
# its own schedule rather than on every tick of a fast poll.
RSSI_INTERVAL = timedelta(minutes=5)


class PhilipsAventCoordinator(DataUpdateCoordinator):
    """Polls camera DPS values, with optional LAN push for real-time updates."""

    def __init__(
        self,
        hass: HomeAssistant,
        api: PhilipsAventAPI,
        camera_id: str,
        camera_name: str,
        local_key: str | None = None,
    ):
        super().__init__(
            hass,
            _LOGGER,
            name=f"Philips Avent {camera_name}",
            update_interval=POLL_FAST,
        )
        self.api = api
        self.camera_id = camera_id
        self.camera_name = camera_name
        self.device_info: dict = {}
        self.rssi: int | None = None
        self._local_key = local_key
        self._lan_client: TuyaLANClient | None = None
        self.last_lan_dps: dict[str, Any] = {}
        self.lan_update_sequence = 0
        self._rssi_refreshed_at = None

    async def start_lan(self) -> None:
        if not self._local_key:
            return
        self._lan_client = TuyaLANClient(
            self.hass,
            self.camera_id,
            self._local_key,
            self._on_lan_dps_update,
        )
        await self._lan_client.start()

    async def stop_lan(self) -> None:
        if self._lan_client:
            await self._lan_client.stop()
            self._lan_client = None

    @property
    def lan_connected(self) -> bool:
        return self._lan_client is not None and self._lan_client.connected

    @callback
    def _on_lan_dps_update(self, dps: dict[str, Any]) -> None:
        if self.data is None:
            return
        self.last_lan_dps = dict(dps)
        self.lan_update_sequence += 1
        merged = {**self.data, **dps}
        _LOGGER.debug(
            "LAN push for %s: %s", self.camera_name, truncated_dps(dps)
        )
        self.async_set_updated_data(merged)

    async def set_dps(self, dps: dict) -> dict:
        """Send DPS command via LAN for instant response, plus REST for cloud sync."""
        if self._lan_client and self._lan_client.connected:
            result = await self._lan_client.set_dps(dps)
            if result:
                _LOGGER.debug("DPS sent via LAN for %s: %s", self.camera_name, dps)
                if self.data is not None:
                    optimistic = {str(k): v for k, v in dps.items()}
                    lullaby_cmd = optimistic.get(DPS_LULLABY_CONTROL)
                    if lullaby_cmd in LULLABY_STATE_MAP:
                        optimistic[DPS_LULLABY_STATE] = LULLABY_STATE_MAP[lullaby_cmd]
                    self.async_set_updated_data({**self.data, **optimistic})
                with contextlib.suppress(TuyaAPIError):
                    await self.api.set_dps(self.camera_id, dps)
                return result
        if self.data is not None:
            optimistic = {str(k): v for k, v in dps.items()}
            lullaby_cmd = optimistic.get(DPS_LULLABY_CONTROL)
            if lullaby_cmd in LULLABY_STATE_MAP:
                optimistic[DPS_LULLABY_STATE] = LULLABY_STATE_MAP[lullaby_cmd]
            self.async_set_updated_data({**self.data, **optimistic})
        return await self.api.set_dps(self.camera_id, dps)

    async def _refresh_rssi(self) -> None:
        """Refresh the WiFi signal, at most once per RSSI_INTERVAL."""
        now = utcnow()
        if self._rssi_refreshed_at and now - self._rssi_refreshed_at < RSSI_INTERVAL:
            return
        self._rssi_refreshed_at = now
        try:
            rssi_data = await self.api.get_rssi(self.camera_id)
        except TuyaAPIError:
            return
        self.rssi = rssi_data.get("value")

    @property
    def alerts_need_the_cloud(self) -> bool:
        """Whether alerts on this monitor are only visible on the cloud poll.

        The SCD951 and SCD953 family reports alarms in DPS 212 and never pushes
        that key over the LAN, confirmed on #61 by a test where the sound alert
        appeared on the next cloud poll with no LAN push at all. DPS 212 is also
        a single slot holding the newest alarm rather than a queue, so a second
        alert overwrites the first: whatever the poll misses is gone. A LAN
        connection is therefore no reason to slow the poll down on these models.
        """
        return DPS_ALARM_RECORD in (self.data or {})

    async def _async_update_data(self) -> dict:
        fast = poll_should_stay_fast(self.lan_connected, self.alerts_need_the_cloud)
        self.update_interval = POLL_FAST if fast else POLL_SLOW

        try:
            device = await self.api.get_device(self.camera_id)
            self.device_info = device
            await self._refresh_rssi()
            api_dps = device.get("dps", {})
            changed = dps_delta(self.data, api_dps)
            if changed:
                _LOGGER.debug("Cloud poll changed DPS for %s: %s", self.camera_name, changed)
            if self.data:
                return {**self.data, **api_dps}
            return api_dps
        except TuyaAPIError as err:
            if "SID_INVALID" in str(err) or "USER_SESSION_LOSS" in str(err):
                raise ConfigEntryAuthFailed(f"Authentication failed: {err}") from err
            raise UpdateFailed(f"Error fetching data: {err}") from err
