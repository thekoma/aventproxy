"""With a snapshot interval, snapshots come from Home Assistant's running stream, else through the bridge at most once per interval.

A grab through the bridge (ffmpeg) starts the camera's stream on the bridge
when none runs, so it is rate-limited, not avoided. Loads the real camera
entity (and the options flow) against the Home Assistant stand-in, with ffmpeg
and Home Assistant's stream replaced by fakes that count their calls.
"""
from __future__ import annotations

import asyncio
import sys
import types
from types import SimpleNamespace

import pytest
import voluptuous as vol

from .ha_stand_in import Hass, home_assistant_installed, load

pytestmark = pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")

TEN_MINUTES = 600


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


class Camera(SimpleNamespace):
    def make(self, interval: float | None = TEN_MINUTES):
        """A camera entity; the default snapshot interval when `interval` is None."""
        args = {} if interval is None else {"snapshot_interval": interval}
        coordinator = SimpleNamespace(camera_name="Nursery", display_name="Nursery", rtsp_path=None, device_info={})
        entity = self.module.AventCamera(coordinator, "bf1camera", **args)
        entity.hass = Hass()
        return entity


@pytest.fixture
def camera(monkeypatch):
    module = load(monkeypatch, "camera")
    grabs: list[tuple] = []
    clock = [1000.0]
    # What the next grab does: a picture, None, an exception to raise, or an
    # event to wait for.
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
    cam = Camera(module=module, grabs=grabs, clock=clock, answers=answers)
    cam.entity = cam.make()  # with an interval of ten minutes
    return cam


def snapshot(entity, width=None, height=None):
    return asyncio.run(entity.async_camera_image(width, height))


def test_through_the_bridge_at_most_once_per_interval(camera):
    assert snapshot(camera.entity) == b"from-bridge-1"
    camera.clock[0] += TEN_MINUTES - 1
    assert snapshot(camera.entity) == b"from-bridge-1"
    assert len(camera.grabs) == 1
    camera.clock[0] += 1
    assert snapshot(camera.entity) == b"from-bridge-2"
    source = camera.grabs[0][0]
    assert source.startswith("-rtsp_transport tcp -i rtsp://") and source.endswith("/Nursery")


def test_the_interval_is_the_one_given(camera):
    entity = camera.make(interval=120)
    assert snapshot(entity) == b"from-bridge-1"
    camera.clock[0] += 119
    assert snapshot(entity) == b"from-bridge-1"
    camera.clock[0] += 1
    assert snapshot(entity) == b"from-bridge-2"


@pytest.mark.parametrize("interval", [None, 0], ids=["default", "zero"])
def test_without_an_interval_every_request_goes_through_the_bridge_as_before(camera, interval):
    # The default, so nothing changes on upgrade: no kept picture, Home
    # Assistant's stream is not asked, and ffmpeg scales as it did.
    entity = camera.make(interval=interval)
    stream = FakeStream(idle=[False])
    entity.stream = stream
    assert [snapshot(entity, 320, 180) for _ in range(3)] == [b"from-bridge-1", b"from-bridge-2", b"from-bridge-3"]
    assert [grab[1:] for grab in camera.grabs] == [(320, 180)] * 3
    assert stream.asked == []
    camera.answers.append(RuntimeError("ffmpeg broke"))
    assert snapshot(entity) is None


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


def test_an_unavailable_preloaded_stream_is_not_asked(camera):
    stream = FakeStream(idle=[True], preload=True)
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


def test_a_picture_from_the_stream_is_kept_for_later(camera):
    camera.entity.stream = FakeStream(idle=[False])
    assert snapshot(camera.entity) == b"from-stream"
    camera.entity.stream = FakeStream(idle=[False], image=None)
    assert snapshot(camera.entity) == b"from-stream"
    assert camera.grabs == []


def test_before_the_first_keyframe_the_last_picture_is_kept(camera):
    assert snapshot(camera.entity) == b"from-bridge-1"
    camera.entity.stream = FakeStream(idle=[False], image=None)
    camera.clock[0] += TEN_MINUTES
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


@pytest.mark.parametrize("failure", [RuntimeError("ffmpeg broke"), None, b""], ids=["exception", "no-picture", "empty"])
def test_a_failed_grab_is_retried_after_a_minute(camera, failure):
    assert snapshot(camera.entity) == b"from-bridge-1"
    camera.clock[0] += TEN_MINUTES
    camera.answers.append(failure)
    # The failed grab returns the last picture.
    assert snapshot(camera.entity) == b"from-bridge-1"
    camera.clock[0] += 59
    assert snapshot(camera.entity) == b"from-bridge-1"
    assert len(camera.grabs) == 2
    camera.clock[0] += 1
    assert snapshot(camera.entity) == b"from-bridge-3"


def test_a_grab_that_raises_is_logged(camera, caplog):
    camera.answers.append(RuntimeError("ffmpeg broke"))
    assert snapshot(camera.entity) is None
    assert "ffmpeg snapshot failed" in caplog.text and "ffmpeg broke" in caplog.text


def test_a_first_grab_that_fails_gives_nothing_and_is_retried(camera):
    camera.answers.append(RuntimeError("ffmpeg broke"))
    assert snapshot(camera.entity) is None
    # The pause holds while the camera never had a picture too.
    camera.clock[0] += 59
    assert snapshot(camera.entity) is None
    assert len(camera.grabs) == 1
    camera.clock[0] += 1
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


def test_the_interval_counts_from_the_start_of_a_grab(camera):
    slow = asyncio.Event()
    camera.answers.append(slow)

    async def scenario():
        caller = asyncio.create_task(camera.entity.async_camera_image())
        while not camera.grabs:  # the grab has started and waits for ffmpeg
            await asyncio.sleep(0)
        camera.clock[0] += 5  # the grab took five seconds
        slow.set()
        assert await caller == b"from-bridge-1"
        camera.clock[0] += TEN_MINUTES - 5
        assert await camera.entity.async_camera_image() == b"from-bridge-2"

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


@pytest.mark.parametrize("earlier", [False, True], ids=["no-picture-yet", "earlier-picture"])
def test_removing_the_entity_stops_its_grab(camera, earlier):
    # An unload or a removal of the entity; Home Assistant itself cancels the
    # background task only when it stops. A caller waiting for the grab gets
    # the last picture (or None), not a CancelledError: Home Assistant's
    # _async_get_image suppresses only TimeoutError.
    if earlier:
        assert snapshot(camera.entity) == b"from-bridge-1"
        camera.clock[0] += TEN_MINUTES
    slow = asyncio.Event()
    camera.answers.append(slow)

    async def scenario():
        caller = asyncio.create_task(camera.entity.async_camera_image())
        await asyncio.sleep(0)
        grab = camera.entity._grab
        await camera.entity.async_will_remove_from_hass()
        # A grab left running would never end here: wait a second, not forever.
        done, _ = await asyncio.wait([caller], timeout=1)
        assert done == {caller}
        assert caller.result() == (b"from-bridge-1" if earlier else None)
        assert grab.cancelled()
        # Its picture is not kept.
        slow.set()
        await asyncio.sleep(0)
        assert camera.entity._last_image == (b"from-bridge-1" if earlier else None)

    asyncio.run(scenario())


def test_a_caller_cancelled_with_its_grab_is_still_cancelled(camera):
    # The entity goes while Home Assistant also cancels this call (its
    # timeout, or a stop): the call's own cancellation is not swallowed.
    slow = asyncio.Event()
    camera.answers.append(slow)

    async def scenario():
        caller = asyncio.create_task(camera.entity.async_camera_image())
        await asyncio.sleep(0)
        await camera.entity.async_will_remove_from_hass()
        caller.cancel()
        done, _ = await asyncio.wait([caller], timeout=1)
        assert done == {caller} and caller.cancelled()

    asyncio.run(scenario())


def test_removing_an_entity_without_a_grab_is_fine(camera):
    asyncio.run(camera.entity.async_will_remove_from_hass())
    assert snapshot(camera.entity) == b"from-bridge-1"
    asyncio.run(camera.entity.async_will_remove_from_hass())


@pytest.mark.parametrize(("options", "interval"), [({}, 0), ({"snapshot_interval": 3}, 180), ({"snapshot_interval": 10}, TEN_MINUTES)])
def test_setup_gives_each_camera_the_option_in_seconds(camera, options, interval):
    added = []
    coordinator = SimpleNamespace(camera_name="Nursery", display_name="Nursery", rtsp_path=None, device_info={})
    hass = SimpleNamespace(data={"philips_avent": {"entry": {"coordinators": {"a1": coordinator, "b2": coordinator}}}})
    entry = SimpleNamespace(entry_id="entry", options=options)
    asyncio.run(camera.module.async_setup_entry(hass, entry, added.extend))
    assert [entity._snapshot_interval for entity in added] == [interval, interval]


def _load_config_flow(monkeypatch):
    """The options flow, against the stand-in plus the few names config_flow adds."""
    camera_module = load(monkeypatch, "camera")
    package = camera_module.__package__

    class Flow:
        def __init_subclass__(cls, **kwargs):
            pass

        def async_show_form(self, **kwargs):
            return kwargs

        def async_create_entry(self, **kwargs):
            return kwargs

    def module(name, **attrs):
        mod = types.ModuleType(name)
        mod.__dict__.update(attrs)
        monkeypatch.setitem(sys.modules, name, mod)
        return mod

    entries = module(
        "homeassistant.config_entries",
        ConfigEntry=object,
        ConfigFlow=Flow,
        OptionsFlow=Flow,
        ConfigFlowResult=dict,
        callback=lambda func: func,
    )
    monkeypatch.setattr(sys.modules["homeassistant"], "config_entries", entries, raising=False)
    module("homeassistant.const", **vars(sys.modules["homeassistant.const"]), CONF_COUNTRY="country", CONF_EMAIL="email", CONF_PASSWORD="password")
    module("homeassistant.data_entry_flow", AbortFlow=type("AbortFlow", (Exception,), {}))
    module("homeassistant.helpers.aiohttp_client", async_get_clientsession=lambda hass: None)
    module("homeassistant.helpers.selector", CountrySelector=lambda config: str, CountrySelectorConfig=lambda **kwargs: None)
    import importlib

    return importlib.import_module(f"{package}.config_flow")


def _options_schema(monkeypatch, options):
    config_flow = _load_config_flow(monkeypatch)
    handler = config_flow.PhilipsAventOptionsFlowHandler()
    handler.config_entry = SimpleNamespace(options=options)
    return asyncio.run(handler.async_step_init())["data_schema"]


def test_the_options_offer_the_interval_in_minutes(monkeypatch):
    schema = _options_schema(monkeypatch, {})
    assert schema({})["snapshot_interval"] == 0
    for minutes in (0, 1440):
        assert schema({"snapshot_interval": minutes})["snapshot_interval"] == minutes
    for minutes in (-1, 1441):
        with pytest.raises(vol.Invalid):
            schema({"snapshot_interval": minutes})


def test_the_options_show_the_stored_interval(monkeypatch):
    assert _options_schema(monkeypatch, {"snapshot_interval": 25})({})["snapshot_interval"] == 25
