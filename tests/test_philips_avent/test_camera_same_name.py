"""Same-named cameras each pull their own stream from the bridge.

Loads the real camera entity against the Home Assistant stand-in, with the
paths from const.rtsp_paths as async_setup_entry sets them.
"""
from __future__ import annotations

import sys
from types import SimpleNamespace

import pytest

from .ha_stand_in import home_assistant_installed, load

pytestmark = pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")


def test_same_named_cameras_pull_their_own_stream(monkeypatch):
    # Two monitors left as "Philips Avent Baby Monitor": the bridge serves the
    # second under a suffixed path, and its camera entity must pull that one.
    module = load(monkeypatch, "camera")
    const = sys.modules[f"{module.__package__}.const"]
    name = "Philips Avent Baby Monitor"
    paths = const.rtsp_paths([("a1b2c3d4e5", name), ("f6e5d4c3b2", name)])
    urls = [
        module.AventCamera(
            SimpleNamespace(camera_name=name, display_name=name, rtsp_path=paths[cam_id], device_info={}),
            cam_id, 8554, "bridge",
        )._stream_url
        for cam_id in ("a1b2c3d4e5", "f6e5d4c3b2")
    ]
    assert urls == [
        "rtsp://bridge:8554/Philips_Avent_Baby_Monitor",
        "rtsp://bridge:8554/Philips_Avent_Baby_Monitor_f6e5d4",
    ]
