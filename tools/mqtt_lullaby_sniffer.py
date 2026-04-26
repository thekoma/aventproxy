#!/usr/bin/env python3
"""Bidirectional MQTT sniffer for lullaby command capture.

Subscribes to ALL /av/ topics with wildcards to capture both:
  - App → Device messages (play/stop commands)
  - Device → App messages (state updates)

Run this WHILE triggering play/stop in the Philips Avent app.

Usage:
    python tools/mqtt_lullaby_sniffer.py [--duration 300]
"""

import argparse
import hashlib
import json
import ssl
import sys
import time

import paho.mqtt.client as mqtt

TUYA_SIGNING_KEY = (
    "com.philips.ph.babymonitorplus"
    "_D2:D6:95:A1:1D:1B:84:F9:25:A9:45:6E:27:F4:45:E9:FD:87:C3:74"
    ":63:AA:8A:34:32:A6:6A:23:3B:0F:D5:0F"
    "_8n459nxk9g98gqgcwrpk3csv97uuwajm"
    "_a3nfht4ufwfw9cmkspaftv4x89cx58qx"
)
TUYA_APP_KEY = "wx3at9qprkhskvkcsyhm"
TUYA_PACKAGE_NAME = "com.philips.ph.babymonitorplus"
TUYA_CH_KEY = "071d81fa"
TUYA_MQTT_HOST = "m1.tuyaeu.com"
TUYA_MQTT_PORT = 8883

SID = "eu16619584946940Nsj46eE38232b7204e8b0bac16f078293db9b175"
ECODE = "11u99u418646946e"
PARTNER = "p1319959"
UID = "eu1661954946940Nj46e"
DEVICE_ID = "51fbc1ec0f3c4340b4dab4a817d79487"
CAMERA_ID = "bf3fb082f7bfac6daaxaw2"

LULLABY_KEYWORDS = {"201", "play", "stop", "pause", "next", "prev",
                     "lullaby", "music", "play_control", "play_state",
                     "246", "248", "202", "203", "209"}


def derive_mqtt_password(ecode: str) -> str:
    md5_key = hashlib.md5(TUYA_SIGNING_KEY.encode()).hexdigest()
    return hashlib.md5((md5_key + ecode).encode()).hexdigest()[8:24]


def derive_mqtt_username(sid: str, ecode: str, partner: str) -> str:
    md5_appkey = hashlib.md5(TUYA_APP_KEY.encode()).hexdigest()
    tail = hashlib.md5((md5_appkey + ecode).encode()).hexdigest()[-16:]
    return f"{partner}_v1_{TUYA_APP_KEY}_{TUYA_CH_KEY}_mb_{sid}{tail}"


def derive_mqtt_client_id(uid: str, device_id: str) -> str:
    uid_hash = hashlib.md5((uid + "sdkfasodifca").encode()).hexdigest()
    return f"{TUYA_PACKAGE_NAME}_mb_{device_id}_{uid_hash}_SNIFFER"


def is_lullaby_related(text: str) -> bool:
    lower = text.lower()
    return any(kw in lower for kw in LULLABY_KEYWORDS)


def on_connect(client, userdata, flags, reason_code, properties):
    print(f"[CONNECTED] reason_code={reason_code}")
    topics = userdata["topics"]
    for topic in topics:
        print(f"[SUBSCRIBE] {topic}")
        client.subscribe(topic, qos=1)


def on_message(client, userdata, msg):
    ts = time.strftime("%H:%M:%S.") + f"{time.time() % 1:.3f}"[2:]
    raw = msg.payload

    try:
        payload = json.loads(raw.decode("utf-8"))
    except (json.JSONDecodeError, UnicodeDecodeError):
        payload = None

    payload_str = json.dumps(payload, indent=2) if payload else repr(raw[:500])
    lullaby = is_lullaby_related(payload_str)
    marker = " *** LULLABY ***" if lullaby else ""

    direction = "???"
    topic = msg.topic
    if "/av/moto/" in topic:
        direction = "APP->DEV"
    elif "/av/u/" in topic:
        direction = "DEV->APP"
    elif "smart/device" in topic:
        direction = "CLOUD->APP"
    elif topic.startswith(PARTNER):
        direction = "CLOUD-MB"

    print(f"\n{'='*70}")
    print(f"[{ts}] [{direction}]{marker}")
    print(f"  TOPIC: {topic}")
    print(f"  QOS: {msg.qos}, RETAIN: {msg.retain}, SIZE: {len(raw)}b")

    if payload:
        protocol = payload.get("protocol", "?")
        pv = payload.get("pv", "?")
        print(f"  PROTOCOL: {protocol}, PV: {pv}")

        data = payload.get("data", {})
        header = data.get("header", {})
        if header:
            print(f"  TYPE: {header.get('type', '?')}")
            print(f"  FROM: {header.get('from', '?')}")
            print(f"  TO:   {header.get('to', '?')}")
            print(f"  SID:  {header.get('sessionid', '?')[:16]}...")

        msg_data = data.get("msg")
        if msg_data:
            if isinstance(msg_data, str):
                try:
                    msg_data = json.loads(msg_data)
                except json.JSONDecodeError:
                    pass
            print(f"  MSG: {json.dumps(msg_data, indent=4) if isinstance(msg_data, dict) else msg_data}")
    else:
        print(f"  RAW: {raw[:500]}")

    print(f"{'='*70}")
    sys.stdout.flush()


def on_disconnect(client, userdata, flags, reason_code, properties):
    print(f"[DISCONNECTED] reason_code={reason_code}")


def on_subscribe(client, userdata, mid, reason_codes, properties):
    print(f"[SUBSCRIBED] mid={mid}, reason_codes={reason_codes}")


def main():
    parser = argparse.ArgumentParser(description="MQTT lullaby command sniffer")
    parser.add_argument("--duration", type=int, default=600, help="Listen duration (default: 600s)")
    args = parser.parse_args()

    username = derive_mqtt_username(SID, ECODE, PARTNER)
    password = derive_mqtt_password(ECODE)
    client_id = derive_mqtt_client_id(UID, DEVICE_ID)

    md5_appkey = hashlib.md5(TUYA_APP_KEY.encode()).hexdigest()
    msid = hashlib.md5((md5_appkey + ECODE).encode()).hexdigest()[-16:]

    topics = [
        f"/av/u/{msid}",                          # Device -> App (via msid)
        f"/av/u/{UID}",                            # Device -> App (via uid)
        f"{PARTNER}/mb/{UID}",                     # Cloud push
        f"smart/device/out/{CAMERA_ID}",           # Device cloud events
        "/av/moto/+/u/" + CAMERA_ID,               # App -> Device (wildcard motoId)
        "/av/moto/+/u/" + DEVICE_ID,               # App -> Device (alt ID)
        "/av/#",                                   # Catch-all AV
    ]

    print("=" * 70)
    print("MQTT LULLABY SNIFFER")
    print("=" * 70)
    print(f"Host:       {TUYA_MQTT_HOST}:{TUYA_MQTT_PORT}")
    print(f"Client ID:  {client_id[:40]}...")
    print(f"Duration:   {args.duration}s")
    print("\nTopics:")
    for t in topics:
        print(f"  - {t}")
    print(f"\nWatching for keywords: {', '.join(sorted(LULLABY_KEYWORDS))}")
    print("=" * 70)
    print()

    client = mqtt.Client(
        client_id=client_id,
        protocol=mqtt.MQTTv311,
        callback_api_version=mqtt.CallbackAPIVersion.VERSION2,
        userdata={"topics": topics},
    )
    client.username_pw_set(username, password)
    client.tls_set(tls_version=ssl.PROTOCOL_TLSv1_2)

    client.on_connect = on_connect
    client.on_message = on_message
    client.on_disconnect = on_disconnect
    client.on_subscribe = on_subscribe

    print("Connecting...")
    client.connect(TUYA_MQTT_HOST, TUYA_MQTT_PORT, keepalive=60)

    client.loop_start()
    try:
        deadline = time.monotonic() + args.duration
        while time.monotonic() < deadline:
            time.sleep(1)
    except KeyboardInterrupt:
        print("\n[INTERRUPTED]")
    finally:
        client.loop_stop()
        client.disconnect()
        print("[DONE]")


if __name__ == "__main__":
    main()
