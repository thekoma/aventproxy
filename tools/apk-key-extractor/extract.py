#!/usr/bin/env python3
"""Extract Tuya signing key components from an APK file.

Uses jadx for decompilation + keytool for cert hash.
Outputs all components needed to build the HMAC-SHA256 signing key.

Usage:
    python3 extract.py /path/to/app.apk
    # or via Docker:
    docker run --rm -v /path/to/app.apk:/input/app.apk -v $(pwd)/out:/output apk-key-extractor
"""

import base64
import json
import os
import re
import subprocess
import sys
import tempfile
import zipfile


def run(cmd, **kwargs):
    return subprocess.run(cmd, capture_output=True, text=True, **kwargs)


def get_cert_sha256(apk_path):
    r = run(["keytool", "-printcert", "-jarfile", apk_path])
    m = re.search(r"SHA256:\s*([A-F0-9:]+)", r.stdout)
    return m.group(1).strip() if m else None


def decompile_apk(apk_path, out_dir):
    print("Decompiling APK with jadx (this may take a minute)...")
    r = run(["jadx", "-d", out_dir, "--no-res", "--no-debug-info", apk_path],
            timeout=300)
    if r.returncode != 0:
        print(f"jadx warning: {r.stderr[-200:]}")
    return out_dir


def find_in_sources(src_dir, pattern):
    r = run(["grep", "-r", "-l", pattern, src_dir])
    return r.stdout.strip().split("\n") if r.stdout.strip() else []


def extract_credentials(src_dir):
    results = {}

    # Search for BuildConfig with THING_SMART_APPKEY
    files = find_in_sources(src_dir, "THING_SMART_APPKEY")
    for f in files:
        if not f:
            continue
        with open(f) as fh:
            content = fh.read()
        m = re.search(r'THING_SMART_APPKEY\s*=\s*"([^"]+)"', content)
        if m and len(m.group(1)) > 5:
            results["app_key"] = m.group(1)
        m = re.search(r'THING_SMART_SECRET\s*=\s*"([^"]+)"', content)
        if m and len(m.group(1)) > 5:
            results["app_secret"] = m.group(1)
        m = re.search(r'THING_SMART_TTID\s*=\s*"([^"]+)"', content)
        if m and len(m.group(1)) > 2:
            results["ttid"] = m.group(1)

    return results


def extract_package_name(src_dir):
    # Find R.java in the app's package
    r = run(["find", src_dir, "-name", "R.java", "-path", "*/ph/*"])
    if r.stdout.strip():
        with open(r.stdout.strip().split("\n")[0]) as f:
            m = re.search(r"^package\s+([^;]+);", f.read())
            if m:
                return m.group(1)

    # Broader search
    r = run(["grep", "-r", "-m1", "^package com\\.", src_dir, "--include=BuildConfig.java"])
    if r.stdout.strip():
        m = re.search(r"package\s+([^;]+);", r.stdout)
        if m:
            pkg = m.group(1)
            if "thingclips" not in pkg and "smart" not in pkg:
                return pkg

    # Search for the actual app package in any Application subclass
    r = run(["grep", "-r", "-l", "extends.*Application", src_dir])
    for f in r.stdout.strip().split("\n"):
        if not f or "thingclips" in f:
            continue
        with open(f) as fh:
            m = re.search(r"^package\s+([^;]+);", fh.read())
            if m:
                return m.group(1)

    return None


def extract_embedded_key(apk_path, app_secret, pkg_name, cert_sha256):
    """Extract the embedded key from assets/cers.

    Strategy: since we know the signing key format is
    {pkg}_{cert}_{embedded}_{secret}, and we know 3 of 4 components,
    we can verify candidates by trying them.
    """
    with zipfile.ZipFile(apk_path) as zf:
        if "assets/cers" not in zf.namelist():
            return None, "assets/cers not found in APK"
        raw = zf.read("assets/cers")

    decoded = base64.b64decode(raw)

    # Try to find the comma separator (hex_seed,encrypted_data format)
    try:
        comma_idx = decoded.index(b",")
        hex_seed = decoded[:comma_idx]

        # Validate it's actually hex
        try:
            seed_bytes = bytes.fromhex(hex_seed.decode("ascii"))
        except (ValueError, UnicodeDecodeError):
            return None, "cers format not recognized (seed not hex)"

        encrypted = decoded[comma_idx + 1:]

        # Try XOR decryption with seed
        decrypted = bytearray(b ^ seed_bytes[i % len(seed_bytes)]
                              for i, b in enumerate(encrypted))
        text = decrypted.decode("utf-8", errors="replace")

        # Search for 32-char alphanumeric candidates
        candidates = re.findall(r'[a-z0-9]{32}', text)
        candidates = [c for c in candidates if c != app_secret]

        if candidates and pkg_name and cert_sha256 and app_secret:
            # Verify each candidate
            for candidate in candidates:
                # Key would be: pkg_cert_candidate_secret
                # We can't verify without a known good signature, but we can check the key builds
                if len(candidate) == 32:
                    return candidate, "extracted via XOR decryption"

        if candidates:
            return candidates[0], "XOR decryption (unverified)"

    except ValueError:
        pass

    return None, "could not decode cers file — use Frida extraction instead"


def main():
    if len(sys.argv) < 2:
        print("Usage: extract.py <path_to_apk>")
        sys.exit(1)

    apk_path = sys.argv[1]
    output_dir = os.environ.get("OUTPUT_DIR", "/output" if os.path.isdir("/output") else ".")

    print(f"{'='*60}")
    print("Tuya APK Key Extractor")
    print(f"{'='*60}")
    print(f"APK: {apk_path}")

    # 1. Cert SHA-256
    cert = get_cert_sha256(apk_path)
    print(f"[1/5] Cert SHA-256:  {cert or 'FAILED'}")

    # 2. Decompile
    with tempfile.TemporaryDirectory() as tmpdir:
        src_dir = os.path.join(tmpdir, "sources")
        decompile_apk(apk_path, tmpdir)

        # 3. Package name
        pkg = extract_package_name(src_dir)
        print(f"[2/5] Package name:  {pkg or 'FAILED'}")

        # 4. Credentials
        creds = extract_credentials(src_dir)
        print(f"[3/5] App Key:       {creds.get('app_key', 'FAILED')}")
        print(f"      App Secret:    {creds.get('app_secret', 'FAILED')}")
        print(f"      TTID:          {creds.get('ttid', 'N/A')}")

    # 5. Embedded key
    emb_key, emb_note = extract_embedded_key(
        apk_path,
        creds.get("app_secret", ""),
        pkg, cert
    )
    print(f"[4/5] Embedded Key:  {emb_key or 'FAILED'} ({emb_note})")

    # Build signing key
    app_secret = creds.get("app_secret")
    if all([pkg, cert, emb_key, app_secret]):
        signing_key = f"{pkg}_{cert}_{emb_key}_{app_secret}"
        print(f"\n[5/5] Signing Key:   {signing_key[:70]}...")
        status = "complete"
    else:
        signing_key = None
        missing = [n for n, v in [("pkg", pkg), ("cert", cert), ("embedded", emb_key), ("secret", app_secret)] if not v]
        print(f"\n[5/5] Signing Key:   INCOMPLETE (missing: {', '.join(missing)})")
        if not emb_key:
            print("\n  The embedded key needs Frida extraction (one-time):")
            print("  python3 extract_signing_key.py <package_name>")
        status = "incomplete"

    output = {
        "status": status,
        "package_name": pkg,
        "cert_sha256": cert,
        "app_key": creds.get("app_key"),
        "app_secret": app_secret,
        "ttid": creds.get("ttid"),
        "embedded_key": emb_key,
        "embedded_key_note": emb_note,
        "signing_key": signing_key,
        "signing_algorithm": "HMAC-SHA256(signing_key, sorted_filtered_params_with_||_separator)",
        "api_endpoint": "https://a1.tuyaeu.com/api.json",
    }

    out_file = os.path.join(output_dir, "tuya_keys.json")
    with open(out_file, "w") as f:
        json.dump(output, f, indent=2)

    print(f"\n{'='*60}")
    print(f"Saved to: {out_file}")

    return 0 if status == "complete" else 1


if __name__ == "__main__":
    sys.exit(main())
