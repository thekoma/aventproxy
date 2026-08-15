#!/usr/bin/env python3
"""Listen to Tuya MQTT and log all messages.

Reuses credentials from the HA config entry — no re-authentication needed.
Trigger DPS changes (night light, volume, lullaby) while this runs to see
what arrives over MQTT.

Credentials come from the environment or tools/credentials.json; see
tools/_credentials.py. Nothing here may be hardcoded.

Usage:
    AVENT_SID=... AVENT_ECODE=... python tools/mqtt_listener.py [--duration 300]
"""

import argparse
import hashlib
import json
import ssl
import sys
import time

import paho.mqtt.client as mqtt
from _credentials import MissingCredentials, load_credentials, mask

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
# EU accounts only. The broker for any account is in the `domain` block of
# the login / smartlife.m.user.info.get response (`mobileMqttsUrl`).
TUYA_MQTT_HOST = "m1.tuyaeu.com"
TUYA_MQTT_PORT = 8883

# Credentials are loaded at runtime in main(); see the module docstring.

TOPICS_NEEDING_CAMERA_ID = ("dev", "all")


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

    wanted = ["sid", "ecode", "partner", "uid", "device_id"]
    if args.topic in TOPICS_NEEDING_CAMERA_ID:
        wanted.append("camera_id")
    try:
        creds = load_credentials(*wanted)
    except MissingCredentials as err:
        sys.exit(str(err))

    username = derive_mqtt_username(creds["sid"], creds["ecode"], creds["partner"])
    password = derive_mqtt_password(creds["ecode"])
    client_id = derive_mqtt_client_id(creds["uid"], creds["device_id"])

    # msid: same derivation as Go bridge
    md5_appkey = hashlib.md5(TUYA_APP_KEY.encode()).hexdigest()
    msid = hashlib.md5((md5_appkey + creds["ecode"]).encode()).hexdigest()[-16:]

    topic_mb = f"{creds['partner']}/mb/{creds['uid']}"
    topic_av_msid = f"/av/u/{msid}"
    topic_av_uid = f"/av/u/{creds['uid']}"

    print(f"MQTT Host:      {TUYA_MQTT_HOST}:{TUYA_MQTT_PORT}")
    print(f"Client ID:      {client_id}")
    print(f"Username:       {mask(username, 12)}")
    print(f"Password:       {mask(password)}")
    print(f"Topic (mb):     {topic_mb}")
    print(f"Topic (av_msid): {topic_av_msid}")
    print(f"Topic (av_uid):  {topic_av_uid}")
    print(f"Duration:       {args.duration}s")
    print()

    camera_id = creds.get("camera_id", "")
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
