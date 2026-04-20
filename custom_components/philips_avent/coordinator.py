"""Data update coordinator for Philips Avent."""

import logging
from datetime import timedelta

from homeassistant.core import HomeAssistant
from homeassistant.helpers.update_coordinator import DataUpdateCoordinator, UpdateFailed

from .api import PhilipsAventAPI, TuyaAPIError

_LOGGER = logging.getLogger(__name__)

SCAN_INTERVAL = timedelta(seconds=30)


class PhilipsAventCoordinator(DataUpdateCoordinator):
    """Polls camera DPS values."""

    def __init__(
        self,
        hass: HomeAssistant,
        api: PhilipsAventAPI,
        camera_id: str,
        camera_name: str,
    ):
        super().__init__(
            hass,
            _LOGGER,
            name=f"Philips Avent {camera_name}",
            update_interval=SCAN_INTERVAL,
        )
        self.api = api
        self.camera_id = camera_id
        self.camera_name = camera_name
        self.device_info: dict = {}

    async def _async_update_data(self) -> dict:
        try:
            device = await self.api.get_device(self.camera_id)
            self.device_info = device
            return device.get("dps", {})
        except TuyaAPIError as err:
            raise UpdateFailed(f"Error fetching data: {err}") from err
