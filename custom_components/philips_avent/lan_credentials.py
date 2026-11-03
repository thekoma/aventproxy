"""LAN credentials the bridge needs, gathered per camera."""
from __future__ import annotations

import asyncio
import logging
from typing import Any

_LOGGER = logging.getLogger(__name__)

# Bound on fetching a missing P2P password again, so a rewrite during a cloud
# outage still writes the new address promptly.
REFETCH_TIMEOUT_SECONDS = 15.0


async def fetch_p2p_passwords(api: Any, camera_ids: list[str], previous: dict[str, str]) -> dict[str, str]:
    """The P2P password per camera, fetched once per setup.

    Only the RTC config carries it. A failed call keeps the previous value (from
    the existing bridge JSON) instead of blanking it, so a cloud outage does not
    turn LAN streaming off. Never None.
    """
    out: dict[str, str] = {}
    for cam_id in camera_ids:
        try:
            out[cam_id] = (await api.get_rtc_config(cam_id)).get("password") or previous.get(cam_id, "")
        except Exception as err:  # noqa: BLE001 - no RTC config only means no fresh password for this camera
            _LOGGER.warning("No RTC config for %s, keeping the previous password if any: %s", cam_id, err)
            out[cam_id] = previous.get(cam_id, "")
    return out


async def fetch_missing_passwords(
    api: Any, camera_ids: list[str], passwords: dict[str, str], timeout: float = REFETCH_TIMEOUT_SECONDS
) -> dict[str, str]:
    """Fetch again the P2P passwords still missing; only the ones that came back.

    A first setup while the RTC config call fails has no earlier file to keep a
    password from, so LAN streaming stayed off for that whole run. A later
    rewrite calls this. No call at all when nothing is missing.
    """
    missing = [cam_id for cam_id in camera_ids if not passwords.get(cam_id)]
    if not missing:
        return {}
    try:
        async with asyncio.timeout(timeout):
            fetched = await fetch_p2p_passwords(api, missing, {})
    except TimeoutError:
        _LOGGER.warning("Timed out fetching the P2P password again for %s", ", ".join(missing))
        return {}
    return {cam_id: password for cam_id, password in fetched.items() if password}


def lan_credentials(coordinators: dict, passwords: dict[str, str]) -> dict[str, dict[str, str]]:
    """Map camera id to ``local_key``, ``p2p_password``, ``lan_ip`` and ``lan_protocol``, all strings.

    No API call, so an address-triggered rewrite works while the cloud is down.
    The key comes from the latest cloud device info, so a rotated key wins over
    the stored one. ``lan_ip`` is the last published address, never the live one,
    and ``lan_protocol`` the version published with it.
    """
    return {
        cam_id: {
            "local_key": coordinator.device_info.get("localKey") or coordinator.local_key or "",
            "p2p_password": passwords.get(cam_id) or "",
            "lan_ip": coordinator.lan_ip or "",
            "lan_protocol": coordinator.lan_protocol or "",
        }
        for cam_id, coordinator in coordinators.items()
    }
