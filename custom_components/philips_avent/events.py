"""Motion and sound event payloads, per device family.

Not every monitor reports an alert the same way, which is why the binary sensors
stayed off on several models while the Philips app got the notification (issues
#40, #42, #59, #61):

- The SCD973 and SCD923 family (`kzm54lhabeeucq5a`) sets DPS 250 to
  `motion_detection` and DPS 141 to `decibel_upload`.
- The SCD951 and SCD953 family (`selj2idknqhjnids`) leaves both empty and posts
  the alarm to DPS 212 instead, as base64 JSON describing the snapshot it
  uploaded:
  `{"v":"4.0","cmd":"ipc_motion","alarm":true,"time":1783686591,"files":[...]}`
  (from the diagnostics on #61).

DPS 212 carries its own timestamp, which makes it usable on the cloud poll as
well: the value sticks around in device state, so freshness comes from the
timestamp rather than from having seen the push arrive.

No Home Assistant imports here, so the parsing is unit-tested on its own.
"""
from __future__ import annotations

import base64
import binascii
import json

# An event older than this is history, not something to turn a sensor on for.
# The cloud poll runs every 30 to 120 seconds and DPS 212 keeps the last alarm
# indefinitely, so without this a restart would replay whatever it finds.
EVENT_MAX_AGE_SECONDS = 90.0

MOTION_COMMANDS = frozenset({"ipc_motion", "ipc_move", "motion"})
SOUND_COMMANDS = frozenset({"ipc_sound", "ipc_decibel", "sound", "decibel"})


def decode_event_payload(raw: object) -> dict | None:
    """Decode a DPS value that carries an event as base64-encoded JSON.

    Accepts plain JSON too, since not every firmware base64s it. Returns None for
    anything unreadable: an event we cannot parse must not turn a sensor on.
    """
    if isinstance(raw, dict):
        return raw
    if not isinstance(raw, str) or not raw.strip():
        return None

    text = raw.strip()
    if not text.startswith("{"):
        try:
            text = base64.b64decode(text, validate=True).decode("utf-8")
        except (binascii.Error, ValueError, UnicodeDecodeError):
            return None

    try:
        payload = json.loads(text)
    except (json.JSONDecodeError, ValueError):
        return None
    return payload if isinstance(payload, dict) else None


def event_timestamp(payload: dict | None) -> float | None:
    """Seconds-since-epoch stamp of an event payload, if it has one."""
    if not payload:
        return None
    value = payload.get("time")
    if isinstance(value, bool) or not isinstance(value, (int, float, str)):
        return None
    try:
        stamp = float(value)
    except (TypeError, ValueError):
        return None
    # Some firmwares report milliseconds.
    if stamp > 1e11:
        stamp /= 1000.0
    return stamp if stamp > 0 else None


def alarm_event_timestamp(raw: object, commands: frozenset[str]) -> float | None:
    """Timestamp of an alarm of the given kind carried in a DPS value.

    Returns None when the payload is unreadable, is not one of `commands`, or
    says `alarm: false`.
    """
    payload = decode_event_payload(raw)
    if not payload:
        return None
    if str(payload.get("cmd", "")).lower() not in commands:
        return None
    if payload.get("alarm") is False:
        return None
    return event_timestamp(payload)


def motion_event_timestamp(raw: object) -> float | None:
    """Timestamp of a motion alarm carried in a DPS value (SCD951 DPS 212)."""
    return alarm_event_timestamp(raw, MOTION_COMMANDS)


def sound_event_timestamp(raw: object) -> float | None:
    """Timestamp of a sound alarm carried in a DPS value."""
    return alarm_event_timestamp(raw, SOUND_COMMANDS)


def is_new_event(
    timestamp: float | None,
    last_seen: float | None,
    now: float,
    max_age: float = EVENT_MAX_AGE_SECONDS,
) -> bool:
    """Whether a timestamped event should fire a sensor now.

    True only when the event is newer than the last one acted on and recent
    enough to be happening rather than remembered. A stamp from the future is
    accepted: a monitor whose clock runs ahead still reports real alarms.
    """
    if timestamp is None:
        return False
    if last_seen is not None and timestamp <= last_seen:
        return False
    return timestamp >= now - max_age
