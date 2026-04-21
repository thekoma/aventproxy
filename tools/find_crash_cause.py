#!/usr/bin/env python3
"""Identify which operation crashes the baby monitor.

Runs each operation one at a time with 30s gaps.
Pings the device continuously to detect crashes.

Usage:
    python tools/find_crash_cause.py [--ip 192.168.85.90]
"""

import argparse
import json
import subprocess
import sys
import threading
import time

import tinytuya

DEV_ID = "bf3fb082f7bfac6daaxaw2"
LOCAL_KEY = '9V-_a|mW^ZkY6u71'
DEFAULT_IP = "192.168.85.90"

PRIME_DPS = [101, 102, 106, 134, 138, 139, 140, 158, 207, 209, 237, 246]

ping_ok = True
ping_stop = threading.Event()


def ping_monitor(ip):
    """Continuous ping in background thread."""
    global ping_ok
    while not ping_stop.is_set():
        result = subprocess.run(
            ["ping", "-c", "1", "-W", "2", ip],
            capture_output=True, timeout=5
        )
        was_ok = ping_ok
        ping_ok = result.returncode == 0
        if was_ok and not ping_ok:
            print(f"\n  !!! PING LOST at {time.strftime('%H:%M:%S')} !!!")
        elif not was_ok and ping_ok:
            print(f"\n  ... ping recovered at {time.strftime('%H:%M:%S')}")
        time.sleep(1)


def wait_online(ip, timeout=120):
    """Wait for device to come back online."""
    print(f"  Waiting for device to come back online (max {timeout}s)...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        result = subprocess.run(
            ["ping", "-c", "1", "-W", "2", ip],
            capture_output=True, timeout=5
        )
        if result.returncode == 0:
            print(f"  Device online")
            time.sleep(5)
            return True
        time.sleep(2)
    print(f"  TIMEOUT - device not responding")
    return False


def test_operation(name, func, ip):
    """Run a single operation and check if device crashes."""
    global ping_ok
    print(f"\n{'='*60}")
    print(f"TEST: {name}")
    print(f"{'='*60}")

    if not ping_ok:
        print(f"  Device offline, waiting for recovery...")
        if not wait_online(ip):
            return "SKIPPED (device offline)"

    print(f"  Running at {time.strftime('%H:%M:%S')}...")
    try:
        result = func()
        print(f"  Result: {result}")
    except Exception as e:
        print(f"  Exception: {e}")

    print(f"  Monitoring for 30s...")
    for i in range(30):
        time.sleep(1)
        if not ping_ok:
            print(f"  !!! DEVICE CRASHED after {i+1}s !!!")
            return "CRASHED"

    print(f"  Device stable after 30s")
    return "OK"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--ip", default=DEFAULT_IP)
    args = parser.parse_args()
    ip = args.ip

    print(f"Baby Monitor Crash Finder")
    print(f"Device: {ip}")
    print(f"Starting ping monitor...")

    ping_thread = threading.Thread(target=ping_monitor, args=(ip,), daemon=True)
    ping_thread.start()
    time.sleep(3)

    if not ping_ok:
        print("Device not responding. Wait for it to boot.")
        wait_online(ip)

    results = {}

    # --- TEST 1: Simple TCP connect to LAN port ---
    def test_tcp_connect():
        import socket
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(5)
        s.connect((ip, 6668))
        s.close()
        return "connected and closed"

    results["1_tcp_connect"] = test_operation("TCP connect to port 6668", test_tcp_connect, ip)

    # --- TEST 2: TinyTuya status (sends DP_QUERY) ---
    def test_status():
        d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
        d.set_socketTimeout(5)
        s = d.status()
        d.close()
        return s

    results["2_tinytuya_status"] = test_operation("TinyTuya status()", test_status, ip)

    # --- TEST 3: TinyTuya updatedps with all PRIME_DPS ---
    def test_updatedps_all():
        d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
        d.set_socketTimeout(5)
        r = d.updatedps(PRIME_DPS)
        d.close()
        return r

    results["3_updatedps_all"] = test_operation("updatedps(12 DPS codes)", test_updatedps_all, ip)

    # --- TEST 4: TinyTuya persistent socket + receive loop (10s) ---
    def test_persistent_receive():
        d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
        d.set_socketPersistent(True)
        d.set_socketTimeout(3)
        d.updatedps(PRIME_DPS)
        count = 0
        for _ in range(5):
            data = d.receive()
            if data and isinstance(data, dict) and "dps" in data:
                count += 1
        d.close()
        return f"received {count} DPS updates"

    results["4_persistent_receive"] = test_operation("Persistent socket + receive (10s)", test_persistent_receive, ip)

    # --- TEST 5: Rapid reconnect (3x in 10s) ---
    def test_rapid_reconnect():
        for i in range(3):
            d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
            d.set_socketPersistent(True)
            d.set_socketTimeout(3)
            d.updatedps(PRIME_DPS)
            time.sleep(1)
            d.close()
            time.sleep(2)
        return "3 connect/disconnect cycles"

    results["5_rapid_reconnect"] = test_operation("Rapid reconnect 3x in 10s", test_rapid_reconnect, ip)

    # --- TEST 6: REST API get_device (cloud) ---
    def test_api_get_device():
        import asyncio
        import hashlib
        import aiohttp

        TUYA_SIGNING_KEY = (
            "com.philips.ph.babymonitorplus"
            "_D2:D6:95:A1:1D:1B:84:F9:25:A9:45:6E:27:F4:45:E9:FD:87:C3:74"
            ":63:AA:8A:34:32:A6:6A:23:3B:0F:D5:0F"
            "_8n459nxk9g98gqgcwrpk3csv97uuwajm"
            "_a3nfht4ufwfw9cmkspaftv4x89cx58qx"
        )
        sys.path.insert(0, ".")
        from custom_components.philips_avent.api import PhilipsAventAPI

        async def _call():
            async with aiohttp.ClientSession() as session:
                api = PhilipsAventAPI.__new__(PhilipsAventAPI)
                api._session = session
                api._sid = "eu16619584946940Nsj46eE38232b7204e8b0bac16f078293db9b175"
                api._ecode = "11u99u418646946e"
                api._signing_key = TUYA_SIGNING_KEY
                api._app_key = "wx3at9qprkhskvkcsyhm"
                api._ch_key = "071d81fa"
                api._api_url = "https://a1.tuyaeu.com/api.json"
                api._partner = "p1319959"
                api._uid = "eu1661954946940Nj46e"
                device = await api.get_device(DEV_ID)
                return f"got {len(device.get('dps', {}))} DPS values"

        return asyncio.run(_call())

    results["6_api_get_device"] = test_operation("REST API get_device (cloud)", test_api_get_device, ip)

    # --- TEST 7: REST API get_rssi ---
    def test_api_get_rssi():
        import asyncio
        import aiohttp

        TUYA_SIGNING_KEY = (
            "com.philips.ph.babymonitorplus"
            "_D2:D6:95:A1:1D:1B:84:F9:25:A9:45:6E:27:F4:45:E9:FD:87:C3:74"
            ":63:AA:8A:34:32:A6:6A:23:3B:0F:D5:0F"
            "_8n459nxk9g98gqgcwrpk3csv97uuwajm"
            "_a3nfht4ufwfw9cmkspaftv4x89cx58qx"
        )
        sys.path.insert(0, ".")
        from custom_components.philips_avent.api import PhilipsAventAPI

        async def _call():
            async with aiohttp.ClientSession() as session:
                api = PhilipsAventAPI.__new__(PhilipsAventAPI)
                api._session = session
                api._sid = "eu16619584946940Nsj46eE38232b7204e8b0bac16f078293db9b175"
                api._ecode = "11u99u418646946e"
                api._signing_key = TUYA_SIGNING_KEY
                api._app_key = "wx3at9qprkhskvkcsyhm"
                api._ch_key = "071d81fa"
                api._api_url = "https://a1.tuyaeu.com/api.json"
                api._partner = "p1319959"
                api._uid = "eu1661954946940Nj46e"
                rssi = await api.get_rssi(DEV_ID)
                return f"rssi={rssi}"

        return asyncio.run(_call())

    results["7_api_get_rssi"] = test_operation("REST API get_rssi (cloud)", test_api_get_rssi, ip)

    # --- TEST 8: set_dps via LAN (night light on/off) ---
    def test_set_dps_lan():
        d = tinytuya.Device(DEV_ID, ip, LOCAL_KEY, version=3.3)
        d.set_socketTimeout(5)
        r1 = d.set_value("138", True)
        time.sleep(1)
        r2 = d.set_value("138", False)
        d.close()
        return f"on={r1}, off={r2}"

    results["8_set_dps_lan"] = test_operation("set_dps via LAN (night light toggle)", test_set_dps_lan, ip)

    # --- RESULTS ---
    print(f"\n{'='*60}")
    print(f"RESULTS")
    print(f"{'='*60}")
    for name, result in results.items():
        status = "CRASH!" if result == "CRASHED" else result
        print(f"  {name}: {status}")

    crashed = [n for n, r in results.items() if r == "CRASHED"]
    if crashed:
        print(f"\n  CULPRITS: {', '.join(crashed)}")
    else:
        print(f"\n  No crashes detected. Issue might be cumulative.")

    ping_stop.set()


if __name__ == "__main__":
    main()
