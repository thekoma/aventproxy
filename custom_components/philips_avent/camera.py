"""Camera entity for Philips Avent Baby Monitor."""
from __future__ import annotations

import asyncio
import logging
import time

from homeassistant.components.camera import Camera, CameraEntityFeature
from homeassistant.components.ffmpeg import async_get_image as ffmpeg_get_image
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from .const import (
    CONF_BRIDGE_HOST,
    CONF_BRIDGE_PORT,
    CONF_TALKBACK,
    DEFAULT_BRIDGE_HOST,
    DEFAULT_BRIDGE_PORT,
    DEFAULT_TALKBACK,
    DOMAIN,
    build_rtsp_url,
)
from .coordinator import PhilipsAventCoordinator
from .entity import build_device_info

try:  # Home Assistant 2026.11 and later
    from homeassistant.components.camera import CameraEntityCapabilities
except ImportError:  # an older core has no two-way audio to declare
    CameraEntityCapabilities = None

_LOGGER = logging.getLogger(__name__)

# A snapshot pulled through the bridge starts the camera's stream when none
# is running, and the bridge keeps it for half a minute after: Home Assistant
# and its frontend asked about once a second on 02-11-2026 (205 times in three
# minutes, most for a camera whose stream could not start), which would keep
# the camera streaming next to the parent unit. So a snapshot comes from Home
# Assistant's own running stream, else the last picture is reused, and a new
# one through the bridge is taken at most this often per camera.
SNAPSHOT_MIN_INTERVAL = 600
# After a grab through the bridge that failed (an error, or no picture), the
# next one may start this much later, so a passing failure does not leave the
# camera without a picture for the full interval.
SNAPSHOT_RETRY_INTERVAL = 60


def stream_running(stream) -> bool:
    """Whether Home Assistant's stream runs now, so asking it starts nothing.

    `Stream.available` stays true after the last viewer left. Without
    "Preload stream" the stream keeps an output only while it is in use, so an
    output that is not idle means the worker runs. With "Preload stream" on,
    Home Assistant keeps the worker running while every output is idle (and
    starts it again if it stopped), so the stream counts as running then too.
    """
    if stream is None or not stream.available:
        return False
    settings = getattr(stream, "dynamic_stream_settings", None)
    if getattr(settings, "preload_stream", False):
        return True
    return any(not output.idle for output in stream.outputs().values())


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    bridge_port = entry.options.get(CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT)
    bridge_host = entry.options.get(CONF_BRIDGE_HOST, DEFAULT_BRIDGE_HOST)
    talkback = entry.options.get(CONF_TALKBACK, DEFAULT_TALKBACK)
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        entities.append(AventCamera(coordinator, cam_id, bridge_port, bridge_host, talkback))
    async_add_entities(entities)


class AventCamera(Camera):
    """Camera entity pointing to the WebRTC bridge RTSP stream."""

    _attr_has_entity_name = True
    _attr_translation_key = "camera"
    _attr_supported_features = CameraEntityFeature.STREAM

    def __init__(
        self,
        coordinator: PhilipsAventCoordinator,
        cam_id: str,
        bridge_port: int = DEFAULT_BRIDGE_PORT,
        bridge_host: str = DEFAULT_BRIDGE_HOST,
        talkback: bool = DEFAULT_TALKBACK,
    ):
        super().__init__()
        self.coordinator = coordinator
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_camera"
        self._stream_url = build_rtsp_url(
            bridge_host, bridge_port, coordinator.camera_name, cam_id
        )
        self._attr_device_info = build_device_info(coordinator, cam_id)
        if talkback and CameraEntityCapabilities is not None:
            # Home Assistant offers its microphone (a Labs preview, over WebRTC
            # only) to a camera that declares two-way audio. go2rtc sends it on
            # the bridge's RTSP backchannel, which the bridge advertises while
            # talkback is on.
            self._attr_camera_entity_capabilities = CameraEntityCapabilities(two_way_audio=True)
        self._last_image: bytes | None = None
        # The grab through the bridge in flight, and the earliest time
        # (time.monotonic) the next one may start.
        self._grab: asyncio.Task | None = None
        self._next_grab: float | None = None

    async def stream_source(self) -> str:
        return self._stream_url

    async def async_camera_image(
        self,
        width: int | None = None,
        height: int | None = None,
    ) -> bytes | None:
        """A JPEG of the camera; through the bridge at most every 10 minutes.

        Required by the `camera.snapshot` service. From Home Assistant's own
        stream while it runs (no extra connection to the bridge); otherwise
        the last picture, and a new one through the bridge (ffmpeg, over TCP)
        at most once per SNAPSHOT_MIN_INTERVAL, or SNAPSHOT_RETRY_INTERVAL
        after a grab that failed. That grab does start the camera's stream on
        the bridge when none runs, which the bridge then keeps for half a
        minute. Always at full size: the picture is kept for later callers,
        and Home Assistant scales it afterwards for a caller that asked for a
        width and height.
        """
        stream = self.stream
        if stream_running(stream):
            # Stream.async_get_image starts the stream when it is not running,
            # so it is asked only while it runs. Before the first keyframe it
            # has no picture: keep the last one rather than open a second
            # connection to the bridge.
            image = await stream.async_get_image()
            if image:
                self._last_image = image
            return image or self._last_image
        grab = self._grab
        if grab is None or grab.done():
            if self._next_grab is not None and time.monotonic() < self._next_grab:
                return self._last_image
            grab = self._grab = self.hass.async_create_background_task(
                self._grab_through_bridge(), f"philips_avent snapshot {self._cam_id}"
            )
        # Home Assistant cancels this call after CAMERA_IMAGE_TIMEOUT (10 s).
        # The grab goes on and keeps its picture for the next caller; a caller
        # that comes while it runs waits for the same grab.
        return await asyncio.shield(grab)

    async def _grab_through_bridge(self) -> bytes | None:
        started = time.monotonic()
        image = None
        try:
            image = await ffmpeg_get_image(self.hass, f"-rtsp_transport tcp -i {self._stream_url}")
        except Exception:
            _LOGGER.exception("ffmpeg snapshot failed for %s", self._stream_url)
        if image:
            self._last_image = image
            self._next_grab = started + SNAPSHOT_MIN_INTERVAL
        else:
            self._next_grab = started + SNAPSHOT_RETRY_INTERVAL
        return image or self._last_image
