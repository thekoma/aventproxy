"""Pure helpers shared between the integration runtime and unit tests.

This module contains no Home Assistant or aiohttp imports so it can be
loaded by tests without dragging in the full HA stack.
"""
from __future__ import annotations

BRIDGE_CONFIG_PREFIX = "philips_avent_bridge_"
BRIDGE_CONFIG_SUFFIX = ".json"


def bridge_config_filename(entry_id: str) -> str:
    """Name of the bridge config file for a config entry."""
    return f"{BRIDGE_CONFIG_PREFIX}{entry_id}{BRIDGE_CONFIG_SUFFIX}"


def orphan_bridge_configs(filenames: list[str], valid_entry_ids: set[str]) -> list[str]:
    """Pick the bridge config files whose config entry is gone.

    Deleting and re-adding the integration mints a new entry id, so the old
    file stays in the Home Assistant config directory. The add-on then has two
    to choose from and can keep streaming the credentials and camera id of the
    entry that no longer exists (issue #52). Anything that is not named after a
    live entry is dead weight.
    """
    orphans = []
    for name in filenames:
        if not name.startswith(BRIDGE_CONFIG_PREFIX) or not name.endswith(BRIDGE_CONFIG_SUFFIX):
            continue
        entry_id = name[len(BRIDGE_CONFIG_PREFIX):-len(BRIDGE_CONFIG_SUFFIX)]
        if entry_id and entry_id not in valid_entry_ids:
            orphans.append(name)
    return orphans


DELTA_VALUE_MAX_CHARS = 300


def dps_delta(old: dict | None, new: dict | None, max_chars: int = DELTA_VALUE_MAX_CHARS) -> dict:
    """The DPS entries that appeared or changed value, for a debug log line.

    Whole-state dumps are unreadable on a monitor with 40 data points, and the
    cloud poll never said what moved. That gap is why "the app notified and Home
    Assistant showed nothing" has meant decoding a diagnostics dump by hand every
    time (issues #42, #61): a sound alert that lands in an unexpected DPS leaves
    no trace at all.

    Long values are cut to `max_chars`, which keeps a base64 alarm record
    readable without filling the log with a snapshot reference.
    """
    if not new:
        return {}
    old = old or {}

    return truncated_dps(
        {k: v for k, v in new.items() if k not in old or old[k] != v}, max_chars
    )


def truncated_dps(dps: dict | None, max_chars: int = DELTA_VALUE_MAX_CHARS) -> dict:
    """DPS entries with long values cut down, for logging a push as it arrived.

    A push is worth logging even when it repeats a value, so this keeps every key
    it was given.
    """
    if not dps:
        return {}
    return {
        key: (
            value[:max_chars] + f"...(+{len(value) - max_chars} chars)"
            if isinstance(value, str) and len(value) > max_chars
            else value
        )
        for key, value in dps.items()
    }


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
