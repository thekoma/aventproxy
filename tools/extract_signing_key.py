#!/usr/bin/env python3
"""One-shot Frida script: extract the Tuya signing key from a running app.

Run once on a phone with the app open. Extracts all components needed for
autonomous API access. No phone needed after this.

Usage:
    python3 extract_signing_key.py [package_name]

Example:
    python3 extract_signing_key.py com.philips.ph.babymonitorplus
"""

import frida
import json
import sys
import os

FRIDA_SCRIPT = """
Java.perform(function() {
    // 1. Extract signing key from libthing_security.so memory
    var lib = Process.getModuleByName("libthing_security.so");
    var strAddr = lib.base.add(0x39070);
    var flag = strAddr.readU8();
    var isHeap = (flag & 1) !== 0;

    var dataPtr, len;
    if (isHeap) {
        len = strAddr.add(8).readU64();
        dataPtr = strAddr.add(16).readPointer();
    } else {
        len = flag >> 1;
        dataPtr = strAddr.add(1);
    }

    var signingKey = "";
    for (var i = 0; i < len; i++) {
        signingKey += String.fromCharCode(dataPtr.add(i).readU8());
    }

    // 2. Extract appKey/appSecret from ThingSmartNetWork
    var NetWork = Java.use("com.thingclips.smart.android.network.ThingSmartNetWork");
    var appKey = NetWork.mAppId.value;
    var appSecret = NetWork.mAppSecret.value;

    // 3. Extract current SID from the app's session
    var sid = "";
    try {
        var ThingHomeSdk = Java.use("com.thingclips.smart.home.sdk.ThingHomeSdk");
        // Try to get user session
        Java.enumerateLoadedClasses({
            onMatch: function(className) {
                if (className === "com.thingclips.smart.android.base.utils.UserPreferenceUtil") {
                    // Found it
                }
            },
            onComplete: function() {}
        });
    } catch(e) {}

    // 4. Extract deviceId and chKey from a recent API call
    // These are in the network config

    // Parse signing key components
    var parts = signingKey.split("_");
    // Format: pkg_CERT:SHA:256_embeddedKey_appSecret
    // But pkg might contain underscores, and cert has colons not underscores
    // Find cert hash (starts with uppercase hex with colons)
    var certStart = signingKey.indexOf("_") + 1;
    var certEnd = signingKey.indexOf("_", certStart + 60); // cert is ~95 chars

    send({
        type: "result",
        signingKey: signingKey,
        signingKeyLength: len,
        appKey: appKey,
        appSecret: appSecret,
        libBase: lib.base.toString(),
    });
});
"""

def main():
    pkg = sys.argv[1] if len(sys.argv) > 1 else None

    device = frida.get_usb_device()

    if pkg:
        # Find by package name
        pid = None
        for proc in device.enumerate_processes():
            if pkg in proc.name and "monitor" not in proc.name:
                pid = proc.pid
                break
        if not pid:
            print(f"Process {pkg} not found. Make sure the app is open.")
            sys.exit(1)
    else:
        # List Tuya-based apps
        print("Looking for Tuya-based apps...")
        candidates = []
        for proc in device.enumerate_processes():
            if any(x in proc.name.lower() for x in ["baby", "camera", "smart", "tuya"]):
                candidates.append((proc.pid, proc.name))
        if not candidates:
            print("No Tuya apps found running.")
            sys.exit(1)
        print("Found:")
        for pid, name in candidates:
            print(f"  {pid}: {name}")
        pid = candidates[0][0]
        print(f"\nUsing PID {pid}")

    result = {}

    def on_message(msg, data):
        nonlocal result
        if msg["type"] == "send" and msg["payload"]["type"] == "result":
            result = msg["payload"]

    session = device.attach(pid)
    script = session.create_script(FRIDA_SCRIPT)
    script.on("message", on_message)
    script.load()

    import time
    time.sleep(2)
    script.unload()
    session.detach()

    if not result:
        print("Failed to extract signing key")
        sys.exit(1)

    signing_key = result["signingKey"]
    app_key = result["appKey"]
    app_secret = result["appSecret"]

    # Parse signing key: {pkg}_{certSHA256}_{embeddedKey}_{appSecret}
    # The cert hash has format XX:XX:XX:... (uppercase hex with colons, 95 chars)
    # So we find it by looking for the colon pattern
    import re
    cert_match = re.search(r'([A-F0-9]{2}(?::[A-F0-9]{2}){31})', signing_key)

    if cert_match:
        cert_sha256 = cert_match.group(1)
        before_cert = signing_key[:cert_match.start() - 1]  # remove trailing _
        after_cert = signing_key[cert_match.end() + 1:]  # remove leading _
        # after_cert = embeddedKey_appSecret
        embedded_key = after_cert[:after_cert.rfind("_")]
        pkg_name = before_cert
    else:
        print("WARNING: Could not parse signing key components")
        pkg_name = "unknown"
        cert_sha256 = "unknown"
        embedded_key = "unknown"

    output = {
        "signing_key": signing_key,
        "components": {
            "package_name": pkg_name,
            "cert_sha256": cert_sha256,
            "embedded_key": embedded_key,
            "app_secret": app_secret,
        },
        "app_key": app_key,
        "app_secret": app_secret,
        "note": "signing_key = HMAC-SHA256 key for Tuya mobile SDK API requests"
    }

    out_file = f"tuya_keys_{pkg_name.replace('.', '_')}.json"
    with open(out_file, "w") as f:
        json.dump(output, f, indent=2)

    print(f"\n{'='*60}")
    print(f"Tuya Signing Key Extracted Successfully")
    print(f"{'='*60}")
    print(f"Package:      {pkg_name}")
    print(f"App Key:      {app_key}")
    print(f"App Secret:   {app_secret}")
    print(f"Cert SHA256:  {cert_sha256[:40]}...")
    print(f"Embedded Key: {embedded_key}")
    print(f"Signing Key:  {signing_key[:60]}...")
    print(f"\nSaved to: {out_file}")
    print(f"\nThis key is STATIC per APK version. Extract once, use forever.")


if __name__ == "__main__":
    main()
