"""Unit tests for the SenseIQ data-point decoders.

Import-clean like the other module tests: conftest.py puts the philips_avent
directory on sys.path, so ``senseiq`` imports without pulling in Home Assistant.

The fixtures are the exact DPS 3 / DPS 4 values captured from an SCD9xx (SenseIQ
product 7d9t0rygsm7ztnww).
"""
import senseiq

# DPS 4 as it comes over the wire: base64 of an ASCII-hex string of the JSON.
DP4_RAW = (
    "N2IyMjczNzQyMjNhMzEzNzM4MzkzNDMwMzkzNzM5MzQyYzIyNzM2NDIyM2EzMTMwMzAzMzM4"
    "MmMyMjYzNzM3MzIyM2EyMjZjMjIyYzIyNjM3MzczNjQyMjNhMzYzMDJjMjI3MzczNjQyMjNh"
    "NWI3YjIyNjEyMjNhMzIzMTM2N2QyYzdiMjI2YzIyM2EzODM2N2QyYzdiMjI2MTIyM2EzOTM0"
    "MzY3ZDJjN2IyMjZjMjIzYTMzMzUzMDdkMmM3YjIyNjQyMjNhMzEzNzMxMzY3ZDJjN2IyMjZj"
    "MjIzYTM5MzI3ZDJjN2IyMjY0MjIzYTMzMzIzNzM0N2QyYzdiMjI2YzIyM2EzNDMwMzY3ZDJj"
    "N2IyMjY0MjIzYTMzMzQ3ZDJjN2IyMjZjMjIzYTMyMzczMTdkMmM3YjIyNjQyMjNhMzEzMDM3"
    "Mzc3ZDJjN2IyMjZjMjIzYTMxMzMzNzM3N2QyYzdiMjI2NDIyM2EzMTMzMzM3ZDVkN2Q="
)


def test_status_no_baby():
    out = senseiq.decode_status('{"r":"o","br":0}')
    assert out["breaths_per_minute"] is None  # 0 -> not reported
    assert out["state"] == "o"


def test_status_breathing():
    out = senseiq.decode_status('{"r":"i","br":42}')
    assert out["breaths_per_minute"] == 42
    assert out["state"] == "i"


def test_status_rejects_non_senseiq():
    assert senseiq.decode_status('{"foo":1}') is None
    assert senseiq.decode_status("") is None
    assert senseiq.decode_status(None) is None


def test_sleep_session_real_capture():
    s = senseiq.decode_sleep_session(DP4_RAW)
    assert s is not None
    assert s["start"] == 1789409794
    assert s["duration_seconds"] == 10038
    assert s["current_stage"] == "light"
    assert s["current_stage_seconds"] == 60
    # Timeline decoded in order, stage codes mapped.
    assert s["stages"][0] == {"stage": "awake", "seconds": 216}
    assert s["stages"][1] == {"stage": "light", "seconds": 86}
    assert s["stages"][4] == {"stage": "deep", "seconds": 1716}
    # Arithmetic invariant observed on hardware: sum(ssd) + cssd == sd.
    total = sum(x["seconds"] for x in s["stages"])
    assert total + s["current_stage_seconds"] == s["duration_seconds"]
    # Per-stage totals.
    assert s["totals_seconds"]["deep"] == 1716 + 3274 + 34 + 1077 + 133
    assert s["totals_seconds"]["awake"] == 216 + 946


def test_sleep_session_rejects_non_session():
    assert senseiq.decode_sleep_session('{"br":0}') is None
    assert senseiq.decode_sleep_session("not base64!!") is None
    assert senseiq.decode_sleep_session(None) is None


def test_is_baby_present():
    # Breathing reported -> present.
    assert senseiq.is_baby_present('{"r":"m","br":38}') is True
    # A present-state flag with no breathing yet -> present.
    assert senseiq.is_baby_present('{"r":"b","br":0}') is True
    assert senseiq.is_baby_present('{"r":"a","br":0}') is True
    # "o" = empty crib -> absent.
    assert senseiq.is_baby_present('{"r":"o","br":0}') is False
    # No SenseIQ status at all -> unknown.
    assert senseiq.is_baby_present(None) is None
    assert senseiq.is_baby_present('{"foo":1}') is None


def test_sleep_session_rejects_negative_durations():
    # A malformed negative duration must not become a reading, and a negative
    # stage segment is dropped rather than skewing the totals.
    s = senseiq.decode_sleep_session(
        '{"st":1789409794,"sd":-5,"css":"l","cssd":-1,'
        '"ssd":[{"l":100},{"d":-20},{"a":50}]}'
    )
    assert s is not None
    assert s["duration_seconds"] is None
    assert s["current_stage_seconds"] is None
    assert s["stages"] == [{"stage": "light", "seconds": 100}, {"stage": "awake", "seconds": 50}]
    assert s["totals_seconds"] == {"awake": 50, "light": 100, "deep": 0}


def test_session_end_and_active():
    s = senseiq.decode_sleep_session(DP4_RAW)
    end = senseiq.session_end_timestamp(s)
    assert end == 1789409794 + 10038
    # Fresh (end ~ now) reads active; a day-old session does not.
    assert senseiq.is_session_active(s, now=end + 10) is True
    assert senseiq.is_session_active(s, now=end + 86400) is False
    # A session far in the future is rejected as clock-skewed/malformed.
    assert senseiq.is_session_active(s, now=end - 86400) is False
    assert senseiq.is_session_active(None) is False


# Real m.solution.sleep.session.day response captured 2026-09-16 (matches the app:
# in bed 11h29, asleep 10h32, deep 6h09, light 4h23, awake 55min).
SLEEP_DAY = [{"dt": 20260916, "totalSd": 41313, "sessions": [{
    "css": "o", "cssd": 0, "et": 1789535909, "sd": 41313, "st": 1789494596, "type": "0",
    "ssd": [{"a": 248}, {"l": 52}, {"a": 194}, {"l": 120}, {"d": 318}, {"l": 129},
            {"d": 4475}, {"l": 805}, {"d": 3340}, {"l": 1641}, {"d": 149}, {"l": 427},
            {"d": 1279}, {"l": 2322}, {"d": 1677}, {"l": 323}, {"d": 577}, {"l": 93},
            {"d": 467}, {"l": 1017}, {"d": 107}, {"l": 308}, {"d": 2212}, {"l": 1507},
            {"d": 254}, {"l": 348}, {"d": 1011}, {"l": 388}, {"d": 1212}, {"l": 1514},
            {"d": 797}, {"l": 265}, {"d": 1475}, {"l": 407}, {"d": 367}, {"l": 144},
            {"d": 190}, {"l": 614}, {"d": 391}, {"l": 277}, {"a": 306}, {"l": 135},
            {"d": 232}, {"l": 541}, {"d": 721}, {"l": 276}, {"d": 937}, {"l": 1601},
            {"a": 926}, {"l": 38}, {"a": 8}, {"l": 14}, {"a": 970}, {"l": 299},
            {"n": 14}, {"l": 97}, {"a": 10}, {"l": 18}, {"a": 4}, {"l": 60}, {"a": 665}],
}]}]


def test_decode_sleep_day():
    s = senseiq.decode_sleep_day(SLEEP_DAY, 20260916)
    assert s is not None
    assert s["date"] == 20260916
    assert s["in_bed_seconds"] == 41313          # 11h29 in bed
    assert s["deep_seconds"] == 22188            # 6h09
    assert s["light_seconds"] == 15780           # 4h23
    assert s["awake_seconds"] == 3331            # 55min
    assert s["no_signal_seconds"] == 14
    assert s["asleep_seconds"] == 15780 + 22188  # 10h32 light + deep
    assert s["start"] == 1789494596              # 19:49
    assert s["end"] == 1789535909                # 07:18
    assert s["session_count"] == 1
    # sum of every stage equals the time in bed
    tot = sum(s[k] for k in ("light_seconds", "deep_seconds", "awake_seconds", "no_signal_seconds"))
    assert tot == s["in_bed_seconds"]


def test_decode_sleep_day_rejects_empty():
    assert senseiq.decode_sleep_day([]) is None
    assert senseiq.decode_sleep_day(None) is None
    assert senseiq.decode_sleep_day([{"dt": 20260916, "sessions": []}]) is None
