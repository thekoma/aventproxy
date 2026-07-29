"""Pure helpers shared between the integration runtime and unit tests.

This module contains no Home Assistant or aiohttp imports so it can be
loaded by tests without dragging in the full HA stack.
"""
from __future__ import annotations


def build_bridge_config(
    *,
    signing_key: str,
    sid: str,
    ecode: str,
    partner: str,
    app_key: str,
    device_id: str,
    package_name: str,
    api_host: str,
    bridge_port: int,
    cameras: list,
) -> dict:
    """Build the bridge JSON the add-on reads.

    Mirrors ``BridgeConfig`` in ``cmd/addon/addon.go``; `api_host` carries the
    data center the account was logged into, because a Tuya session is rejected
    by any other host (issues #44, #58).
    """
    return {
        "signing_key": signing_key,
        "sid": sid,
        "ecode": ecode,
        "partner": partner,
        "app_key": app_key,
        "device_id": device_id,
        "package_name": package_name,
        "api_host": api_host,
        "bridge_port": bridge_port,
        "cameras": build_cameras_payload(cameras),
    }


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
