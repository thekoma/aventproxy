"""What the bridge does with each camera's stream, from its /status endpoint.

The bridge (pkg/rtsp/status.go) serves, on the port after its RTSP port and
on the same address, a JSON document: per camera the path its stream takes
now (local, imm, webrtc or idle), since when, and the last time it left the
LAN path and why. The "Stream path" sensor reads it; without it Home Assistant
cannot tell whether a monitor streams over the LAN.

Kept free of Home Assistant imports so the unit tests can load it.
"""
from __future__ import annotations

PATH_LOCAL = "local"  # LAN signalling, media over the LAN
PATH_IMM = "imm"  # imm over cloud signalling (the add-on's Dev card)
PATH_WEBRTC = "webrtc"  # the webrtc cloud session
PATH_IDLE = "idle"
STREAM_PATHS = (PATH_LOCAL, PATH_IMM, PATH_WEBRTC, PATH_IDLE)

# Often enough to see a stream start or fall back while a dashboard is open,
# rare enough to cost nothing: one small local request per entry, and only
# while a Stream path sensor is enabled.
STATUS_POLL_SECONDS = 30
STATUS_TIMEOUT = 5


def status_url(host: str, rtsp_port: int) -> str:
    """The bridge's status endpoint: its RTSP host (as the camera's stream URL uses it), the next port."""
    return f"http://{host}:{rtsp_port + 1}/status"


def _text(value: object) -> str | None:
    return value if isinstance(value, str) else None


def parse_status(document: object) -> dict[str, dict]:
    """{camera id: status} from the bridge's document; nothing for what it cannot read.

    A camera's status keeps only known fields with the expected types, and an
    unknown path becomes idle, so a newer or broken bridge cannot put
    arbitrary values into the sensor.
    """
    if not isinstance(document, dict) or not isinstance(document.get("cameras"), list):
        return {}
    out: dict[str, dict] = {}
    for cam in document["cameras"]:
        if not isinstance(cam, dict) or not isinstance(cam.get("camera_id"), str):
            continue
        path = cam.get("path")
        out[cam["camera_id"]] = {
            "path": path if path in STREAM_PATHS else PATH_IDLE,
            "since": _text(cam.get("since")),
            "last_fallback": _text(cam.get("last_fallback")),
            "last_fallback_at": _text(cam.get("last_fallback_at")),
        }
    return out
