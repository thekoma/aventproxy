"""Data update coordinator for Philips Avent."""

import logging
from datetime import timedelta
from typing import Any

from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.update_coordinator import DataUpdateCoordinator, UpdateFailed

from .api import PhilipsAventAPI, TuyaAPIError
from .const import DPS_LULLABY_CONTROL, DPS_LULLABY_STATE
from .lan import TuyaLANClient

LULLABY_STATE_MAP = {"play": "playing", "pause": "stopping", "stop": "stopping"}

_LOGGER = logging.getLogger(__name__)

POLL_FAST = timedelta(seconds=30)
POLL_SLOW = timedelta(seconds=120)


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
        merged = {**self.data, **dps}
        _LOGGER.debug("LAN push for %s: %s", self.camera_name, dps)
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
                try:
                    await self.api.set_dps(self.camera_id, dps)
                except TuyaAPIError:
                    pass
                return result
        if self.data is not None:
            optimistic = {str(k): v for k, v in dps.items()}
            lullaby_cmd = optimistic.get(DPS_LULLABY_CONTROL)
            if lullaby_cmd in LULLABY_STATE_MAP:
                optimistic[DPS_LULLABY_STATE] = LULLABY_STATE_MAP[lullaby_cmd]
            self.async_set_updated_data({**self.data, **optimistic})
        return await self.api.set_dps(self.camera_id, dps)

    async def _async_update_data(self) -> dict:
        self.update_interval = POLL_SLOW if self.lan_connected else POLL_FAST

        try:
            device = await self.api.get_device(self.camera_id)
            self.device_info = device
            try:
                rssi_data = await self.api.get_rssi(self.camera_id)
                self.rssi = rssi_data.get("value")
            except TuyaAPIError:
                pass
            api_dps = device.get("dps", {})
            if self.data:
                return {**self.data, **api_dps}
            return api_dps
        except TuyaAPIError as err:
            raise UpdateFailed(f"Error fetching data: {err}") from err
