"""Tuya LAN protocol client for real-time DPS push updates."""
from __future__ import annotations

import asyncio
import contextlib
import logging
import time
from collections.abc import Callable
from typing import Any

import tinytuya
from homeassistant.core import HomeAssistant

from .lan_policy import (
    data_stale,
    heartbeat_due,
    is_connection_error,
    reconnect_delay,
    should_reconnect,
)

_LOGGER = logging.getLogger(__name__)

SOCKET_TIMEOUT = 5
SCAN_MAXRETRY = 5


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
            with contextlib.suppress(asyncio.CancelledError):
                await self._task
        self._disconnect()

    def _disconnect(self) -> None:
        if self._device:
            try:
                self._device.close()
            except Exception as ex:  # noqa: BLE001 - teardown must not raise, whatever tinytuya throws
                _LOGGER.debug("Ignoring error while closing the LAN socket: %s", ex)
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
        """Try connecting directly to a known IP via TCP handshake only.

        Don't send status() or updatedps() — this IPC device doesn't
        respond to DP_QUERY and updatedps() crashes the firmware.
        """
        try:
            d = tinytuya.Device(self._device_id, ip, self._local_key, version=3.3)
            d.set_socketPersistent(True)
            d.set_socketTimeout(SOCKET_TIMEOUT)
            if d._get_socket(False) is not None:
                return d
            d.close()
        except Exception as ex:  # noqa: BLE001 - tinytuya raises socket, decode and key errors here
            _LOGGER.debug("Direct LAN connection to %s failed: %s", ip, ex)
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

    def _send_heartbeat(self) -> bool:
        """Ping the device on the persistent socket. Returns False if it is gone.

        tinytuya does not keep persistent sockets alive by itself, so without
        this the monitor closes the connection after 30-45 seconds of silence
        (issue #62). Unlike status() and updatedps(), which this firmware either
        ignores or crashes on, a heartbeat is the plain keep-alive command.

        The response is consumed here (`nowait=False`) so a stray ack cannot turn
        up in the next receive() and look like a device error. Only a connection
        error counts as failure: an ack we cannot parse still proves the socket
        is alive.
        """
        try:
            result = self._device.heartbeat(nowait=False)
        except Exception as ex:  # noqa: BLE001 - any failure here means the socket is unusable
            _LOGGER.debug("LAN heartbeat failed (%s)", ex)
            return False

        if isinstance(result, dict) and is_connection_error(result.get("Err")):
            _LOGGER.debug("LAN heartbeat unreachable: %s", result.get("Error"))
            return False
        return True

    async def _run(self) -> None:
        last_data = time.monotonic()
        last_heartbeat = last_data
        failures = 0
        payload_errors = 0

        while not self._stop_event.is_set():
            if not self._device:
                if not await self._connect():
                    failures += 1
                    await self._interruptible_sleep(reconnect_delay(failures))
                    continue
                last_data = time.monotonic()
                last_heartbeat = last_data
                payload_errors = 0

            if data_stale(time.monotonic(), last_data):
                _LOGGER.debug("No LAN data within the timeout, reconnecting")
                self._disconnect()
                continue

            try:
                data = await self._hass.async_add_executor_job(self._device.receive)
            except Exception as ex:  # noqa: BLE001 - tinytuya raises socket and decode errors alike
                _LOGGER.debug("LAN receive error (%s), reconnecting", ex)
                self._disconnect()
                failures += 1
                await self._interruptible_sleep(reconnect_delay(failures))
                continue

            if data and isinstance(data, dict):
                last_data = time.monotonic()

                if "Error" in data or "Err" in data:
                    payload_errors += 1
                    err_code = data.get("Err")
                    if should_reconnect(err_code, payload_errors):
                        _LOGGER.debug(
                            "LAN device error: %s, reconnecting",
                            data.get("Error", err_code),
                        )
                        self._disconnect()
                        failures += 1
                        await self._interruptible_sleep(reconnect_delay(failures))
                        continue
                    _LOGGER.debug(
                        "Ignoring LAN frame we could not read (%s), connection still up",
                        data.get("Error", err_code),
                    )
                else:
                    # A real exchange: the connection is healthy.
                    failures = 0
                    payload_errors = 0

                    if data.get("dps"):
                        try:
                            self._on_dps_update(data["dps"])
                        except Exception:
                            _LOGGER.exception("Error in DPS update callback")

            # Keep-alive. Runs after the receive so the two never share the
            # socket at the same time.
            now = time.monotonic()
            if heartbeat_due(now, last_heartbeat):
                if not await self._hass.async_add_executor_job(self._send_heartbeat):
                    self._disconnect()
                    failures += 1
                    await self._interruptible_sleep(reconnect_delay(failures))
                    continue
                last_heartbeat = now
                last_data = now
                failures = 0

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
        except Exception as ex:  # noqa: BLE001 - a failed command falls back to the cloud path
            _LOGGER.warning("LAN set_dps failed: %s", ex)
            return None

    async def _interruptible_sleep(self, seconds: float) -> None:
        with contextlib.suppress(TimeoutError):
            await asyncio.wait_for(self._stop_event.wait(), timeout=seconds)
