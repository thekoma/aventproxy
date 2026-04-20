#!/usr/bin/env python3
import os
"""Tuya Mobile SDK API client using Frida RPC for request signing."""

import frida
import json
import sys
import time
import uuid
import requests

BASE_URL = "https://a1.tuyaeu.com/api.json"
APP_KEY = os.environ.get("TUYA_APP_KEY", "YOUR_APP_KEY")
SID = os.environ.get("TUYA_SID", "YOUR_SID")
DEVICE_ID = os.environ.get("TUYA_DEVICE_ID", "YOUR_PHONE_DEVICE_ID")

class TuyaAPI:
    def __init__(self):
        self.device = frida.get_usb_device()
        self.session = None
        self.script = None
        self._attach()

    def _attach(self):
        # Find the app process
        for proc in self.device.enumerate_processes():
            if ("babymonitorplus" in proc.name or "Baby Monitor" in proc.name) and "monitor" not in proc.name:
                pid = proc.pid
                break
        else:
            raise RuntimeError("Baby Monitor+ app not running")

        print(f"[*] Attaching to PID {pid}...")
        self.session = self.device.attach(pid)

        with open("/home/koma/src/babymonitor/sign_proxy.js") as f:
            script_code = f.read()

        self.script = self.session.create_script(script_code, runtime="v8")
        self.script.on("message", lambda msg, data: print(f"[frida] {msg}"))
        self.script.load()

        # Wait for Java to be ready
        import time as _time
        for _ in range(10):
            pong = self.script.exports_sync.ping()
            if pong == "ready":
                break
            _time.sleep(1)
        print(f"[*] Frida RPC status: {pong}")

    def sign(self, params):
        return self.script.exports_sync.sign(json.dumps(params))

    def call(self, action, version="1.0", post_data=None):
        t = str(int(time.time()))
        params = {
            "a": action,
            "appVersion": "1.8.0",
            "appRnVersion": "5.92",
            "channel": "oem",
            "chKey": "071d81fa",
            "clientId": APP_KEY,
            "cp": "gzip",
            "deviceCoreVersion": "6.7.0",
            "deviceId": DEVICE_ID,
            "et": "0.0.1",
            "lang": "en_US",
            "nd": "1",
            "os": "Android",
            "osSystem": "14",
            "platform": "LE2113",
            "requestId": str(uuid.uuid4()),
            "sdkVersion": "6.7.0",
            "sid": SID,
            "time": t,
            "timeZoneId": "Europe/Rome",
            "ttid": f"sdk_international@{APP_KEY}",
            "v": version,
        }

        if post_data:
            params["postData"] = json.dumps(post_data) if isinstance(post_data, dict) else post_data

        # Sign the request via Frida RPC
        sign = self.sign(params)
        if sign.startswith("ERROR"):
            print(f"[!] Sign error: {sign}")
            return None

        params["sign"] = sign

        r = requests.post(BASE_URL, data=params, headers={
            "User-Agent": "Thing-UA=APP/Android/1.8.0/SDK/6.7.0",
            "Content-Type": "application/x-www-form-urlencoded",
        })
        return r.json()


def main():
    api = TuyaAPI()

    # 1. Get time (simple test)
    print("\n=== smartlife.p.time.get ===")
    resp = api.call("smartlife.p.time.get")
    print(json.dumps(resp, indent=2))

    if not resp or not resp.get("success"):
        print("[!] time.get failed, checking sign...")
        return

    # 2. Get device info
    print("\n=== tuya.m.device.get ===")
    resp = api.call("tuya.m.device.get", post_data={"devId": os.environ.get("TUYA_CAM_ID", "YOUR_CAMERA_ID")})
    print(json.dumps(resp, indent=2))

    # 3. Get home list
    print("\n=== m.life.home.space.list ===")
    resp = api.call("m.life.home.space.list")
    print(json.dumps(resp, indent=2))

    # 4. Get RTC config (WebRTC)
    print("\n=== smartlife.m.rtc.config.get ===")
    resp = api.call("smartlife.m.rtc.config.get", post_data={"devId": os.environ.get("TUYA_CAM_ID", "YOUR_CAMERA_ID")})
    print(json.dumps(resp, indent=2))

    # 5. Get P2P pre-link
    print("\n=== smartlife.m.p2p.main.pre.link.get ===")
    resp = api.call("smartlife.m.p2p.main.pre.link.get", post_data={"devId": os.environ.get("TUYA_CAM_ID", "YOUR_CAMERA_ID")})
    print(json.dumps(resp, indent=2))

    # 6. Get MQTT token
    print("\n=== smartlife.m.token.get ===")
    resp = api.call("smartlife.m.token.get")
    print(json.dumps(resp, indent=2))


if __name__ == "__main__":
    main()
