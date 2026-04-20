"""Tests for configuration loading and validation."""


import pytest
from proxy.config import ProxyConfig, load_from_dict
from proxy.server import CameraTarget


class TestProxyConfigValidation:
    def test_valid_config(self):
        cfg = ProxyConfig(
            cameras=[CameraTarget(name="cam1", host="192.168.1.1")],
        )
        cfg.validate()

    def test_empty_cameras(self):
        cfg = ProxyConfig(cameras=[])
        with pytest.raises(ValueError, match="(?i)at least one camera"):
            cfg.validate()

    def test_duplicate_names(self):
        cfg = ProxyConfig(
            cameras=[
                CameraTarget(name="cam", host="1.1.1.1"),
                CameraTarget(name="cam", host="2.2.2.2"),
            ],
        )
        with pytest.raises(ValueError, match="unique"):
            cfg.validate()

    def test_empty_name(self):
        cfg = ProxyConfig(
            cameras=[CameraTarget(name="", host="1.1.1.1")],
        )
        with pytest.raises(ValueError, match="name cannot be empty"):
            cfg.validate()

    def test_empty_host(self):
        cfg = ProxyConfig(
            cameras=[CameraTarget(name="cam", host="")],
        )
        with pytest.raises(ValueError, match="host cannot be empty"):
            cfg.validate()

    def test_invalid_camera_port(self):
        cfg = ProxyConfig(
            cameras=[CameraTarget(name="cam", host="1.1.1.1", port=0)],
        )
        with pytest.raises(ValueError, match="port must be"):
            cfg.validate()

    def test_invalid_proxy_port(self):
        cfg = ProxyConfig(
            cameras=[CameraTarget(name="cam", host="1.1.1.1")],
            port=99999,
        )
        with pytest.raises(ValueError, match="Proxy port"):
            cfg.validate()


class TestLoadFromDict:
    def test_minimal(self):
        data = {
            "cameras": [{"name": "baby", "host": "192.168.1.100"}],
        }
        cfg = load_from_dict(data)
        assert len(cfg.cameras) == 1
        assert cfg.cameras[0].name == "baby"
        assert cfg.cameras[0].host == "192.168.1.100"
        assert cfg.cameras[0].port == 554

    def test_full(self):
        data = {
            "cameras": [
                {"name": "cam1", "host": "10.0.0.1", "port": 8554},
                {"name": "cam2", "host": "10.0.0.2"},
            ],
            "bind_address": "0.0.0.0",
            "port": 9000,
            "log_level": "debug",
        }
        cfg = load_from_dict(data)
        assert len(cfg.cameras) == 2
        assert cfg.cameras[0].port == 8554
        assert cfg.cameras[1].port == 554
        assert cfg.bind_address == "0.0.0.0"
        assert cfg.port == 9000
        assert cfg.log_level == "debug"

    def test_defaults(self):
        data = {"cameras": [{"name": "x", "host": "1.1.1.1"}]}
        cfg = load_from_dict(data)
        assert cfg.bind_address == "127.0.0.1"
        assert cfg.port == 8554
        assert cfg.log_level == "info"
