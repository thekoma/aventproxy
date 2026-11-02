"""LAN credentials the bridge needs, gathered per camera."""
from __future__ import annotations

import logging
from typing import Any

_LOGGER = logging.getLogger(__name__)


async def lan_credentials(api: Any, coordinators: dict) -> dict[str, dict[str, str]]:
    """Map camera id to ``local_key``, ``p2p_password`` and ``lan_ip``, all strings.

    The key comes from the latest cloud device info, so a rotated key wins over the
    stored one. The password only exists in the RTC config. A failed call leaves it
    empty and that camera streams through the cloud. Values are never None: the
    bridge JSON would otherwise carry ``null``.
    """
    out: dict[str, dict[str, str]] = {}
    for cam_id, coordinator in coordinators.items():
        password = ""
        try:
            password = (await api.get_rtc_config(cam_id)).get("password") or ""
        except Exception as err:  # noqa: BLE001 - no RTC config only means no LAN path for this camera
            _LOGGER.warning("No RTC config for %s, it will stream through the cloud: %s", cam_id, err)
        out[cam_id] = {
            "local_key": coordinator.device_info.get("localKey") or coordinator._local_key or "",
            "p2p_password": password,
            "lan_ip": coordinator.lan_ip or "",
        }
    return out
