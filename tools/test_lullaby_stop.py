#!/usr/bin/env python3
"""Test lullaby stop with different Tuya LAN protocol variants.

From APK decompilation we found:
  - FrameTypeEnum.CONTROL = 7 (protocol 3.3)
  - FrameTypeEnum.CONTROL_NEW = 13 (protocol 3.4+, cadv >= 1.0.1)
  - The app may use CONTROL_NEW with extra fields (gwId, cid, t, uid)

This script tries multiple approaches to stop the lullaby.

Usage:
    # First start a lullaby from the Philips Avent app, then:
    python tools/test_lullaby_stop.py [--ip 192.168.85.90]
"""

import argparse
import json
import sys
import time

import tinytuya

DEV_ID = "bf3fb082f7bfac6daaxaw2"
LOCAL_KEY = '9V-_a|mW^ZkY6u71'
DEFAULT_IP = "192.168.85.90"


def scan_for_device():
    """Find device IP via broadcast scan."""
    print("[SCAN] Scanning LAN for device...")
    devices = tinytuya.deviceScan(maxretry=5)
    for ip, info in devices.items():
        if info.get("gwId") == DEV_ID:
            print(f"[SCAN] Found at {ip}")
            return ip
    return None


def read_current_state(d):
    """Read current DPS state."""
    d.updatedps([201, 202, 203, 209, 246, 248])
    time.sleep(0.5)
    data = d.receive()
    if data and isinstance(data, dict) and data.get("dps"):
        print(f"  Current DPS: {json.dumps(data['dps'], indent=2)}")
        return data["dps"]
    print("  (no DPS response)")
    return {}


def drain_responses(d, timeout=2.0):
    """Read all pending responses."""
    responses = []
    deadline = time.time() + timeout
    while time.time() < deadline:
        data = d.receive()
        if data and isinstance(data, dict):
            if "dps" in data:
                responses.append(data["dps"])
                print(f"  Response DPS: {data['dps']}")
            elif "Error" in data or "Err" in data:
                print(f"  Error: {data}")
            else:
                print(f"  Other: {data}")
        else:
            break
    return responses


def test_v33_control(ip):
    """Test 1: Protocol 3.3, CONTROL (type 7) — baseline."""
    print("\n" + "=" * 60)
    print("TEST 1: Protocol 3.3, CONTROL (type 7) — set_value")
    print("=" * 60)
    d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
    d.set_socketPersistent(True)
    d.set_socketTimeout(3)

    print("  Reading current state...")
    read_current_state(d)

    print("  Sending DPS 201='stop' via set_value (CONTROL)...")
    result = d.set_value("201", "stop")
    print(f"  Result: {result}")
    drain_responses(d)
    d.close()


def test_v33_set_multiple(ip):
    """Test 2: Protocol 3.3, send multiple DPS at once."""
    print("\n" + "=" * 60)
    print("TEST 2: Protocol 3.3, multiple DPS — set_multiple_values")
    print("=" * 60)
    d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
    d.set_socketPersistent(True)
    d.set_socketTimeout(3)

    print("  Sending DPS 201='stop' + 246='stopping'...")
    result = d.set_multiple_values({"201": "stop", "246": "stopping"})
    print(f"  Result: {result}")
    drain_responses(d)
    d.close()


def test_v34_control_new(ip):
    """Test 3: Protocol 3.4, CONTROL_NEW (type 13)."""
    print("\n" + "=" * 60)
    print("TEST 3: Protocol 3.4, CONTROL_NEW (type 13)")
    print("=" * 60)
    d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.4)
    d.set_socketPersistent(True)
    d.set_socketTimeout(3)

    print("  Sending DPS 201='stop' via set_value (CONTROL_NEW)...")
    result = d.set_value("201", "stop")
    print(f"  Result: {result}")
    drain_responses(d)
    d.close()


def test_v35_control_new(ip):
    """Test 4: Protocol 3.5."""
    print("\n" + "=" * 60)
    print("TEST 4: Protocol 3.5")
    print("=" * 60)
    d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.5)
    d.set_socketPersistent(True)
    d.set_socketTimeout(3)

    print("  Sending DPS 201='stop' via set_value...")
    result = d.set_value("201", "stop")
    print(f"  Result: {result}")
    drain_responses(d)
    d.close()


def test_raw_control_new_v33(ip):
    """Test 5: Protocol 3.3 but with raw CONTROL_NEW frame type 13."""
    print("\n" + "=" * 60)
    print("TEST 5: Protocol 3.3 with raw CONTROL_NEW (type 13)")
    print("=" * 60)
    d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
    d.set_socketPersistent(True)
    d.set_socketTimeout(3)

    payload = d.generate_payload(tinytuya.CONTROL_NEW, {"201": "stop"})
    print("  Sending raw CONTROL_NEW payload...")
    d._send_receive(payload)
    drain_responses(d)
    d.close()


def test_raw_with_extra_fields(ip):
    """Test 6: CONTROL_NEW with extra fields like the SDK sends."""
    print("\n" + "=" * 60)
    print("TEST 6: Protocol 3.3 CONTROL_NEW + extra fields (gwId, t)")
    print("=" * 60)
    d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
    d.set_socketPersistent(True)
    d.set_socketTimeout(3)

    t = int(time.time())
    payload_data = {
        "devId": DEV_ID,
        "gwId": DEV_ID,
        "dps": {"201": "stop"},
        "t": t,
    }
    payload_json = json.dumps(payload_data)
    print(f"  Payload: {payload_json}")

    payload = d.generate_payload(tinytuya.CONTROL_NEW, {"201": "stop"})
    print("  Sending...")
    d._send_receive(payload)
    drain_responses(d)
    d.close()


def test_dp_query_then_stop(ip):
    """Test 7: Query DPS first (like the app does), then stop."""
    print("\n" + "=" * 60)
    print("TEST 7: DP_QUERY first, then CONTROL stop")
    print("=" * 60)
    d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
    d.set_socketPersistent(True)
    d.set_socketTimeout(3)

    print("  Querying current DPS state...")
    status = d.status()
    print(f"  Status: {status}")

    print("  Sending DPS 201='stop'...")
    result = d.set_value("201", "stop")
    print(f"  Result: {result}")
    drain_responses(d)
    d.close()


def test_play_then_stop(ip):
    """Test 8: Send play first, wait, then stop — simulate app flow."""
    print("\n" + "=" * 60)
    print("TEST 8: Full cycle — play, wait 3s, stop")
    print("=" * 60)
    d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
    d.set_socketPersistent(True)
    d.set_socketTimeout(3)

    print("  Sending DPS 201='play'...")
    result = d.set_value("201", "play")
    print(f"  Result: {result}")
    drain_responses(d, timeout=1)

    print("  Waiting 3 seconds...")
    time.sleep(3)

    print("  Sending DPS 201='stop'...")
    result = d.set_value("201", "stop")
    print(f"  Result: {result}")
    drain_responses(d)
    d.close()


def test_integer_dps(ip):
    """Test 9: Try with integer DPS key instead of string."""
    print("\n" + "=" * 60)
    print("TEST 9: Integer DPS key {201: 'stop'}")
    print("=" * 60)
    d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
    d.set_socketPersistent(True)
    d.set_socketTimeout(3)

    print("  Sending DPS {201: 'stop'} with integer key...")
    result = d.set_value(201, "stop")
    print(f"  Result: {result}")
    drain_responses(d)
    d.close()


def main():
    parser = argparse.ArgumentParser(description="Test lullaby stop commands")
    parser.add_argument("--ip", default=None, help=f"Device IP (default: scan or {DEFAULT_IP})")
    parser.add_argument("--test", type=int, default=0, help="Run specific test (1-9), 0=all")
    parser.add_argument("--scan", action="store_true", help="Scan for device first")
    args = parser.parse_args()

    ip = args.ip
    if not ip:
        if args.scan:
            ip = scan_for_device()
        if not ip:
            ip = DEFAULT_IP
            print(f"[INFO] Using default IP: {ip}")

    print(f"[INFO] Device: {DEV_ID}")
    print(f"[INFO] IP: {ip}")
    print(f"[INFO] Key: {LOCAL_KEY}")

    tests = {
        1: test_v33_control,
        2: test_v33_set_multiple,
        3: test_v34_control_new,
        4: test_v35_control_new,
        5: test_raw_control_new_v33,
        6: test_raw_with_extra_fields,
        7: test_dp_query_then_stop,
        8: test_play_then_stop,
        9: test_integer_dps,
    }

    if args.test > 0:
        test_fn = tests.get(args.test)
        if test_fn:
            try:
                test_fn(ip)
            except Exception as e:
                print(f"  ERROR: {e}")
        else:
            print(f"Unknown test {args.test}")
    else:
        for num, test_fn in tests.items():
            try:
                test_fn(ip)
            except Exception as e:
                print(f"  ERROR: {e}")
            time.sleep(1)

    print("\n" + "=" * 60)
    print("DONE. Check if lullaby stopped after each test.")
    print("=" * 60)


if __name__ == "__main__":
    main()
