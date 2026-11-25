"""Camera entity for Philips Avent Baby Monitor."""
from __future__ import annotations

import asyncio
import logging
import shlex
import time

from homeassistant.components.camera import Camera, CameraEntityFeature
from homeassistant.components.ffmpeg import async_get_image as ffmpeg_get_image
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from .const import (
    CONF_BRIDGE_HOST,
    CONF_BRIDGE_PORT,
    CONF_SNAPSHOT_INTERVAL,
    CONF_TALKBACK,
    DEFAULT_BRIDGE_HOST,
    DEFAULT_BRIDGE_PORT,
    DEFAULT_SNAPSHOT_INTERVAL,
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

# After a grab through the bridge that failed (an error, or no picture), the
# next one may start this many seconds later, so a passing failure does not
# leave the camera without a picture for the full snapshot interval (at least
# a minute).
SNAPSHOT_RETRY_INTERVAL = 60


def stream_running(stream) -> bool:
    """Whether Home Assistant's stream runs now, so asking it starts nothing.

    `Stream.available` stays true after the last viewer left. Without
    "Preload stream" the stream keeps an output only while it is in use, so an
    output that is not idle means the worker runs. With "Preload stream" on,
    Home Assistant keeps the worker running while every output is idle (and
    restarts a worker that failed), so the stream counts as running then too.
    Home Assistant starts a preloaded stream when it starts; when "Preload
    stream" is switched on later, the stream counts as running before it does,
    and the next snapshot starts it.
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
    snapshot_interval = entry.options.get(CONF_SNAPSHOT_INTERVAL, DEFAULT_SNAPSHOT_INTERVAL) * 60
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        entities.append(AventCamera(coordinator, cam_id, bridge_port, bridge_host, talkback, snapshot_interval))
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
        snapshot_interval: float = DEFAULT_SNAPSHOT_INTERVAL * 60,
    ):
        super().__init__()
        self.coordinator = coordinator
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_camera"
        self._stream_url = build_rtsp_url(
            bridge_host, bridge_port, coordinator.camera_name, cam_id, coordinator.rtsp_path
        )
        self._attr_device_info = build_device_info(coordinator, cam_id)
        if talkback and CameraEntityCapabilities is not None:
            # Home Assistant offers its microphone (a Labs preview, over WebRTC
            # only) to a camera that declares two-way audio. go2rtc sends it on
            # the bridge's RTSP backchannel, which the bridge advertises while
            # talkback is on.
            self._attr_camera_entity_capabilities = CameraEntityCapabilities(two_way_audio=True)
        # Seconds between grabs through the bridge, the last picture, the grab
        # in flight, and the earliest time (time.monotonic) the next may start.
        self._snapshot_interval = snapshot_interval
        self._last_image: bytes | None = None
        self._grab: asyncio.Task | None = None
        self._next_grab: float | None = None

    async def stream_source(self) -> str:
        return self._stream_url

    async def async_camera_image(
        self,
        width: int | None = None,
        height: int | None = None,
    ) -> bytes | None:
        """A JPEG of the camera.

        Required by the `camera.snapshot` service. With a snapshot interval of
        0 (the default) every call pulls a frame through the bridge. With an
        interval, the picture comes from Home Assistant's own stream while it
        runs (no extra connection to the bridge); otherwise the last picture
        is returned, and a new one is pulled through the bridge at most once
        per interval, or SNAPSHOT_RETRY_INTERVAL after a grab that failed.
        Such a grab starts the camera's stream on the bridge when none runs
        there, which the bridge then keeps for half a minute. It is taken at
        full size: the picture is kept for later callers, and Home Assistant
        scales it afterwards for a caller that asked for a width and height.
        """
        if not self._snapshot_interval:
            return await self._ffmpeg_image(width, height)
        stream = self.stream
        if stream_running(stream):
            # Stream.async_get_image starts the stream when it is not running,
            # so it is asked only while it runs. Before the stream's first
            # keyframe it has no picture: keep the last one rather than open a
            # second connection to the bridge. (A later session of the stream
            # returns the previous session's picture until its own first
            # keyframe.)
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
        try:
            return await asyncio.shield(grab)
        except asyncio.CancelledError:
            # Only the grab was cancelled (the entity is going), not this call:
            # Home Assistant's _async_get_image suppresses only TimeoutError.
            if grab.cancelled() and not asyncio.current_task().cancelling():
                return self._last_image
            raise

    async def async_will_remove_from_hass(self) -> None:
        """Cancel a grab through the bridge when the entity goes (an unload or a removal).

        Its picture is then not kept. The ffmpeg run itself goes on: Home
        Assistant's ffmpeg component shields it, and it ends on its own within
        15 s (haffmpeg's timeout).
        """
        if self._grab is not None:
            self._grab.cancel()

    async def _grab_through_bridge(self) -> bytes | None:
        started = time.monotonic()
        image = await self._ffmpeg_image()
        if image:
            self._last_image = image
            self._next_grab = started + self._snapshot_interval
        else:
            self._next_grab = started + SNAPSHOT_RETRY_INTERVAL
        return image or self._last_image

    async def _ffmpeg_image(self, width: int | None = None, height: int | None = None) -> bytes | None:
        """One frame through the bridge, which starts the camera's stream when none runs there."""
        try:
            # haffmpeg runs shlex.split on the input source and passes it on
            # split when that gives more than one token, else unchanged after
            # "-i". With "-i" and the URL quoted, ffmpeg gets the exact URL for
            # any camera name (an apostrophe made shlex.split raise). Over TCP:
            # by default ffmpeg first has the bridge send the media over UDP to
            # the address the request came from, which is not Home Assistant's
            # behind a NAT (a published port), and its fallback to TCP comes
            # after the snapshot has already timed out.
            return await ffmpeg_get_image(
                self.hass,
                f"-rtsp_transport tcp -i {shlex.quote(self._stream_url)}",
                width=width,
                height=height,
            )
        except Exception:
            _LOGGER.exception("ffmpeg snapshot failed for %s", self._stream_url)
            return None
