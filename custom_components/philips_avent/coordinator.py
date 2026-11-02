"""Data update coordinator for Philips Avent."""
from __future__ import annotations

import contextlib
import logging
import time
from collections.abc import Callable
from datetime import timedelta
from typing import Any

import aiohttp
from homeassistant.core import HomeAssistant, callback
from homeassistant.exceptions import ConfigEntryAuthFailed
from homeassistant.helpers.event import async_call_later
from homeassistant.helpers.update_coordinator import DataUpdateCoordinator, UpdateFailed
from homeassistant.util.dt import now as dt_now
from homeassistant.util.dt import utcnow

from . import senseiq
from .api import PhilipsAventAPI, TuyaAPIError
from .const import (
    DPS_ALARM_RECORD,
    DPS_LULLABY_CONTROL,
    DPS_LULLABY_STATE,
    DPS_SENSEIQ_STATUS,
    mirror_dps_to_cloud,
)
from .events import LULLABY_SETTLE_SECONDS, lullaby_state_settled, poll_should_stay_fast
from .lan import TuyaLANClient
from .payload import dps_delta, truncated_dps

LULLABY_STATE_MAP = {"play": "playing", "pause": "stopping", "stop": "stopping"}

_LOGGER = logging.getLogger(__name__)

POLL_FAST = timedelta(seconds=30)
POLL_SLOW = timedelta(seconds=120)

# RSSI moves slowly and costs a second API call per poll, so it is refreshed on
# its own schedule rather than on every tick of a fast poll.
RSSI_INTERVAL = timedelta(minutes=5)

# The SenseIQ daily sleep summary is a cloud aggregate that changes slowly, so it
# is fetched on its own schedule rather than on every DPS poll.
SLEEP_DAY_INTERVAL = timedelta(minutes=15)


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
        self._pending_lullaby: str | None = None
        self._pending_lullaby_since: float | None = None
        self._lullaby_unsub = None
        # Monotonic time of the last DPS 3 (SenseIQ status) LAN push, and the
        # timer that re-evaluates entities once it would go stale.
        self._senseiq_pushed_at: float | None = None
        self._senseiq_stale_unsub = None
        self._rssi_refreshed_at = None
        # SenseIQ nightly sleep summary (cloud aggregate), decoded in senseiq.py.
        self.sleep_day: dict | None = None
        self._sleep_day_refreshed_at = None

    async def start_lan(
        self,
        on_address_change: Callable[[str], None] | None = None,
        last_ip: str | None = None,
        last_protocol: str = "",
    ) -> None:
        if not self._local_key:
            return
        self._lan_client = TuyaLANClient(
            self.hass,
            self.camera_id,
            self._local_key,
            self._on_lan_dps_update,
            on_address_change,
            last_ip,
            last_protocol,
        )
        await self._lan_client.start()

    async def stop_lan(self) -> None:
        self._cancel_pending_lullaby()
        if self._senseiq_stale_unsub:
            self._senseiq_stale_unsub()
            self._senseiq_stale_unsub = None
        if self._lan_client:
            await self._lan_client.stop()
            self._lan_client = None

    @property
    def lan_ip(self) -> str | None:
        return self._lan_client.ip if self._lan_client else None

    @property
    def lan_protocol(self) -> str:
        return self._lan_client.protocol if self._lan_client else ""

    @property
    def lan_connected(self) -> bool:
        return self._lan_client is not None and self._lan_client.connected

    @property
    def senseiq_status_stale(self) -> bool:
        """Whether the SenseIQ live status has stopped arriving; see senseiq.status_is_stale."""
        return senseiq.status_is_stale(self.lan_connected, self._senseiq_pushed_at, time.monotonic())

    @callback
    def _note_senseiq_push(self) -> None:
        """Record a DPS 3 push and re-check the entities once it would go stale.

        Without the timer nothing would re-evaluate Baby Detected after the
        pushes stop, until some unrelated update or the next poll arrived.
        """
        self._senseiq_pushed_at = time.monotonic()
        if self._senseiq_stale_unsub:
            self._senseiq_stale_unsub()
        self._senseiq_stale_unsub = async_call_later(
            self.hass, senseiq.STATUS_STALE_AFTER_SECONDS + 1, self._senseiq_went_stale
        )

    @callback
    def _senseiq_went_stale(self, _now=None) -> None:
        self._senseiq_stale_unsub = None
        _LOGGER.debug(
            "SenseIQ status for %s went stale after %ss without a push",
            self.camera_name, senseiq.STATUS_STALE_AFTER_SECONDS,
        )
        self.async_update_listeners()

    @callback
    def _on_lan_dps_update(self, dps: dict[str, Any]) -> None:
        if self.data is None:
            return
        self.last_lan_dps = dict(dps)
        self.lan_update_sequence += 1
        _LOGGER.debug(
            "LAN push for %s: %s", self.camera_name, truncated_dps(dps)
        )

        if DPS_SENSEIQ_STATUS in dps:
            self._note_senseiq_push()
        dps = self._hold_lullaby_state(dps)
        merged = {**self.data, **dps}
        self.async_set_updated_data(merged)

    @callback
    def _hold_lullaby_state(self, dps: dict[str, Any]) -> dict[str, Any]:
        """Keep a lullaby state out of entity state until it stands still.

        The camera re-announces `stopping` and then `playing` within a third of a
        second when a viewing session ends, which reached the Lullaby Playing
        sensor as a flicker (issue #72). Holding the value lets such a pair
        cancel itself.
        """
        if DPS_LULLABY_STATE not in dps:
            return dps

        value = dps[DPS_LULLABY_STATE]
        rest = {k: v for k, v in dps.items() if k != DPS_LULLABY_STATE}

        if value == (self.data or {}).get(DPS_LULLABY_STATE):
            # Already the state on show; nothing to hold or cancel.
            self._cancel_pending_lullaby()
            return rest

        self._pending_lullaby = value
        self._pending_lullaby_since = time.monotonic()
        if self._lullaby_unsub:
            self._lullaby_unsub()
        self._lullaby_unsub = async_call_later(
            self.hass, LULLABY_SETTLE_SECONDS, self._commit_lullaby_state
        )
        _LOGGER.debug(
            "Holding lullaby state %r for %s until it settles",
            value, self.camera_name,
        )
        return rest

    @callback
    def _cancel_pending_lullaby(self) -> None:
        if self._lullaby_unsub:
            self._lullaby_unsub()
            self._lullaby_unsub = None
        self._pending_lullaby = None
        self._pending_lullaby_since = None

    @callback
    def _commit_lullaby_state(self, _now=None) -> None:
        """Apply a held lullaby state once it has stood still."""
        self._lullaby_unsub = None
        if not lullaby_state_settled(
            self._pending_lullaby, self._pending_lullaby_since, time.monotonic()
        ):
            return
        value = self._pending_lullaby
        self._pending_lullaby = None
        self._pending_lullaby_since = None
        if self.data is None or value is None:
            return
        _LOGGER.debug("Lullaby state settled at %r for %s", value, self.camera_name)
        self.async_set_updated_data({**self.data, DPS_LULLABY_STATE: value})

    async def set_dps(self, dps: dict) -> dict:
        """Send DPS command via LAN for instant response, plus REST for cloud sync.

        Relative lullaby steps skip the REST mirror after a LAN success; see
        `mirror_dps_to_cloud`.
        """
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
                if mirror_dps_to_cloud(dps):
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

    async def _refresh_sleep_day(self) -> None:
        """Fetch and decode the SenseIQ nightly sleep summary, throttled.

        Self-contained: a failure here (e.g. a monitor without SenseIQ, or the
        cloud endpoint refusing the call) never disturbs the DPS poll.
        """
        now = utcnow()
        if (
            self._sleep_day_refreshed_at
            and now - self._sleep_day_refreshed_at < SLEEP_DAY_INTERVAL
        ):
            return
        # Stamped before the request, so a server that keeps refusing the call
        # is asked again after SLEEP_DAY_INTERVAL rather than on every poll.
        self._sleep_day_refreshed_at = now
        date = dt_now().strftime("%Y%m%d")
        try:
            result = await self.api.get_sleep_day(self.camera_id, date)
        except (TuyaAPIError, aiohttp.ClientError, TimeoutError, ValueError) as err:
            _LOGGER.warning(
                "SenseIQ sleep-day fetch failed for %s: %s", self.camera_name, err
            )
            return
        self.sleep_day = senseiq.decode_sleep_day(result, date)
        _LOGGER.debug(
            "SenseIQ sleep-day for %s (%s): %s",
            self.camera_name, date, self.sleep_day,
        )

    @property
    def alerts_need_the_cloud(self) -> bool:
        """Whether alerts on this monitor are only visible on the cloud poll.

        The SCD951 and SCD953 family reports alarms in DPS 212, and whether that
        key arrives over the LAN depends on the negotiated protocol version: it
        never did on 3.3, while on 3.5 an owner measured a motion record pushed
        and the sensor firing in 1.3 seconds (#61, rc9). Sound on the same
        monitor still arrived through the poll. DPS 212 is also a single slot
        holding the newest alarm rather than a queue, so a second alert
        overwrites the first and whatever the poll misses is gone. The fast poll
        therefore stays for these models as the floor, not as the mechanism.
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
            # Only a SenseIQ monitor has a sleep summary; asking every other
            # model would cost a cloud call per poll for nothing.
            if DPS_SENSEIQ_STATUS in api_dps or DPS_SENSEIQ_STATUS in (self.data or {}):
                await self._refresh_sleep_day()
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
