"""Tuya LAN protocol client for real-time DPS push updates."""

import asyncio
import logging
import time
from collections.abc import Callable
from typing import Any

import tinytuya

from homeassistant.core import HomeAssistant

_LOGGER = logging.getLogger(__name__)

SOCKET_TIMEOUT = 5
RECONNECT_DELAY = 15
SCAN_MAXRETRY = 5
HEARTBEAT_TIMEOUT = 600
PRIME_DPS = [101, 102, 106, 134, 138, 139, 140, 158, 207, 209, 237, 246]


class TuyaLANClient:
    """Persistent LAN connection to a Tuya device for real-time DPS push."""

    def __init__(
        self,
        hass: HomeAssistant,
        device_id: str,
        local_key: str,
        on_dps_update: Callable[[dict[str, Any]], None],
    ) -> None:
        self._hass = hass
        self._device_id = device_id
        self._local_key = local_key
        self._on_dps_update = on_dps_update
        self._device: tinytuya.Device | None = None
        self._ip: str | None = None
        self._task: asyncio.Task | None = None
        self._stop_event = asyncio.Event()
        self.connected: bool = False

    async def start(self) -> None:
        self._stop_event.clear()
        self._task = self._hass.async_create_background_task(
            self._run(), "tuya_lan_listener"
        )

    async def stop(self) -> None:
        self._stop_event.set()
        if self._task and not self._task.done():
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
        self._disconnect()

    def _disconnect(self) -> None:
        if self._device:
            try:
                self._device.close()
            except Exception:
                pass
            self._device = None
        self.connected = False

    async def _discover_ip(self) -> str | None:
        def _scan():
            devices = tinytuya.deviceScan(maxretry=SCAN_MAXRETRY)
            for ip, info in devices.items():
                if info.get("gwId") == self._device_id:
                    return ip
            return None

        return await self._hass.async_add_executor_job(_scan)

    def _try_direct_connect(self, ip: str) -> tinytuya.Device | None:
        """Try connecting directly to a known IP."""
        try:
            d = tinytuya.Device(self._device_id, ip, self._local_key, version=3.3)
            d.set_socketPersistent(True)
            d.set_socketTimeout(SOCKET_TIMEOUT)
            result = d.updatedps(PRIME_DPS)
            if result and isinstance(result, dict):
                return d
            d.close()
        except Exception:
            pass
        return None

    async def _connect(self) -> bool:
        # Try cached IP first (direct TCP, no broadcast needed)
        if self._ip:
            _LOGGER.debug("Trying direct LAN connection to %s at %s", self._device_id, self._ip)
            device = await self._hass.async_add_executor_job(self._try_direct_connect, self._ip)
            if device:
                self._device = device
                self.connected = True
                _LOGGER.info("LAN reconnected to %s at %s", self._device_id, self._ip)
                return True
            self._ip = None

        # Scan for device IP
        _LOGGER.debug("Scanning LAN for device %s", self._device_id)
        self._ip = await self._discover_ip()
        if not self._ip:
            _LOGGER.warning("Device %s not found on LAN", self._device_id)
            return False

        device = await self._hass.async_add_executor_job(self._try_direct_connect, self._ip)
        if device:
            self._device = device
            self.connected = True
            _LOGGER.info("LAN connected to %s at %s", self._device_id, self._ip)
            return True

        _LOGGER.debug("LAN connection failed for %s at %s", self._device_id, self._ip)
        self._ip = None
        return False

    async def _run(self) -> None:
        last_data = time.monotonic()

        while not self._stop_event.is_set():
            if not self._device:
                if not await self._connect():
                    await self._interruptible_sleep(RECONNECT_DELAY)
                    continue
                last_data = time.monotonic()

            if time.monotonic() - last_data > HEARTBEAT_TIMEOUT:
                _LOGGER.debug("LAN heartbeat timeout, reconnecting")
                self._disconnect()
                continue

            try:
                data = await self._hass.async_add_executor_job(self._device.receive)
            except Exception as ex:
                _LOGGER.debug("LAN receive error (%s), reconnecting", ex)
                self._disconnect()
                await self._interruptible_sleep(RECONNECT_DELAY)
                continue

            if not data or not isinstance(data, dict):
                continue

            last_data = time.monotonic()

            if "Error" in data or "Err" in data:
                _LOGGER.debug("LAN device error: %s, reconnecting", data.get("Error", data.get("Err")))
                self._disconnect()
                await self._interruptible_sleep(RECONNECT_DELAY)
                continue

            if data.get("dps"):
                try:
                    self._on_dps_update(data["dps"])
                except Exception:
                    _LOGGER.exception("Error in DPS update callback")

    async def set_dps(self, dps: dict) -> dict | None:
        """Send DPS command via a temporary LAN connection.

        Uses a separate short-lived socket so the persistent listener
        is never disrupted. Sends each value individually — the device
        ignores batched commands for certain codes (e.g. 201 + 202).
        """
        if not self._ip:
            return None

        def _send():
            d = tinytuya.Device(self._device_id, self._ip, self._local_key, version=3.3)
            d.set_socketTimeout(SOCKET_TIMEOUT)
            try:
                for key, value in dps.items():
                    d.set_value(key, value)
            finally:
                d.close()

        try:
            await self._hass.async_add_executor_job(_send)
            return {"success": True}
        except Exception as ex:
            _LOGGER.warning("LAN set_dps failed: %s", ex)
            return None

    async def _interruptible_sleep(self, seconds: float) -> None:
        try:
            await asyncio.wait_for(self._stop_event.wait(), timeout=seconds)
        except asyncio.TimeoutError:
            pass
