"""Snapshots come from Home Assistant's running stream, else through the bridge at most every 10 minutes.

A grab through the bridge (ffmpeg) starts the camera's stream on the bridge
when none runs, so it is rate-limited, not avoided. Loads the real camera
entity against the Home Assistant stand-in, with ffmpeg and Home Assistant's
stream replaced by fakes that count their calls.
"""
from __future__ import annotations

import asyncio
from types import SimpleNamespace

import pytest

from .ha_stand_in import home_assistant_installed, load

pytestmark = pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")


class FakeStream:
    """Home Assistant's Stream: `available` stays true after the last viewer left."""

    def __init__(self, idle: list[bool], image: bytes | None = b"from-stream", preload: bool = False):
        self.available = True
        self.dynamic_stream_settings = SimpleNamespace(preload_stream=preload)
        self._outputs = {f"out{i}": SimpleNamespace(idle=flag) for i, flag in enumerate(idle)}
        self.image = image
        self.asked: list[tuple] = []

    def outputs(self):
        return self._outputs

    async def async_get_image(self, width=None, height=None):
        self.asked.append((width, height))
        return self.image


@pytest.fixture
def camera(monkeypatch):
    module = load(monkeypatch, "camera")
    grabs: list[tuple] = []
    clock = [1000.0]
    # What the next grab does: a picture, None, or an exception to raise.
    answers: list = []

    async def ffmpeg_get_image(hass, input_source, width=None, height=None):
        grabs.append((input_source, width, height))
        answer = answers.pop(0) if answers else f"from-bridge-{len(grabs)}".encode()
        if isinstance(answer, Exception):
            raise answer
        if isinstance(answer, asyncio.Event):
            await answer.wait()
            return f"from-bridge-{len(grabs)}".encode()
        return answer

    monkeypatch.setattr(module, "ffmpeg_get_image", ffmpeg_get_image)
    # Only the module's clock: patching time.monotonic itself would also
    # freeze the event loop's clock under asyncio.run.
    monkeypatch.setattr(module, "time", SimpleNamespace(monotonic=lambda: clock[0]))
    coordinator = SimpleNamespace(camera_name="Nursery", device_info={})
    entity = module.AventCamera(coordinator, "bf1camera")
    entity.hass = SimpleNamespace(
        async_create_background_task=lambda target, name: asyncio.get_running_loop().create_task(target, name=name)
    )
    return SimpleNamespace(module=module, entity=entity, grabs=grabs, clock=clock, answers=answers)


def snapshot(entity, width=None, height=None):
    return asyncio.run(entity.async_camera_image(width, height))


def test_through_the_bridge_at_most_every_ten_minutes(camera):
    assert snapshot(camera.entity) == b"from-bridge-1"
    camera.clock[0] += camera.module.SNAPSHOT_MIN_INTERVAL - 1
    assert snapshot(camera.entity) == b"from-bridge-1"
    assert len(camera.grabs) == 1
    camera.clock[0] += 1
    assert snapshot(camera.entity) == b"from-bridge-2"
    source = camera.grabs[0][0]
    assert source.startswith("-rtsp_transport tcp -i rtsp://") and source.endswith("/Nursery")


def test_a_stream_without_a_viewer_is_not_asked(camera):
    # Stream.async_get_image would start the stream again.
    stream = FakeStream(idle=[True])
    camera.entity.stream = stream
    assert snapshot(camera.entity) == b"from-bridge-1"
    assert stream.asked == []


def test_an_unavailable_stream_is_not_asked(camera):
    stream = FakeStream(idle=[False])
    stream.available = False
    camera.entity.stream = stream
    assert snapshot(camera.entity) == b"from-bridge-1"
    assert stream.asked == []


def test_a_running_stream_gives_the_picture(camera):
    stream = FakeStream(idle=[True, False])
    camera.entity.stream = stream
    assert snapshot(camera.entity) == b"from-stream"
    assert camera.grabs == []


def test_a_preloaded_stream_gives_the_picture_while_idle(camera):
    # With "Preload stream" on, Home Assistant keeps the worker running while
    # every output is idle; a grab through the bridge would be a second session.
    stream = FakeStream(idle=[True], preload=True)
    camera.entity.stream = stream
    assert snapshot(camera.entity) == b"from-stream"
    assert camera.grabs == []


def test_before_the_first_keyframe_the_last_picture_is_kept(camera):
    assert snapshot(camera.entity) == b"from-bridge-1"
    camera.entity.stream = FakeStream(idle=[False], image=None)
    camera.clock[0] += camera.module.SNAPSHOT_MIN_INTERVAL
    assert snapshot(camera.entity) == b"from-bridge-1"
    assert len(camera.grabs) == 1


def test_the_kept_picture_is_full_size(camera):
    # Home Assistant scales a picture after async_camera_image when a caller
    # asks for a size, so a thumbnail request must not leave a thumbnail for
    # the next camera.snapshot.
    stream = FakeStream(idle=[False])
    camera.entity.stream = stream
    snapshot(camera.entity, 320, 180)
    camera.entity.stream = None
    snapshot(camera.entity, 320, 180)
    assert stream.asked == [(None, None)]
    assert [grab[1:] for grab in camera.grabs] == [(None, None)]


@pytest.mark.parametrize("failure", [RuntimeError("ffmpeg broke"), None], ids=["exception", "no-picture"])
def test_a_failed_grab_is_retried_after_a_short_pause(camera, failure):
    assert snapshot(camera.entity) == b"from-bridge-1"
    camera.clock[0] += camera.module.SNAPSHOT_MIN_INTERVAL
    camera.answers.append(failure)
    # The failed grab returns the last picture.
    assert snapshot(camera.entity) == b"from-bridge-1"
    camera.clock[0] += camera.module.SNAPSHOT_RETRY_INTERVAL - 1
    assert snapshot(camera.entity) == b"from-bridge-1"
    assert len(camera.grabs) == 2
    camera.clock[0] += 1
    assert snapshot(camera.entity) == b"from-bridge-3"
    assert camera.module.SNAPSHOT_RETRY_INTERVAL < camera.module.SNAPSHOT_MIN_INTERVAL


def test_a_first_grab_that_fails_gives_nothing_and_is_retried(camera):
    camera.answers.append(RuntimeError("ffmpeg broke"))
    assert snapshot(camera.entity) is None
    camera.clock[0] += camera.module.SNAPSHOT_RETRY_INTERVAL
    assert snapshot(camera.entity) == b"from-bridge-2"


def test_a_slow_grab_keeps_its_picture_after_the_caller_gave_up(camera):
    # Home Assistant cancels async_camera_image after CAMERA_IMAGE_TIMEOUT.
    slow = asyncio.Event()
    camera.answers.append(slow)

    async def scenario():
        first = asyncio.create_task(camera.entity.async_camera_image())
        await asyncio.sleep(0)
        second = asyncio.create_task(camera.entity.async_camera_image())
        await asyncio.sleep(0)
        first.cancel()
        with pytest.raises(asyncio.CancelledError):
            await first
        slow.set()
        # A caller that came during the grab waits for the same grab.
        assert await second == b"from-bridge-1"
        assert len(camera.grabs) == 1
        # The next caller gets the picture the cancelled call started.
        assert await camera.entity.async_camera_image() == b"from-bridge-1"
        assert len(camera.grabs) == 1

    asyncio.run(scenario())


def test_the_picture_of_a_grab_whose_only_caller_gave_up_is_served_next(camera):
    slow = asyncio.Event()
    camera.answers.append(slow)

    async def scenario():
        first = asyncio.create_task(camera.entity.async_camera_image())
        await asyncio.sleep(0)
        first.cancel()
        with pytest.raises(asyncio.CancelledError):
            await first
        slow.set()
        for _ in range(5):
            await asyncio.sleep(0)
        assert await camera.entity.async_camera_image() == b"from-bridge-1"
        assert len(camera.grabs) == 1

    asyncio.run(scenario())
