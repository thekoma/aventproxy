"""Camera entity for Philips Avent Baby Monitor."""
from __future__ import annotations

import logging
import shlex

from homeassistant.components.camera import Camera, CameraEntityFeature
from homeassistant.components.ffmpeg import async_get_image as ffmpeg_get_image
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.dispatcher import async_dispatcher_connect
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from .const import (
    CONF_BRIDGE_HOST,
    CONF_BRIDGE_PORT,
    CONF_TALKBACK,
    DEFAULT_BRIDGE_HOST,
    DEFAULT_BRIDGE_PORT,
    DEFAULT_TALKBACK,
    DOMAIN,
    SIGNAL_STREAM_SOURCE,
    STREAM_SOURCE_AUTO,
    build_rtsp_url,
)
from .coordinator import PhilipsAventCoordinator
from .entity import build_device_info

try:  # Home Assistant 2026.11 and later
    from homeassistant.components.camera import CameraEntityCapabilities
except ImportError:  # an older core has no two-way audio to declare
    CameraEntityCapabilities = None

_LOGGER = logging.getLogger(__name__)


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    bridge_port = entry.options.get(CONF_BRIDGE_PORT, DEFAULT_BRIDGE_PORT)
    bridge_host = entry.options.get(CONF_BRIDGE_HOST, DEFAULT_BRIDGE_HOST)
    talkback = entry.options.get(CONF_TALKBACK, DEFAULT_TALKBACK)
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        entities.append(AventCamera(
            coordinator, cam_id, bridge_port, bridge_host, talkback, data.get("stream_sources")
        ))
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
        stream_sources: dict[str, str] | None = None,
    ):
        super().__init__()
        self.coordinator = coordinator
        self._cam_id = cam_id
        self._attr_unique_id = f"{cam_id}_camera"
        self._bridge_host = bridge_host
        self._bridge_port = bridge_port
        # The "Stream source" selects' options, shared with select.py.
        self._stream_sources = stream_sources if stream_sources is not None else {}
        self._attr_device_info = build_device_info(coordinator, cam_id)
        if talkback and CameraEntityCapabilities is not None:
            # Home Assistant offers its microphone (a Labs preview, over WebRTC
            # only) to a camera that declares two-way audio. go2rtc sends it on
            # the bridge's RTSP backchannel, which the bridge advertises while
            # talkback is on.
            self._attr_camera_entity_capabilities = CameraEntityCapabilities(two_way_audio=True)

    @property
    def _stream_url(self) -> str:
        source = self._stream_sources.get(self._cam_id, STREAM_SOURCE_AUTO)
        return build_rtsp_url(
            self._bridge_host,
            self._bridge_port,
            self.coordinator.camera_name,
            self._cam_id,
            self.coordinator.rtsp_path,
            source=source,
        )

    async def stream_source(self) -> str:
        return self._stream_url

    async def async_added_to_hass(self) -> None:
        await super().async_added_to_hass()
        self.async_on_remove(async_dispatcher_connect(
            self.hass, SIGNAL_STREAM_SOURCE.format(self._cam_id), self._stream_source_changed
        ))

    @callback
    def _stream_source_changed(self) -> None:
        """Point Home Assistant's own stream at the new path.

        WebRTC through go2rtc asks stream_source() again for every offer
        (go2rtc/__init__.py, 2026.11.2), so only the stream worker (HLS,
        recording) has to move. Stream.update_source restarts a running
        worker with the new source. On a stopped stream it leaves the
        fast-restart flag set, which start() does not clear (still so in
        2026.11.2), and the next stop() then restarted the worker instead of
        ending it and waited for it for good: on 05-11-2026 (2026.10.4) the
        dashboard's stream request never answered after a source change. A
        stopped stream only gets the new source for its next start.
        """
        stream, url = self.stream, self._stream_url
        if stream is None or stream.source == url:
            return
        worker = getattr(stream, "_thread", None)
        if worker is not None and worker.is_alive():
            stream.update_source(url)
        else:
            stream.source = url

    async def async_camera_image(
        self,
        width: int | None = None,
        height: int | None = None,
    ) -> bytes | None:
        """Pull a single JPEG frame from the live RTSP stream.

        Required by the `camera.snapshot` service. Without this override the
        base class raises NotImplementedError. The helper runs ffmpeg under
        the hood; it shares the running bridge stream so it adds no extra
        load on the camera itself.
        """
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
