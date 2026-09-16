"""Decode Philips Avent SenseIQ live status and sleep-session data."""
from __future__ import annotations

import base64
import binascii
import json
import logging
import time
from datetime import datetime

_LOGGER = logging.getLogger(__name__)

STAGE_NAMES = {"a": "awake", "l": "light", "d": "deep"}
SLEEP_STAGES = ("awake", "light", "deep")

# If st + sd has not moved for this long, DPS 4 is considered to be the
# persisted last session rather than a live session.
SESSION_STALE_AFTER_SECONDS = 300

# Small tolerance for clock skew / server timestamps slightly ahead of HA.
SESSION_FUTURE_TOLERANCE_SECONDS = 120


def _looks_hex(text: str) -> bool:
    """True when the string is an even-length run of hex digits."""
    if not text or len(text) % 2:
        return False
    return all(c in "0123456789abcdefABCDEF" for c in text)


def _decode_raw_json(raw: object) -> dict | None:
    """Decode a SenseIQ data point into a dict, or None if unreadable."""
    if isinstance(raw, dict):
        return raw
    if not isinstance(raw, str) or not raw.strip():
        return None

    text = raw.strip()
    if text.startswith("{"):
        try:
            return json.loads(text)
        except (json.JSONDecodeError, ValueError):
            return None

    try:
        decoded = base64.b64decode(text, validate=True)
    except (binascii.Error, ValueError):
        return None

    try:
        inner = decoded.decode("utf-8").strip()
    except UnicodeDecodeError:
        return None

    if inner.startswith("{"):
        try:
            return json.loads(inner)
        except (json.JSONDecodeError, ValueError):
            return None

    if _looks_hex(inner):
        try:
            payload = bytes.fromhex(inner).decode("utf-8")
            return json.loads(payload)
        except (ValueError, json.JSONDecodeError, UnicodeDecodeError):
            return None

    return None


def _to_int(value: object) -> int | None:
    """Coerce to int, rejecting bools and unparseable values."""
    if isinstance(value, bool) or value is None:
        return None
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def _to_nonneg_int(value: object) -> int | None:
    """Coerce to a non-negative int, or None.

    SenseIQ durations are never negative; a negative one is malformed and must
    not become a reading (would break the totals and the sum(ssd)+cssd==sd
    invariant).
    """
    secs = _to_int(value)
    return secs if (secs is not None and secs >= 0) else None


def _epoch(value: object) -> int | None:
    """Epoch seconds from a SenseIQ timestamp; tolerates milliseconds."""
    stamp = _to_int(value)
    if stamp is None or stamp <= 0:
        return None
    if stamp > 1_000_000_000_000:
        stamp //= 1000
    return stamp


def decode_status(raw: object) -> dict | None:
    """Decode DPS 3 (senseiq_status)."""
    data = _decode_raw_json(raw)
    if not isinstance(data, dict) or "br" not in data:
        return None

    bpm = _to_int(data.get("br"))
    return {
        "breaths_per_minute": bpm if (bpm is not None and bpm > 0) else None,
        "state": data.get("r"),
        "raw": data,
    }


# Values of the DPS 3 ``r`` flag that mean "no baby present". Observed over a
# night on an SCD9xx: ``o`` while the crib was empty, and ``m``/``b``/``a`` while
# the baby was detected. DPS 15 (``no_senseiq_signal``) does NOT track live
# presence — it stayed set all night while breathing and sleep were reported —
# so presence is read from DPS 3 instead.
PRESENCE_ABSENT_STATES = frozenset({"o"})


def is_baby_present(raw: object) -> bool | None:
    """Whether SenseIQ currently sees the baby, from DPS 3.

    True when a breathing rate is reported or the ``r`` flag is a present-state;
    False when ``r`` says absent (``o``); None when there is no SenseIQ status to
    read (so the sensor is "unknown" rather than a false "away").
    """
    status = decode_status(raw)
    if status is None:
        return None
    if status["breaths_per_minute"] is not None:
        return True
    state = status.get("state")
    if not isinstance(state, str) or not state:
        return None
    return state not in PRESENCE_ABSENT_STATES


def decode_sleep_session(raw: object) -> dict | None:
    """Decode DPS 4 (sleep_session_data)."""
    data = _decode_raw_json(raw)
    if not isinstance(data, dict) or "ssd" not in data:
        return None

    stages: list[dict] = []
    totals = {name: 0 for name in SLEEP_STAGES}

    for item in data.get("ssd") or []:
        if not isinstance(item, dict) or len(item) != 1:
            continue
        (code, seconds), = item.items()
        name = STAGE_NAMES.get(code)
        secs = _to_nonneg_int(seconds)
        if name is None or secs is None:
            continue
        stages.append({"stage": name, "seconds": secs})
        totals[name] += secs

    return {
        "start": _epoch(data.get("st")),
        "duration_seconds": _to_nonneg_int(data.get("sd")),
        "current_stage": STAGE_NAMES.get(data.get("css")),
        "current_stage_seconds": _to_nonneg_int(data.get("cssd")),
        "stages": stages,
        "totals_seconds": totals,
    }


def session_end_timestamp(session: dict | None) -> int | None:
    """Return st + sd for a decoded session."""
    if not session:
        return None

    start = _to_int(session.get("start"))
    duration = _to_int(session.get("duration_seconds"))
    if start is None or duration is None or start <= 0 or duration < 0:
        return None

    return start + duration


def is_session_active(
    session: dict | None,
    *,
    now: float | int | datetime | None = None,
    stale_after_seconds: int = SESSION_STALE_AFTER_SECONDS,
) -> bool:
    """Return True when DPS 4 looks like a live, still-updating session.

    DPS 4 persists the last completed session. During a live session, st + sd
    stays close to the current time because sd grows as SenseIQ tracks stages.
    """
    end = session_end_timestamp(session)
    if end is None:
        return False

    if now is None:
        now_ts = time.time()
    elif isinstance(now, datetime):
        now_ts = now.timestamp()
    else:
        now_ts = float(now)

    age = now_ts - end

    # Too far in the future means malformed/clock-skewed data.
    if age < -SESSION_FUTURE_TOLERANCE_SECONDS:
        return False

    return age <= stale_after_seconds


# Stage codes in the cloud daily summary (m.solution.sleep.session.day). Same as
# the live session plus `n` = no signal (the grey "Aucun signal" in the app).
NIGHT_STAGE_NAMES = {"a": "awake", "l": "light", "d": "deep", "n": "no_signal"}
NIGHT_STAGES = ("awake", "light", "deep", "no_signal")


def decode_sleep_day(result: object, date: object = None) -> dict | None:
    """Decode the SenseIQ daily sleep summary into night totals.

    `result` is the list returned by `m.solution.sleep.session.day`: one record
    per day, each `{"dt": YYYYMMDD, "sessions": [...], "totalSd": ...}`. Every
    session carries `st`/`et` (in bed from/to), `sd` (time in bed) and `ssd`
    (the full stage timeline). Aggregates all of a day's sessions.

    Returns None when there is nothing to decode. Otherwise::

        {
          "date": int,                 # YYYYMMDD
          "in_bed_seconds": int,       # sum of session sd (time in bed)
          "asleep_seconds": int,       # light + deep
          "light_seconds": int, "deep_seconds": int,
          "awake_seconds": int, "no_signal_seconds": int,
          "start": int|None, "end": int|None,   # earliest st / latest et (epoch)
          "session_count": int,
          "sessions": [ {start, end, in_bed_seconds, totals_seconds}, ... ],
        }
    """
    if not isinstance(result, list) or not result:
        return None

    record = None
    if date is not None:
        for day in result:
            if isinstance(day, dict) and str(day.get("dt")) == str(date):
                record = day
                break
    if record is None:
        dicts = [d for d in result if isinstance(d, dict)]
        if not dicts:
            return None
        record = dicts[-1]

    totals = {name: 0 for name in NIGHT_STAGES}
    in_bed = 0
    start = None
    end = None
    sessions_out: list[dict] = []

    for session in record.get("sessions") or []:
        if not isinstance(session, dict):
            continue
        st = _to_int(session.get("st"))
        et = _to_int(session.get("et"))
        sd = _to_nonneg_int(session.get("sd"))
        if sd:
            in_bed += sd
        if st is not None and st > 0:
            start = st if start is None else min(start, st)
        if et is not None and et > 0:
            end = et if end is None else max(end, et)

        per = {name: 0 for name in NIGHT_STAGES}
        for seg in session.get("ssd") or []:
            if not isinstance(seg, dict) or len(seg) != 1:
                continue
            (code, seconds), = seg.items()
            name = NIGHT_STAGE_NAMES.get(code)
            secs = _to_nonneg_int(seconds)
            if name is None or secs is None:
                continue
            totals[name] += secs
            per[name] += secs

        sessions_out.append({
            "start": _epoch(session.get("st")),
            "end": _epoch(session.get("et")),
            "in_bed_seconds": sd,
            "totals_seconds": per,
        })

    if not sessions_out:
        return None

    return {
        "date": _to_int(record.get("dt")),
        "in_bed_seconds": in_bed,
        "asleep_seconds": totals["light"] + totals["deep"],
        "light_seconds": totals["light"],
        "deep_seconds": totals["deep"],
        "awake_seconds": totals["awake"],
        "no_signal_seconds": totals["no_signal"],
        "start": _epoch(start),
        "end": _epoch(end),
        "session_count": len(sessions_out),
        "sessions": sessions_out,
    }
