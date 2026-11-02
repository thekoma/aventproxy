"""LAN credentials the bridge needs, gathered per camera."""
from __future__ import annotations

import logging
from typing import Any

_LOGGER = logging.getLogger(__name__)


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


def lan_credentials(coordinators: dict, passwords: dict[str, str]) -> dict[str, dict[str, str]]:
    """Map camera id to ``local_key``, ``p2p_password``, ``lan_ip`` and ``lan_protocol``, all strings.

    No API call, so an address-triggered rewrite works while the cloud is down.
    The key comes from the latest cloud device info, so a rotated key wins over
    the stored one. ``lan_ip`` is the last published address, never the live one,
    and ``lan_protocol`` the version published with it.
    """
    return {
        cam_id: {
            "local_key": coordinator.device_info.get("localKey") or coordinator._local_key or "",
            "p2p_password": passwords.get(cam_id) or "",
            "lan_ip": coordinator.lan_ip or "",
            "lan_protocol": coordinator.lan_protocol or "",
        }
        for cam_id, coordinator in coordinators.items()
    }
