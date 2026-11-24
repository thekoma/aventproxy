"""The camera's RTSP URL reaches ffmpeg as one argument, whatever the camera's name.

haffmpeg splits the input source with shlex.split, so an unquoted apostrophe
(Baby's room, path Baby's_room) raised "No closing quotation" and every
snapshot failed. Loads the real camera entity against the Home Assistant
stand-in, with ffmpeg replaced by a fake that builds ffmpeg's arguments from the
input as haffmpeg 3.2.2 does (HAFFmpeg._put_input).
"""
from __future__ import annotations

import asyncio
import shlex
from types import SimpleNamespace

import pytest

from .ha_stand_in import home_assistant_installed, load

pytestmark = pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")


def put_input(input_source: str) -> list[str]:
    """haffmpeg's HAFFmpeg._put_input: split tokens if more than one, else "-i" and the original."""
    input_cmd = shlex.split(str(input_source))
    if len(input_cmd) > 1:
        return input_cmd
    return ["-i", input_source]


# The double quotes reached ffmpeg whole without quoting too; that case guards
# the fix. A tab split the URL in two, and an apostrophe made shlex.split raise.
@pytest.mark.parametrize("name", ["Nursery", "Baby's room", 'The "big" room', "Tab\there"])
def test_any_camera_name_reaches_ffmpeg_whole(monkeypatch, name):
    module = load(monkeypatch, "camera")
    argvs: list[list[str]] = []

    async def ffmpeg_get_image(hass, input_source, width=None, height=None):
        argvs.append(put_input(input_source))
        return b"jpeg"

    monkeypatch.setattr(module, "ffmpeg_get_image", ffmpeg_get_image)
    entity = module.AventCamera(SimpleNamespace(camera_name=name, device_info={}), "bf1camera")
    assert asyncio.run(entity.async_camera_image()) == b"jpeg"
    argv = argvs[0]
    assert argv[argv.index("-i") + 1] == entity._stream_url
    assert entity._stream_url.endswith("/" + name.replace(" ", "_"))


def test_snapshot_reads_the_bridge_over_tcp(monkeypatch):
    # With ffmpeg's default transport the media comes over UDP, which a NAT
    # between Home Assistant and the bridge does not deliver.
    module = load(monkeypatch, "camera")
    argvs: list[list[str]] = []

    async def ffmpeg_get_image(hass, input_source, width=None, height=None):
        argvs.append(put_input(input_source))
        return b"jpeg"

    monkeypatch.setattr(module, "ffmpeg_get_image", ffmpeg_get_image)
    entity = module.AventCamera(SimpleNamespace(camera_name="Nursery", display_name="Nursery", rtsp_path=None, device_info={}), "bf1camera")
    assert asyncio.run(entity.async_camera_image()) == b"jpeg"
    argv = argvs[0]
    assert argv[: argv.index("-i")] == ["-rtsp_transport", "tcp"]
