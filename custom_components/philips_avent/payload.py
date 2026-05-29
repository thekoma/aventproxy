"""Pure helpers shared between the integration runtime and unit tests.

This module contains no Home Assistant or aiohttp imports so it can be
loaded by tests without dragging in the full HA stack.
"""
from __future__ import annotations


def build_cameras_payload(cameras: list) -> list:
    """Build the canonical bridge-JSON cameras list.

    Accepts any of the dict shapes we may hold:
    - stored entry camera: ``{"id", "name", "product_id"}``
    - raw Tuya discovery dict: ``{"devId"|"deviceId", "name"|"deviceName", "productId"|"productKey"}``
    - in-memory shape from async_setup_entry: ``{"deviceId", "deviceName", "productId"}``

    Returns a list of ``{"camera_id", "camera_name", "product_id"}`` dicts —
    the contract consumed by the Go bridge in ``cmd/addon/addon.go``.
    """
    return [
        {
            "camera_id": cam.get("deviceId") or cam.get("devId") or cam.get("id", ""),
            "camera_name": cam.get("deviceName") or cam.get("name", "camera"),
            "product_id": (
                cam.get("productId")
                or cam.get("product_id")
                or cam.get("productKey")
                or ""
            ),
        }
        for cam in cameras
    ]
