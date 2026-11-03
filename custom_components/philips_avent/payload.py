"""Pure helpers shared between the integration runtime and unit tests.

This module contains no Home Assistant or aiohttp imports so it can be
loaded by tests without dragging in the full HA stack.
"""
from __future__ import annotations

import contextlib
import json
import os
import tempfile

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
    talkback: bool = False,
    uid: str = "",
    lan_streaming: bool = True,
    lan: dict[str, dict] | None = None,
) -> dict:
    """Build the bridge JSON the add-on reads.

    Mirrors ``BridgeConfig`` in ``cmd/addon/addon.go``. `api_host` carries the
    data center the account was logged into, because a Tuya session is rejected
    by any other host (issues #44, #58). `talkback` asks the camera for two-way
    audio, which the monitor answers by stopping a playing lullaby, so it
    defaults to off (issue #72).
    `uid` and `lan_streaming` steer the LAN path; `lan` maps a camera id to its
    `local_key`, `p2p_password`, `lan_ip` and `lan_protocol`.

    With LAN streaming off the bridge never uses the local key or the P2P
    password, so they stay out of a file that every backup copies. The address
    and protocol stay: they are no credentials, and the next setup seeds the LAN
    client from them, which keeps the file the same across restarts.
    """
    if not lan_streaming:
        lan = {
            camera_id: {**values, "local_key": "", "p2p_password": ""}
            for camera_id, values in (lan or {}).items()
        }
    return {
        "signing_key": signing_key,
        "sid": sid,
        "ecode": ecode,
        "partner": partner,
        "app_key": app_key,
        "device_id": device_id,
        "package_name": package_name,
        "api_host": api_host,
        "talkback": talkback,
        "uid": uid,
        "lan_streaming": lan_streaming,
        "bridge_port": bridge_port,
        "cameras": build_cameras_payload(cameras, lan),
    }


def build_cameras_payload(cameras: list, lan: dict[str, dict] | None = None) -> list:
    """Build the canonical bridge-JSON cameras list.

    Accepts any of the dict shapes we may hold:
    - stored entry camera: ``{"id", "name", "product_id"}``
    - raw Tuya discovery dict: ``{"devId"|"deviceId", "name"|"deviceName", "productId"|"productKey"}``
    - in-memory shape from async_setup_entry: ``{"deviceId", "deviceName", "productId"}``

    Returns a list of ``{"camera_id", "camera_name", "product_id", "local_key",
    "p2p_password", "lan_ip", "lan_protocol"}`` dicts — the contract consumed by
    the Go bridge in ``cmd/addon/addon.go``. The last four come from
    ``lan[camera_id]`` and are empty strings when unknown.
    """
    lan = lan or {}
    return [
        {
            "camera_id": (camera_id := cam.get("deviceId") or cam.get("devId") or cam.get("id", "")),
            "camera_name": cam.get("deviceName") or cam.get("name", "camera"),
            "product_id": (
                cam.get("productId")
                or cam.get("product_id")
                or cam.get("productKey")
                or ""
            ),
            "local_key": lan.get(camera_id, {}).get("local_key", ""),
            "p2p_password": lan.get(camera_id, {}).get("p2p_password", ""),
            "lan_ip": lan.get(camera_id, {}).get("lan_ip", ""),
            "lan_protocol": lan.get(camera_id, {}).get("lan_protocol", ""),
        }
        for cam in cameras
    ]


BRIDGE_CONFIG_MODE = 0o600


def previous_lan_values(text: str) -> dict[str, dict[str, str]]:
    """Per camera ``lan_ip``, ``lan_protocol`` and ``p2p_password`` from an earlier bridge JSON.

    Missing or corrupt text gives an empty result. Used to keep the file
    byte-identical across restarts and to keep a password when the cloud call
    that refreshes it fails.
    """
    try:
        cameras = json.loads(text).get("cameras", [])
        return {
            cam["camera_id"]: {
                "lan_ip": str(cam.get("lan_ip") or ""),
                "lan_protocol": str(cam.get("lan_protocol") or ""),
                "p2p_password": str(cam.get("p2p_password") or ""),
            }
            for cam in cameras
        }
    except (ValueError, TypeError, KeyError, AttributeError):
        return {}


def write_bridge_config_file(path, config: dict) -> None:
    """Write the bridge config, readable only by the owner.

    The file carries a live Tuya session — sid, ecode, partner, device id —
    and it lands in /config, which the Samba, File Editor and Terminal add-ons
    all mount and which every backup copies. ``Path.write_text`` left it at
    the process umask, so in practice 0644.

    The content goes to a temporary file in the same directory, owner-only
    before anything is written to it, which then replaces the old file in one
    step. Truncating in place let run.sh's md5 poll, or the seed read at the
    next setup, catch a half-written file: a changed md5 restarts the bridge
    on a config it cannot parse, and the full write then restarts it again
    (#73). The temporary name matches neither the add-on's glob nor the
    orphan cleanup, and it is removed if the write fails.
    """
    payload = json.dumps(config, indent=2)
    directory = os.path.dirname(os.fspath(path)) or "."
    fd, tmp = tempfile.mkstemp(prefix=".philips_avent_bridge_", suffix=".tmp", dir=directory)
    try:
        with os.fdopen(fd, "w") as handle:
            if hasattr(os, "fchmod"):
                os.fchmod(handle.fileno(), BRIDGE_CONFIG_MODE)
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, path)
    except BaseException:
        with contextlib.suppress(OSError):
            os.unlink(tmp)
        raise


# homeassistant.const.CONF_PASSWORD, spelled out so this module stays free of
# Home Assistant imports.
STORED_PASSWORD_KEY = "password"


def strip_stored_password(data: dict) -> dict | None:
    """Config entry data with the account password removed, or None.

    The config flow used to persist the plaintext password even though
    nothing ever read it back — reauth prompts for it again. Returns None
    when there is nothing to remove, so the caller can skip rewriting
    .storage on every startup.
    """
    if STORED_PASSWORD_KEY not in data:
        return None
    return {k: v for k, v in data.items() if k != STORED_PASSWORD_KEY}
