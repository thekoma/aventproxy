"""Constants for the Philips Avent Baby Monitor integration."""
from __future__ import annotations

import json
import math

DOMAIN = "philips_avent"

# Tuya Mobile SDK credentials (static per APK version)
# These are functional identifiers, equivalent to OAuth client_id/secret
# in other HA integrations. Same for all users of the Philips Avent app.
TUYA_SIGNING_KEY = (
    "com.philips.ph.babymonitorplus"
    "_D2:D6:95:A1:1D:1B:84:F9:25:A9:45:6E:27:F4:45:E9:FD:87:C3:74"
    ":63:AA:8A:34:32:A6:6A:23:3B:0F:D5:0F"
    "_8n459nxk9g98gqgcwrpk3csv97uuwajm"
    "_a3nfht4ufwfw9cmkspaftv4x89cx58qx"
)
TUYA_APP_KEY = "wx3at9qprkhskvkcsyhm"
TUYA_PACKAGE_NAME = "com.philips.ph.babymonitorplus"
TUYA_CH_KEY = "071d81fa"

# Default data center (Central Europe). A Tuya account is bound to one data
# center and its session id is rejected by the others, so the real host is
# resolved per account at login time by `region.py` and persisted in the config
# entry. These two constants are only the fallback for entries created before
# that resolution existed (issues #44, #58).
TUYA_API_URL = "https://a1.tuyaeu.com/api.json"
TUYA_DEFAULT_COUNTRY_CODE = "39"

# MQTT is never addressed by a constant: the login response and
# `smartlife.m.user.info.get` both return the account's own broker in their
# `domain` block (`mobileMqttsUrl`, e.g. m1.tuyaeu.com for EU accounts,
# m1.tuyaus.com for American ones) and the bridge connects to that.
TUYA_MQTT_PORT = 8883

# Device model naming (issue #42). The Tuya productId does not distinguish
# models within the SCD9xx family — the same id ("selj2idknqhjnids") has been
# reported by both SCD951 and SCD953/26 units — so the displayed model stays
# generic. Add an entry here only when a productId is confirmed to belong to
# exactly one model.
PRODUCT_ID_TO_MODEL: dict[str, str] = {}
DEFAULT_MODEL = "Avent Baby Monitor"

# DPS codes
DPS_NIGHT_LIGHT = "138"
DPS_BRIGHTNESS = "158"
DPS_LIGHT_COLOR = "204"
DPS_LIGHT_TIMER = "240"
DPS_LIGHT_TIMER_SWITCH = "241"
DPS_LIGHT_REMAINING = "242"      # light_timer_display, -1..86400: assumed to behave like 245 (see Countdown)
DPS_TEMPERATURE = "207"
DPS_TEMPERATURE_F = "208"
DPS_MOTION_SWITCH = "134"
DPS_MOTION_SENSITIVITY = "106"
DPS_SOUND_SWITCH = "139"
DPS_SOUND_SENSITIVITY = "140"
DPS_LULLABY_CONTROL = "201"
DPS_LULLABY_VOLUME = "209"
DPS_LULLABY_MODE = "203"
DPS_LULLABY_STATE = "246"
DPS_LULLABY_TIMER_SWITCH = "243"
DPS_LULLABY_TIMER = "244"
DPS_LULLABY_REMAINING = "245"    # lullaby_display, -1..86400: the set length while it plays, 0 at the end
DPS_PRIVACY_MODE = "237"
DPS_POWER_STATUS = "205"
DPS_FLIP = "102"
DPS_APP_TALKING = "253"
# Alert delivery differs by firmware and by negotiated LAN protocol version. A
# decrypted capture (#51) found none of these three in the LAN DP_QUERY set, and
# on a 3.3 session none of them ever arrived as a push. From 3.4 up DPS 212 does
# arrive: an SCD951 owner measured a motion record pushed with the sensor firing
# 1.3 seconds later, where the poll took 35 (#61), and an SCD973/26 pushed motion
# and sound records within 2.4 seconds (fork 619981b). coordinator.py therefore
# pauses the cloud poll behind a 3.4+ session once the monitor has reported in
# 212, and otherwise keeps the fast poll for monitors reporting alarms in 212.
DPS_ALERT_EVENT = "250"
DPS_DECIBEL_EVENT = "141"
# Alarm record with the snapshot the camera uploaded. The SCD951 and SCD953 on
# issues #61 and #42 report alerts here instead of on DPS 250, and so did an
# SCD973/26 in the fork, with 250 and 141 empty throughout: the key follows the
# firmware, not the model, so both are read on every monitor. See events.py for
# the payload. One slot holding the newest alarm, not a queue.
DPS_ALARM_RECORD = "212"
DPS_SENSEIQ_STATUS = "3"
DPS_SLEEP_SESSION = "4"
# More SenseIQ data points, from the product schema (productId 7d9t0rygsm7ztnww)
# read out of m.life.my.group.device.list. Confirmed live on an SCD9xx.
DPS_SENSEIQ_SWITCH = "1"       # sleepiq_switch  (bool) SenseIQ on/off
DPS_AWAKE_DELAY = "8"          # awake_delay     (value, seconds) awake-alert delay
DPS_AWAKE_SWITCH = "11"        # awake_switch    (bool) baby-awake alert on/off
DPS_CRY_DET_SWITCH = "12"      # cry_det_switch  (bool) cry alert on/off
DPS_NO_SENSEIQ_SIGNAL = "15"   # no_senseiq_signal (bool) True when SenseIQ has no
                               # signal (no baby detected / "scanning crib")
DPS_DEVICE_ERRORS = "18"       # device_errors   (value) 0 = no error

# The 15 tracks, in id order, which is also the order Next steps through them.
LULLABY_CATALOGUE = (
    ("Baa Baa Black Sheep", "lullabies"),
    ("Brahms' Lullaby", "lullabies"),
    ("Rock-a-Bye Baby", "lullabies"),
    ("Golden Slumbers", "lullabies"),
    ("Hush Little Baby", "lullabies"),
    ("Mother's Shh", "noise"),
    ("Calming River", "noise"),
    ("Heartbeat", "noise"),
    ("Vacuum Cleaner", "noise"),
    ("White Noise", "noise"),
    ("Garden Bird Song", "nature_sounds"),
    ("Valley Wind", "nature_sounds"),
    ("Ocean Shore", "nature_sounds"),
    ("Night-time Nature", "nature_sounds"),
    ("Rain Shower", "nature_sounds"),
)
# First id of each block the catalogue is published under. Units of the same
# product (kzm54lhabeeucq5a) report different blocks: the original SCD973 uses
# 3542154-3542168, a unit on firmware 1.1.12 uses 12072916-12072930 (issue #91),
# an SCD923 on firmware 1.4.0 uses 3219038-3219052 (issue #93), an SCD953P on
# firmware 2.4.1 uses 12552458-12552472 (PR #116).
# A camera ignores DPS 202 with an id outside its own block and keeps playing.
LULLABY_BLOCK_STARTS = (3542154, 12072916, 3219038, 12552458)
LULLABY_TRACK_MAP = {
    start + index: track
    for start in LULLABY_BLOCK_STARTS
    for index, track in enumerate(LULLABY_CATALOGUE)
}
# Next and Prev move relative to the current track, so delivering one twice
# moves two tracks. The cloud forwards a published DPS to the camera, which is
# why a step already sent over the LAN must not be mirrored there (issue #91).
LULLABY_STEP_COMMANDS = frozenset({"next", "prev"})

TIMER_OPTIONS = {
    "Off": 0,
    "5 min": 300,
    "10 min": 600,
    "20 min": 1200,
    "30 min": 1800,
    "60 min": 3600,
    "90 min": 5400,
}
TIMER_SECONDS_TO_LABEL = {v: k for k, v in TIMER_OPTIONS.items()}

LULLABY_TRACKS = [name for name, _ in LULLABY_CATALOGUE]

CONF_SID = "sid"
CONF_API_HOST = "api_host"
# Phone device id, generated once and kept, see api.new_device_id and issue #73.
CONF_DEVICE_ID = "device_id"
CONF_COUNTRY_CODE = "country_code"
CONF_ECODE = "ecode"
CONF_PARTNER = "partner_identity"
CONF_UID = "uid"
CONF_CAMERA_ID = "camera_id"
CONF_CAMERA_NAME = "camera_name"
CONF_BRIDGE_PORT = "bridge_port"
DEFAULT_BRIDGE_PORT = 38554
CONF_BRIDGE_HOST = "bridge_host"
DEFAULT_BRIDGE_HOST = "localhost"
# Two-way audio. Off by default: asking the camera for it makes the monitor stop
# a playing lullaby and restart it with a fresh timer when the stream closes
# (issue #72), so listening must not imply talking.
CONF_TALKBACK = "talkback"
DEFAULT_TALKBACK = False
CONF_LAN_STREAMING = "lan_streaming"
DEFAULT_LAN_STREAMING = True


def lullaby_report_id(raw: object) -> int | None:
    """The track id in a DPS 248 report, or None when there is none.

    Most units report the record as JSON. The SCD953P on firmware 2.4.1 escapes
    its quotes (`{\\"id\\":12552458}`), which is not valid JSON until they are
    unescaped, and a record encoded twice decodes to a string first (PR #116).
    """
    report = raw
    if isinstance(report, str):
        try:
            report = json.loads(report)
        except ValueError:
            try:
                report = json.loads(report.replace('\\"', '"'))
            except ValueError:
                return None
    if isinstance(report, str):
        try:
            report = json.loads(report)
        except ValueError:
            return None
    if not isinstance(report, dict):
        return None
    track_id = report.get("id")
    # bool is an int subclass, and a float id never matches a block.
    return track_id if type(track_id) is int else None


def lullaby_track_id(name: str, current_id: int | None) -> int | None:
    """The id to send in DPS 202 to play `name` on this camera.

    The block is taken from the id the camera last reported in DPS 248, since
    that is the only sign of which catalogue it holds. With nothing reported
    yet, the original block is assumed, which is what every release before
    issue #91 sent.
    """
    if name not in LULLABY_TRACKS:
        return None
    start = LULLABY_BLOCK_STARTS[0]
    if isinstance(current_id, int):
        for block in LULLABY_BLOCK_STARTS:
            if block <= current_id < block + len(LULLABY_CATALOGUE):
                start = block
                break
    return start + LULLABY_TRACKS.index(name)


def mirror_dps_to_cloud(dps: dict) -> bool:
    """Whether a command the LAN already delivered should also go to the cloud.

    The mirror keeps the cloud state in step, which is harmless for values and
    for play, pause and stop. A relative step is not idempotent, and the cloud
    forwards it to the camera, so Next would advance two tracks (issue #91).
    """
    return dps.get(DPS_LULLABY_CONTROL) not in LULLABY_STEP_COMMANDS


def seconds_left(value) -> int | None:
    """A timer's time left in seconds; 0 when it is not running (-1), None when unknown."""
    try:
        seconds = int(value)
    except (TypeError, ValueError):
        return None
    return max(seconds, 0)


class Countdown:
    """Counts a lullaby or night light timer down between the monitor's reports.

    The monitor does not count DPS 245 down: while the lullaby plays it holds
    the length the timer was set to, and it reads 0 when the timer ends (an
    SCD923, 21-11-2026: 300 throughout a 5-minute timer; the app counts down
    by itself). An SCD951 on 02-11-2026 still read 1800 from the cloud 22
    minutes into a 30-minute timer; which of the two timers that was is not
    recorded. While nothing plays it reads either 0 or the
    set length (21-11-2026: 300 or 0 on an SCD923, 5400 or 0 on an SCD951,
    at different times; 02-11-2026: 5400 with the lullaby stopped). 242 is
    assumed to behave the same: the night light timer has not been run, and
    with the light off it read 1800 once (02-11-2026) and 0 on both monitors
    on 21-11-2026. So the length alone cannot tell a run from a held value;
    `running` (the lullaby playing, DPS 246; the light on, DPS 138) can. The
    end is the moment a run starts, or a new length is seen while it runs,
    plus that length; the same length reported again keeps the end, and a
    stop clears it. A pause is a stop: a resume counts from the full length
    again (whether the monitor's own timer does the same is not measured).
    After a restart the end comes back from the saved state, as long as the
    length is still the same and the end has not passed. Times are seconds
    since the epoch.

    Without `running` (None: 246 or 138 not reported) only the length counts,
    as a fallback: a monitor that never reports them still gets a countdown
    from the first report of a length. The cost is that a held length then
    counts down while nothing runs, and a start with the length of the run
    before it keeps that run's end (it showed 0 for a whole lullaby on
    02-11-2026).
    """

    def __init__(self, length: int | None = None, ends_at: float | None = None):
        self.length = length
        self.ends_at = ends_at

    @classmethod
    def restored(cls, saved: dict, now: float) -> Countdown:
        """The countdown saved before a restart (length and ends_at).

        An end that passed while Home Assistant was down belongs to a run that
        is over, so a new run of the same length counts anew.
        """
        length, ends_at = saved.get("length"), saved.get("ends_at")
        if not isinstance(length, int) or not isinstance(ends_at, (int, float)):
            return cls()
        return cls(length, float(ends_at) if ends_at > now else None)

    def seen(self, value, now: float, running: bool | None = None) -> None:
        length = seconds_left(value)
        if length is None:
            length = self.length
        if length is None:
            return
        if length == 0 or running is False:
            self.length, self.ends_at = length, None
        elif length != self.length or self.ends_at is None:
            self.length, self.ends_at = length, now + length

    def left(self, now: float) -> int | None:
        """Whole seconds left, rounded up; 0 when not running, None before any report."""
        if self.length is None:
            return None
        if self.ends_at is None:
            return 0
        return max(0, math.ceil(self.ends_at - now))


def lullaby_running(dps: dict | None) -> bool | None:
    """Whether the lullaby plays on its timer (DPS 246, 243), for its Countdown.

    None only when 246 is not reported (Countdown then counts from the length
    alone). Any state but "playing" is a stop, a pause included: nothing sets
    "paused" (the Pause button writes the optimistic "stopping"), and should a
    monitor report it, nothing plays, so no countdown. A lullaby whose timer
    is switched off plays until it is stopped, while 245 may still hold the
    length: no countdown then either.
    """
    if (dps or {}).get(DPS_LULLABY_TIMER_SWITCH) is False:
        return False
    state = (dps or {}).get(DPS_LULLABY_STATE)
    if state == "playing":
        return True
    if state is None:
        return None
    return False


def night_light_running(dps: dict | None) -> bool | None:
    """Whether the night light is on on its timer (DPS 138, 241), for its Countdown.

    None when 138 is not reported (Countdown then counts from the length
    alone). With its timer switched off the light stays on, while 242 may
    still hold the length: no countdown then.
    """
    if (dps or {}).get(DPS_LIGHT_TIMER_SWITCH) is False:
        return False
    on = (dps or {}).get(DPS_NIGHT_LIGHT)
    return on if isinstance(on, bool) else None


def sanitize_rtsp_path(name: str, cam_id: str) -> str:
    """Convert a camera display name into an RTSP path component.

    Spaces, forward slashes and backslashes are replaced with underscores.
    If the result is empty or consists only of an underscore, falls back to
    the camera id. Returns the path component WITHOUT a leading slash; the
    caller composes the URL.

    Mirrors `pkg/storage/path.go::SanitizeRTSPPath` in the Go bridge. The two
    helpers MUST stay in sync — they determine whether the integration's RTSP
    URL matches the path the bridge serves.
    """
    safe = name.replace(" ", "_").replace("/", "_").replace("\\", "_")
    if safe == "" or safe == "_":
        safe = cam_id
    return safe


def build_rtsp_url(host: str, port: int, name: str, cam_id: str) -> str:
    """Build the RTSP URL Home Assistant should pull the stream from.

    The host defaults to localhost, which only holds when the bridge runs
    alongside Home Assistant (add-on, or same container). A bridge in its own
    container or on another machine is not reachable there, and the resulting
    "Connection refused" makes the camera entity flap between Unavailable and
    Idle, because Home Assistant marks a camera unavailable while its stream
    cannot be opened (issue #62).
    """
    return f"rtsp://{host or DEFAULT_BRIDGE_HOST}:{port}/{sanitize_rtsp_path(name, cam_id)}"
