"""Configuration loading for Avent RTSP Proxy."""

import argparse
import json
import logging
from dataclasses import dataclass, field
from pathlib import Path

from .server import CameraTarget

HA_OPTIONS = Path("/data/options.json")

log = logging.getLogger(__name__)


@dataclass
class ProxyConfig:
    cameras: list[CameraTarget] = field(default_factory=list)
    bind_address: str = "127.0.0.1"
    port: int = 8554
    log_level: str = "info"

    def validate(self):
        if not self.cameras:
            raise ValueError("At least one camera must be configured")
        names = [c.name for c in self.cameras]
        if len(names) != len(set(names)):
            raise ValueError("Camera names must be unique")
        for cam in self.cameras:
            if not cam.name:
                raise ValueError("Camera name cannot be empty")
            if not cam.host:
                raise ValueError(f"Camera '{cam.name}': host cannot be empty")
            if not 1 <= cam.port <= 65535:
                raise ValueError(
                    f"Camera '{cam.name}': port must be 1-65535, got {cam.port}"
                )
        if not 1 <= self.port <= 65535:
            raise ValueError(f"Proxy port must be 1-65535, got {self.port}")


def load_config() -> ProxyConfig:
    if HA_OPTIONS.exists():
        log.info("Loading config from Home Assistant options")
        return _load_ha()
    return _load_cli()


def _load_ha() -> ProxyConfig:
    data = json.loads(HA_OPTIONS.read_text())
    cameras = [
        CameraTarget(
            name=c["name"],
            host=c["host"],
            port=c.get("port", 554),
        )
        for c in data.get("cameras", [])
    ]
    return ProxyConfig(
        cameras=cameras,
        bind_address=data.get("bind_address", "0.0.0.0"),
        port=8554,
        log_level=data.get("log_level", "info"),
    )


def load_from_dict(data: dict) -> ProxyConfig:
    cameras = [
        CameraTarget(
            name=c["name"],
            host=c["host"],
            port=c.get("port", 554),
        )
        for c in data.get("cameras", [])
    ]
    return ProxyConfig(
        cameras=cameras,
        bind_address=data.get("bind_address", "127.0.0.1"),
        port=data.get("port", 8554),
        log_level=data.get("log_level", "info"),
    )


def _load_cli() -> ProxyConfig:
    parser = argparse.ArgumentParser(
        description="RTSP proxy for Tuya cameras that reject OPTIONS",
    )
    parser.add_argument(
        "-c", "--config",
        type=Path,
        help="Path to JSON config file",
    )
    parser.add_argument(
        "--camera",
        action="append",
        metavar="NAME:HOST[:PORT]",
        help="Camera target (repeatable). Example: baby:192.168.1.100:554",
    )
    parser.add_argument(
        "--bind", default="127.0.0.1",
        help="Bind address (default: 127.0.0.1)",
    )
    parser.add_argument(
        "--port", type=int, default=8554,
        help="Listen port (default: 8554)",
    )
    parser.add_argument(
        "--log-level", default="info",
        choices=["debug", "info", "warning", "error"],
    )

    args = parser.parse_args()

    if args.config:
        data = json.loads(args.config.read_text())
        cfg = load_from_dict(data)
        if args.bind != "127.0.0.1":
            cfg.bind_address = args.bind
        if args.port != 8554:
            cfg.port = args.port
        if args.log_level != "info":
            cfg.log_level = args.log_level
        return cfg

    if not args.camera:
        parser.error("Provide --config or at least one --camera")

    cameras = []
    for spec in args.camera:
        parts = spec.split(":")
        if len(parts) == 2:
            cameras.append(CameraTarget(name=parts[0], host=parts[1]))
        elif len(parts) == 3:
            cameras.append(
                CameraTarget(
                    name=parts[0], host=parts[1], port=int(parts[2]),
                )
            )
        else:
            parser.error(f"Invalid camera spec: {spec}")

    return ProxyConfig(
        cameras=cameras,
        bind_address=args.bind,
        port=args.port,
        log_level=args.log_level,
    )
