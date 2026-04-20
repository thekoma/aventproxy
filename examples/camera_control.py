#!/usr/bin/env python3
"""
Camera control examples — night light, lullabies, temperature, alerts.

Requires a valid SID (run login_example.py first).

Environment variables:
    TUYA_SIGNING_KEY
    TUYA_APP_KEY
    TUYA_SID
    TUYA_CAM_ID      — Camera device ID
"""

import json
import os
import sys

sys.path.insert(0, os.path.dirname(__file__))
from tuya_client import TuyaClient

client = TuyaClient(
    signing_key=os.environ["TUYA_SIGNING_KEY"],
    app_key=os.environ["TUYA_APP_KEY"],
    sid=os.environ["TUYA_SID"],
)
CAM = os.environ["TUYA_CAM_ID"]


def show_status():
    """Print current camera status."""
    dev = client.get_device(CAM)
    dps = dev["dps"]
    print(f"Camera: {dev['name']}  Online: {dev['isOnline']}")
    print(f"  Temperature:    {dps['207'] / 100:.1f}°C")
    print(f"  Night light:    {'ON' if dps['138'] else 'OFF'} (brightness {dps['158']}%)")
    print(f"  Lullaby:        {dps['246']} (volume {dps['209']}%)")
    print(f"  Motion alert:   {'ON' if dps['134'] else 'OFF'} (sensitivity {dps['106']})")
    print(f"  Sound alert:    {'ON' if dps['139'] else 'OFF'} (sensitivity {dps['140']})")
    print(f"  Privacy mode:   {'ON' if dps['237'] == '1' else 'OFF'}")
    print(f"  Power:          {'Plugged' if dps['205'] == '1' else 'Battery'}")
    print(f"  Talkback:       app={dps['253']} pu={dps['252']}")


def night_light(on: bool, brightness: int = 50):
    """Control the night light."""
    dps = {"138": on}
    if on:
        dps["158"] = max(1, min(100, brightness))
    client.set_dps(CAM, dps)
    print(f"Night light {'ON' if on else 'OFF'}" + (f" at {brightness}%" if on else ""))


def night_light_timer(seconds: int):
    """Set night light auto-off timer."""
    client.set_dps(CAM, {"241": True, "240": seconds})
    print(f"Night light timer: {seconds // 60}m {seconds % 60}s")


def lullaby(action: str, volume: int = None, timer_seconds: int = None):
    """Control lullaby playback.

    action: play, pause, stop, next, prev
    """
    dps = {"201": action}
    if volume is not None:
        dps["209"] = max(1, min(100, volume))
    if timer_seconds is not None:
        dps["243"] = True
        dps["244"] = timer_seconds
    client.set_dps(CAM, dps)
    print(f"Lullaby: {action}" +
          (f" volume={volume}%" if volume else "") +
          (f" timer={timer_seconds}s" if timer_seconds else ""))


def alerts(motion: bool = None, sound: bool = None,
           motion_sensitivity: str = None, sound_sensitivity: str = None):
    """Configure motion and sound alerts."""
    dps = {}
    if motion is not None:
        dps["134"] = motion
    if sound is not None:
        dps["139"] = sound
    if motion_sensitivity is not None:
        dps["106"] = motion_sensitivity  # "0"=off, "1"=low, "2"=high
    if sound_sensitivity is not None:
        dps["140"] = sound_sensitivity
    if dps:
        client.set_dps(CAM, dps)
        print(f"Alerts updated: {dps}")


def get_webrtc_config():
    """Get WebRTC streaming configuration."""
    config = client.get_rtc_config(CAM)
    print(f"WebRTC session: {config['p2pConfig']['session']['sessionId']}")
    print(f"STUN/TURN servers:")
    for ice in config["p2pConfig"]["ices"]:
        print(f"  {ice['urls']}")
    print(f"AES key: {config['p2pConfig']['session']['aesKey']}")
    print(f"Expires: {config['p2pConfig']['expire']}")
    print(f"Video qualities: {config.get('vedioClaritys', [])}")
    return config


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Camera control")
    sub = parser.add_subparsers(dest="cmd")

    sub.add_parser("status")

    p = sub.add_parser("light")
    p.add_argument("state", choices=["on", "off"])
    p.add_argument("--brightness", type=int, default=50)
    p.add_argument("--timer", type=int, help="Auto-off in seconds")

    p = sub.add_parser("lullaby")
    p.add_argument("action", choices=["play", "pause", "stop", "next", "prev"])
    p.add_argument("--volume", type=int)
    p.add_argument("--timer", type=int, help="Auto-stop in seconds")

    p = sub.add_parser("alerts")
    p.add_argument("--motion", choices=["on", "off"])
    p.add_argument("--sound", choices=["on", "off"])
    p.add_argument("--sensitivity", choices=["0", "1", "2"])

    sub.add_parser("webrtc")
    sub.add_parser("temperature")

    args = parser.parse_args()

    if args.cmd == "status":
        show_status()
    elif args.cmd == "light":
        night_light(args.state == "on", args.brightness)
        if args.timer:
            night_light_timer(args.timer)
    elif args.cmd == "lullaby":
        lullaby(args.action, args.volume, args.timer)
    elif args.cmd == "alerts":
        alerts(
            motion=args.motion == "on" if args.motion else None,
            sound=args.sound == "on" if args.sound else None,
            motion_sensitivity=args.sensitivity,
            sound_sensitivity=args.sensitivity,
        )
    elif args.cmd == "webrtc":
        get_webrtc_config()
    elif args.cmd == "temperature":
        dev = client.get_device(CAM)
        print(f"{dev['dps']['207'] / 100:.1f}°C")
    else:
        parser.print_help()
