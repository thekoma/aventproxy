#!/usr/bin/env python3
"""Listen to Tuya MQTT and log all messages.

Reuses credentials from the HA config entry — no re-authentication needed.
Trigger DPS changes (night light, volume, lullaby) while this runs to see
what arrives over MQTT.

Usage:
    python tools/mqtt_listener.py [--duration 300]
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

# From HA config entry
SID = "eu16619584946940Nsj46eE38232b7204e8b0bac16f078293db9b175"
ECODE = "11u99u418646946e"
PARTNER = "p1319959"
UID = "eu1661954946940Nj46e"
DEVICE_ID = "51fbc1ec0f3c4340b4dab4a817d79487"


def derive_mqtt_password(ecode: str) -> str:
    md5_key = hashlib.md5(TUYA_SIGNING_KEY.encode()).hexdigest()
    full = hashlib.md5((md5_key + ecode).encode()).hexdigest()
    return full[8:24]


def derive_mqtt_username(sid: str, ecode: str, partner: str) -> str:
    md5_appkey = hashlib.md5(TUYA_APP_KEY.encode()).hexdigest()
    tail = hashlib.md5((md5_appkey + ecode).encode()).hexdigest()[-16:]
    return f"{partner}_v1_{TUYA_APP_KEY}_{TUYA_CH_KEY}_mb_{sid}{tail}"


def derive_mqtt_client_id(uid: str, device_id: str) -> str:
    uid_hash = hashlib.md5((uid + "sdkfasodifca").encode()).hexdigest()
    return f"{TUYA_PACKAGE_NAME}_mb_{device_id}_{uid_hash}_DEFAULT"


def on_connect(client, userdata, flags, reason_code, properties):
    print(f"[CONNECTED] reason_code={reason_code}")
    topics = userdata.get("topics", [])
    for topic in topics:
        print(f"[SUBSCRIBE] {topic}")
        client.subscribe(topic, qos=1)


def on_message(client, userdata, msg):
    ts = time.strftime("%H:%M:%S")
    print(f"\n{'='*60}")
    print(f"[{ts}] TOPIC: {msg.topic}")
    print(f"[{ts}] QOS: {msg.qos}, RETAIN: {msg.retain}")
    try:
        payload = json.loads(msg.payload.decode("utf-8"))
        protocol = payload.get("protocol", "?")
        print(f"[{ts}] PROTOCOL: {protocol}")
        print(json.dumps(payload, indent=2))
    except (json.JSONDecodeError, UnicodeDecodeError):
        print(f"[{ts}] RAW ({len(msg.payload)} bytes): {msg.payload[:200]}")
    print(f"{'='*60}")
    sys.stdout.flush()


def on_disconnect(client, userdata, flags, reason_code, properties):
    print(f"[DISCONNECTED] reason_code={reason_code}")


def on_subscribe(client, userdata, mid, reason_codes, properties):
    print(f"[SUBSCRIBED] mid={mid}, reason_codes={reason_codes}")


def main():
    parser = argparse.ArgumentParser(description="Tuya MQTT listener")
    parser.add_argument("--duration", type=int, default=300, help="Listen duration in seconds (default: 300)")
    parser.add_argument("--topic", choices=["av", "mb", "av_uid", "dev", "all", "none"], default="av", help="Topic: av, av_uid, mb, dev, all, none")
    args = parser.parse_args()

    username = derive_mqtt_username(SID, ECODE, PARTNER)
    password = derive_mqtt_password(ECODE)
    client_id = derive_mqtt_client_id(UID, DEVICE_ID)

    # msid: same derivation as Go bridge
    md5_appkey = hashlib.md5(TUYA_APP_KEY.encode()).hexdigest()
    msid = hashlib.md5((md5_appkey + ECODE).encode()).hexdigest()[-16:]

    topic_mb = f"{PARTNER}/mb/{UID}"
    topic_av_msid = f"/av/u/{msid}"
    topic_av_uid = f"/av/u/{UID}"

    print(f"MQTT Host:      {TUYA_MQTT_HOST}:{TUYA_MQTT_PORT}")
    print(f"Client ID:      {client_id}")
    print(f"Username:       {username[:30]}...")
    print(f"Password:       {password}")
    print(f"Topic (mb):     {topic_mb}")
    print(f"Topic (av_msid): {topic_av_msid}")
    print(f"Topic (av_uid):  {topic_av_uid}")
    print(f"Duration:       {args.duration}s")
    print()

    camera_id = "bf3fb082f7bfac6daaxaw2"
    topic_dev = f"smart/device/out/{camera_id}"

    topic_map = {
        "mb": [topic_mb],
        "av": [topic_av_msid],
        "av_uid": [topic_av_uid],
        "dev": [topic_dev],
        "all": [topic_mb, topic_av_uid, topic_dev],
        "none": [],
    }
    topics = topic_map.get(args.topic, [topic_av_msid])

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

    print(f"Connecting to {TUYA_MQTT_HOST}:{TUYA_MQTT_PORT}...")
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
