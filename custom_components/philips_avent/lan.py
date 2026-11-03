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
    PROTOCOL_VERSION_DEFAULT,
    AddressPublisher,
    baseline_dps,
    confirmed_protocol,
    data_stale,
    heartbeat_due,
    heartbeat_outcome,
    keyed_protocol,
    lan_write_delivered,
    parse_protocol_version,
    reconnect_delay,
    should_reconnect,
    version_candidates,
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
        on_dps_update: Callable[[dict[str, Any], bool], None],
        on_address_change: Callable[[str], None] | None = None,
        last_ip: str | None = None,
        last_protocol: str = "",
    ) -> None:
        self._hass = hass
        self._device_id = device_id
        self._local_key = local_key
        self._on_dps_update = on_dps_update
        self._device: tinytuya.Device | None = None
        self._ip: str | None = None
        self._address = AddressPublisher(on_address_change, last_ip, last_protocol)
        self._announced_version: float | None = None
        self._version: float | None = None
        self._task: asyncio.Task | None = None
        self._stop_event = asyncio.Event()
        self.connected: bool = False

    @property
    def ip(self) -> str | None:
        """The last published address, not the live one: `_ip` is cleared on every failed connect."""
        return self._address.last

    @property
    def protocol(self) -> str:
        """The protocol published with `ip`, such as "3.3"; "" when unknown."""
        return self._address.protocol

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

    async def _discover_device(self) -> tuple[str | None, float | None]:
        """Find the monitor on the LAN, keeping the protocol version it announces."""
        def _scan():
            devices = tinytuya.deviceScan(maxretry=SCAN_MAXRETRY)
            for ip, info in devices.items():
                if info.get("gwId") == self._device_id:
                    return ip, parse_protocol_version(info.get("version"))
            return None, None

        return await self._hass.async_add_executor_job(_scan)

    def _try_direct_connect(self, ip: str, version: float) -> tinytuya.Device | None:
        """Try connecting directly to a known IP via TCP handshake only.

        Don't send updatedps() — it crashes this firmware. DP_QUERY goes out
        once the session is up, and only on 3.4 and up; see `_query_baseline`.

        `_get_socket` answers True or a tinytuya error code, and from 3.4 up that
        covers the session-key negotiation, so the caller learns whether this
        protocol version actually works on this firmware.
        """
        try:
            d = tinytuya.Device(self._device_id, ip, self._local_key, version=version)
            d.set_socketPersistent(True)
            d.set_socketTimeout(SOCKET_TIMEOUT)
            result = d._get_socket(False)
            if result is True:
                return d
            _LOGGER.debug(
                "LAN connection to %s at protocol %s refused: %s", ip, version, result
            )
            d.close()
        except Exception as ex:  # noqa: BLE001 - tinytuya raises socket, decode and key errors here
            _LOGGER.debug("Direct LAN connection to %s at protocol %s failed: %s", ip, version, ex)
        return None

    async def _connect_at_any_version(self, ip: str) -> tinytuya.Device | None:
        """Connect to a known IP, trying the announced protocol then the default."""
        for version in version_candidates(self._version or self._announced_version):
            device = await self._hass.async_add_executor_job(
                self._try_direct_connect, ip, version
            )
            if device:
                if version != self._version:
                    _LOGGER.info(
                        "LAN session with %s speaks protocol %s", self._device_id, version
                    )
                self._version = version
                # On the event loop, like the rest of _connect. An unconfirmed fallback keeps the last pair.
                self._address.update(ip, confirmed_protocol(version, self._announced_version))
                return device
        return None

    async def _connect(self) -> bool:
        # Try cached IP first (direct TCP, no broadcast needed)
        if self._ip:
            _LOGGER.debug("Trying direct LAN connection to %s at %s", self._device_id, self._ip)
            device = await self._connect_at_any_version(self._ip)
            if device:
                self._device = device
                self.connected = True
                _LOGGER.info("LAN reconnected to %s at %s", self._device_id, self._ip)
                return True
            self._ip = None

        # Scan for device IP
        _LOGGER.debug("Scanning LAN for device %s", self._device_id)
        self._ip, announced = await self._discover_device()
        if announced is not None and announced != self._announced_version:
            _LOGGER.debug(
                "Device %s announces protocol %s", self._device_id, announced
            )
            self._announced_version = announced
        if not self._ip:
            _LOGGER.warning("Device %s not found on LAN", self._device_id)
            return False

        device = await self._connect_at_any_version(self._ip)
        if device:
            self._device = device
            self.connected = True
            _LOGGER.info("LAN connected to %s at %s", self._device_id, self._ip)
            return True

        _LOGGER.debug("LAN connection failed for %s at %s", self._device_id, self._ip)
        self._ip = None
        return False

    def _query_baseline(self) -> dict[str, Any] | None:
        """Read the monitor's whole DPS set on the session that just came up.

        A read and only a read: DP_QUERY (command 10) carries no data points, so
        it cannot change a setting. Sent only on a keyed session, where the
        monitor answers it (`keyed_protocol`); a 3.3 session leaves it
        unanswered, which would cost a socket timeout on every connect.
        """
        try:
            result = self._device.status()
        except Exception as ex:  # noqa: BLE001 - tinytuya raises socket and decode errors alike
            _LOGGER.debug("LAN baseline query failed: %s", ex)
            return None
        dps = baseline_dps(result)
        if dps is None:
            _LOGGER.debug(
                "LAN baseline query returned no state: %s",
                result.get("Error") if isinstance(result, dict) else result,
            )
        return dps

    async def _load_baseline(self) -> None:
        """Publish the full local state, so it need not wait for the cloud."""
        dps = await self._hass.async_add_executor_job(self._query_baseline)
        if dps:
            _LOGGER.debug("LAN baseline for %s carries %d keys", self._device_id, len(dps))
            self._publish(dps, baseline=True)

    def _publish(self, dps: dict[str, Any], *, baseline: bool = False) -> None:
        """Hand state to the coordinator, on the event loop.

        A baseline is the answer to the query sent when the session came up:
        state, not news, so the coordinator does not treat it as an event.
        """
        try:
            self._on_dps_update(dps, baseline)
        except Exception:
            _LOGGER.exception("Error in DPS %s callback", "baseline" if baseline else "update")

    def _send_heartbeat(self) -> tuple[bool, dict[str, Any] | None]:
        """Ping the device on the persistent socket.

        Returns whether the socket is still usable, and any state that came back
        in place of the ack (see `heartbeat_outcome`), for the caller to publish
        on the event loop.

        tinytuya does not keep persistent sockets alive by itself, so without
        this the monitor closes the connection after 30-45 seconds of silence
        (issue #62). Unlike status() and updatedps(), which this firmware either
        ignores or crashes on, a heartbeat is the plain keep-alive command.

        The response is consumed here (`nowait=False`) so a stray ack cannot turn
        up in the next receive() and look like a device error.
        """
        try:
            result = self._device.heartbeat(nowait=False)
        except Exception as ex:  # noqa: BLE001 - any failure here means the socket is unusable
            _LOGGER.debug("LAN heartbeat failed (%s)", ex)
            return False, None

        alive, pushed = heartbeat_outcome(result)
        if not alive:
            _LOGGER.debug("LAN heartbeat unreachable: %s", result.get("Error"))
        return alive, pushed

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
                # Before the receive loop, so the query never shares the socket
                # with a keep-alive.
                if keyed_protocol(self._version):
                    await self._load_baseline()
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
                        self._publish(data["dps"])

            # Keep-alive. Runs after the receive so the two never share the
            # socket at the same time.
            now = time.monotonic()
            if heartbeat_due(now, last_heartbeat):
                alive, pushed = await self._hass.async_add_executor_job(self._send_heartbeat)
                if not alive:
                    self._disconnect()
                    failures += 1
                    await self._interruptible_sleep(reconnect_delay(failures))
                    continue
                if pushed:
                    self._publish(pushed)
                last_heartbeat = now
                last_data = now
                failures = 0

    async def set_dps(self, dps: dict) -> dict | None:
        """Send DPS command via a temporary LAN connection.

        Uses a separate short-lived socket so the persistent listener
        is never disrupted. Sends each value individually, in order — the
        device ignores batched commands for certain codes (e.g. 201 + 202).

        None when the command did not reach the monitor, so the caller can
        send it through the cloud instead.
        """
        if not self._ip:
            return None

        def _send() -> dict | None:
            d = tinytuya.Device(
                self._device_id,
                self._ip,
                self._local_key,
                version=self._version or PROTOCOL_VERSION_DEFAULT,
            )
            d.set_socketTimeout(SOCKET_TIMEOUT)
            try:
                for key, value in dps.items():
                    result = d.set_value(key, value)
                    if not lan_write_delivered(result):
                        return result
                return None
            finally:
                d.close()

        try:
            refused = await self._hass.async_add_executor_job(_send)
        except Exception as ex:  # noqa: BLE001 - a failed command falls back to the cloud path
            _LOGGER.warning("LAN set_dps failed: %s", ex)
            return None
        if refused is not None:
            _LOGGER.warning("LAN command did not reach the monitor: %s", refused.get("Error"))
            return None
        return {"success": True}

    async def _interruptible_sleep(self, seconds: float) -> None:
        with contextlib.suppress(TimeoutError):
            await asyncio.wait_for(self._stop_event.wait(), timeout=seconds)
