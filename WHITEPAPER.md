# Reverse Engineering Tuya Whitelabel Camera API Authentication

## A complete methodology for extracting RTSP streams from Tuya-based cameras

**Date:** April 2026
**Target:** Tuya IoT Platform — Mobile SDK API (`a1.tuyaeu.com`)
**Device:** Tuya whitelabel baby monitor (IPC category)
**Goal:** Local RTSP streaming via Home Assistant, bypassing vendor cloud lock-in

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Background and Motivation](#2-background-and-motivation)
3. [Target Architecture](#3-target-architecture)
4. [Phase 1: Network Reconnaissance](#4-phase-1-network-reconnaissance)
5. [Phase 2: MITM and Traffic Analysis](#5-phase-2-mitm-and-traffic-analysis)
6. [Phase 3: API Signing Algorithm Reverse Engineering](#6-phase-3-api-signing-algorithm-reverse-engineering)
7. [Phase 4: Login Flow and Session Management](#7-phase-4-login-flow-and-session-management)
8. [Phase 5: Key Extraction Industrialization](#8-phase-5-key-extraction-industrialization)
9. [Complete API Reference](#9-complete-api-reference)
10. [Integration Architecture](#10-integration-architecture)
11. [Failures and Dead Ends](#11-failures-and-dead-ends)
12. [Phase 6: MQTT Credential Derivation](#12-phase-6-mqtt-credential-derivation)
13. [Security Considerations](#13-security-considerations)
14. [Reproducibility Guide](#14-reproducibility-guide)

---

## 1. Executive Summary

This paper documents the complete reverse engineering of the Tuya Mobile SDK API authentication mechanism, enabling autonomous programmatic access to Tuya whitelabel cameras without dependency on the vendor's mobile application or cloud portal.

**Key findings:**

- The API signing algorithm is **HMAC-SHA256** with a composite key derived from four static components extractable from the APK
- Authentication uses the **same password + MFA flow** as the vendor app (email + password + RSA encryption + 6-digit email MFA code), making API traffic indistinguishable from legitimate app usage
- A **passwordless OTP fallback** also exists but uses a different API endpoint
- The signing key is **static per APK version** — extract once, use indefinitely
- The SID is the only rotating credential, renewable via the password + MFA flow
- All WebRTC/MQTT/RTSP bridge logic is protocol-level and API-agnostic

**Result:** Full autonomous API access from Python/Go, suitable for Home Assistant integration with a config flow identical to the vendor app's login (email → password → MFA code from email).

---

## 2. Background and Motivation

### The Problem

A consumer baby monitor, sold under a major brand name, uses the Tuya IoT platform as its backend. The device:

- Streams video only through the vendor's proprietary mobile app
- Has no documented API or local streaming capability
- Exposes RTSP on port 554 but with AES-encrypted video (unusable without the session key)
- Uses Tuya's whitelabel platform, meaning the vendor's app is a thin skin over Tuya's SDK

The goal: integrate the camera into Home Assistant for local viewing, recording, and automation — without depending on the vendor's app or servers for the video path.

### Prior Work

An earlier phase of this project produced `aventproxy` — an RTSP proxy that handles the camera's non-standard RTSP implementation (rejects `OPTIONS` requests). This solved the protocol compatibility issue but not the authentication/encryption problem.

### Why Tuya Mobile SDK

The Tuya platform offers three API surfaces:

| API | Endpoint | Auth | Used By |
|-----|----------|------|---------|
| Mobile SDK | `a1.tuyaeu.com/api.json` | HMAC-SHA256 + SID | Mobile apps |
| Web Portal | `protect-eu.ismartlife.me` | Cookie session | Smart Life web |
| Cloud OpenAPI | `openapi.tuyaeu.com` | HMAC-SHA256 (simple) | IoT developers |

The camera is registered under the vendor's Tuya namespace, invisible to both Smart Life and the Cloud OpenAPI. Only the Mobile SDK API, using the vendor's app credentials, can access it.

---

## 3. Target Architecture

### Device Network Profile

The camera exposes the following LAN services:

| Port | Protocol | Purpose |
|------|----------|---------|
| 554 | RTSP | Video stream (AES encrypted) |
| 6000 | Tuya Auth | RTSP credential negotiation |
| 6668 | Tuya LAN | Native video transport (used by app) |
| 8686 | Unknown | Discovery/control |
| 8687 | Unknown | Discovery/control |

### App-to-Cloud Communication

The mobile app maintains three persistent connections:

1. **HTTPS** to `a1.tuyaeu.com` — API calls (REST over form-encoded POST)
2. **MQTT/TLS** to `m1.tuyaeu.com:8883` — Real-time signaling, device events
3. **TCP** to camera `192.168.x.x:6668` — Direct LAN video transport

When streaming, the app authenticates via the cloud, then establishes a direct LAN connection to the camera on port 6668. No WebRTC or P2P relay is used when on the same network.

### Software Stack

- **App framework:** React Native + Tuya Thing SDK 6.7.0
- **Native crypto:** `libthing_security.so` (mbedTLS-based, ARM64)
- **Key-value storage:** MMKV (encrypted)
- **Network:** OkHttp3 with certificate pinning

---

## 4. Phase 1: Network Reconnaissance

### Methodology

Determined the streaming path by examining active network connections during a live video session.

**Tools:** `adb shell netstat -anp`, `adb root`

### Findings

```
Process: com.vendor.app:monitor (PID xxxxx)

TCP connections:
  phone:48306 → camera:6668     ESTABLISHED   (LAN video transport)

UDP listeners:
  0.0.0.0:6666                  (Tuya discovery)
  0.0.0.0:6667                  (Tuya discovery)
  0.0.0.0:7000                  (Tuya discovery)

Process: com.vendor.app (PID xxxxx)

TCP connections:
  phone:38435 → 3.x.x.x:1443   ESTABLISHED   (Tuya cloud - signaling)
  phone:xxxxx → [2a05:...]:443  ESTABLISHED   (Tuya MQTT - TLS)
  phone:xxxxx → [2a05:...]:8883 ESTABLISHED   (Tuya MQTT - TLS)
```

**Key insight:** Video streams over LAN (port 6668), not cloud. Authentication and signaling go through the cloud. The camera also supports WebRTC (confirmed later via API), which provides an alternative streaming path usable by third-party tools.

---

## 5. Phase 2: MITM and Traffic Analysis

### Setup

**Proxy:** mitmproxy (Docker container)
**SSL Unpinning:** Frida 17.9.1 with httptoolkit's SSL unpinning script
**Certificate:** mitmproxy CA installed as Android system certificate

```bash
# Docker proxy
docker run --rm -d --name mitmproxy -p 8080:8080 \
  mitmproxy/mitmproxy mitmdump --set block_global=false --set flow_detail=4

# Android proxy
adb shell "settings put global http_proxy proxy_ip:8080"

# Frida SSL unpinning (spawn mode)
frida -U -f com.vendor.app -l ssl_unpin.js
```

### Captured API Structure

All API calls use a single endpoint:

```
POST https://a1.tuyaeu.com/api.json
Content-Type: application/x-www-form-urlencoded
```

Parameters are form-encoded (not JSON). Each request includes:

| Parameter | Example | Purpose |
|-----------|---------|---------|
| `a` | `smartlife.m.rtc.config.get` | API action |
| `v` | `1.0` | API version |
| `clientId` | `wx3at9q...` | App key |
| `sid` | `eu16619...` | Session token |
| `sign` | `f728c6b8...` (64 hex chars) | HMAC signature |
| `et` | `3` (encrypted) or `0.0.1` (plain) | Encryption type |
| `time` | `1776695995` | Unix timestamp |
| `postData` | `{\"devId\":\"bf3fb..."}` | Request body (JSON string) |
| `ttid` | `sdk_international@wx3at9q...` | Tenant/app ID |
| `deviceId` | `4ea12...` | Phone device identifier |

**Critical observation:** All responses with `et: '3'` are encrypted. Switching to `et: '0.0.1'` returns plaintext responses — the server accepts both.

### API Actions Discovered

From a single app session, 50+ API actions were captured. Key ones:

| Action | Purpose |
|--------|---------|
| `smartlife.m.rtc.config.get` | WebRTC configuration (STUN/TURN, ICE, AES key) |
| `tuya.m.device.get` | Full device info (localKey, DPS, online status) |
| `smartlife.m.user.info.get` | User profile + MQTT domain URLs |
| `m.life.home.space.list` | Home/room structure |
| `smartlife.m.p2p.main.pre.link.get` | P2P pre-connection |
| `smartlife.m.token.get` | MQTT token |
| `smartlife.m.rtc.log` | WebRTC telemetry |

---

## 6. Phase 3: API Signing Algorithm Reverse Engineering

This was the core challenge. The `sign` parameter is a 64-character hex string (SHA-256), computed by native code in `libthing_security.so`.

### 6.1 Java-Side Analysis

**Tool:** jadx (APK decompilation)

The signing flow in Java:

```
ThingApiSignManager.generateSignatureSdk(Map params)
  → filter params to whitelist
  → sort alphabetically
  → if postData present: replace value with swapSignString(MD5(postData))
  → concatenate as "key=value||key=value||..."
  → call doCommandNative(context, 1, paramString.getBytes(), null, debugFlag)
  → return hex string result
```

**Parameter whitelist** (only these keys are included in the signature):
```
a, v, lat, lon, lang, deviceId, appVersion, ttid, isH5, h5Token,
os, clientId, postData, time, requestId, et, n4h5, sid, chKey, sp
```

**swapSignString** rearranges a 32-char hex string in 8-char blocks:
```
Input:  [A][B][C][D]    (4 blocks × 8 chars)
Output: [B][A][D][C]
```

**Initialization:** `doCommandNative(context, 0, appSecret.getBytes(), appKey.getBytes(), debugFlag)` — passes the app credentials to the native layer during SDK init.

### 6.2 Native Library Analysis

**Target:** `libthing_security.so` (ARM64, ~240KB)
**Tools:** radare2 + r2ghidra plugin, Frida

The native `doCommandNative` is registered via `JNI_OnLoad` using `RegisterNatives`. The function pointer was located at runtime using Frida:

```javascript
var lib = Process.getModuleByName("libthing_security.so");
var tableAddr = lib.base.add(0x38c68); // JNI method table
// Entry 0: doCommandNative at offset 0x13ed8
```

Decompilation of the init function (`fcn.00016528`, called with command 0) revealed:

1. Calls `getPackageManager().getPackageInfo(pkgName, GET_SIGNATURES)`
2. Extracts APK signing certificate via `Signature.toByteArray()`
3. Computes `SHA-256(certificate_bytes)` via JNI callback to `java.security.MessageDigest`
4. Stores the hash alongside `appSecret` and `appKey`
5. Reads additional key material from an embedded file via `read_keys_from_content`

### 6.3 Signing Key Extraction

The breakthrough came from dumping the constructed signing key directly from process memory.

The key is stored as a `std::__ndk1::basic_string` at a fixed offset in the `.bss` section of `libthing_security.so` (offset `0x39070`). Using Frida:

```javascript
var lib = Process.getModuleByName("libthing_security.so");
var strAddr = lib.base.add(0x39070);
var flag = strAddr.readU8();
// libc++ SSO: if (flag & 1) → heap allocated
var len = strAddr.add(8).readU64();     // 192
var dataPtr = strAddr.add(16).readPointer();
// Read 192 bytes from dataPtr → the complete signing key
```

**Result:**
```
{packageName}_{certSHA256}_{embeddedKey}_{appSecret}
```

Where:
- `packageName` = Android package name (from manifest)
- `certSHA256` = SHA-256 of APK signing cert (uppercase hex, colon-separated)
- `embeddedKey` = 32-char alphanumeric string from `assets/cers`
- `appSecret` = Tuya app secret (from BuildConfig)

### 6.4 Algorithm Verification

With the extracted key, the signing algorithm was verified as standard **HMAC-SHA256**:

```python
import hmac, hashlib

signing_key = "{pkg}_{cert}_{embedded}_{secret}"
params_string = "a=smartlife.p.time.get||appVersion=1.8.0||chKey=071d81fa||..."
signature = hmac.new(signing_key.encode(), params_string.encode(), hashlib.sha256).hexdigest()
# → matches the captured sign value exactly
```

### 6.5 Autonomous API Calls

With the signing algorithm known, API calls were made from Python without any phone involvement:

```python
# Independent API call — no phone, no Frida, no proxy
response = call_api("tuya.m.device.get", post_data={"devId": "..."})
# → returns full device info including localKey, DPS, online status
```

---

## 7. Phase 4: Login Flow and Session Management

### The SID Problem

The session ID (SID) is the only rotating credential. It's obtained during login and included in every API call. When it expires, a new login is required.

### Password + MFA Login (Production Flow)

This is the exact flow used by the vendor's mobile app, reverse engineered via Frida instrumentation of a clean first-login (app data wiped, no prior session).

**Step 1 — Obtain RSA token:**
```
Action: thing.m.user.username.token.get  (v2.0)
PostData: {"countryCode": "39", "username": "user@example.com", "isUid": false}
SID: (empty — no session required)
Result: {"token": "f065...", "pbKey": "MIGd...", "publicKey": "1353...", "exponent": "3"}
```

**Step 2 — Encrypt password and attempt login:**

The password is MD5-hashed, then RSA-encrypted using the `pbKey` from step 1:
```python
md5_pass = hashlib.md5(password.encode()).hexdigest()
pem_key = f"-----BEGIN PUBLIC KEY-----\n{pbKey}\n-----END PUBLIC KEY-----"
rsa_key = RSA.import_key(pem_key)
encrypted = PKCS1_v1_5.new(rsa_key).encrypt(md5_pass.encode()).hex()
```

```
Action: thing.m.user.email.password.login  (v3.0)
PostData: {
  "countryCode": "39", "email": "user@example.com",
  "passwd": "<RSA encrypted MD5>", "token": "<from step 1>",
  "ifencrypt": 1, "options": "{\"group\": 1, \"mfaCode\": \"\"}"
}
Result: {"errorCode": "MFA_NEED_SEND_CODE"}
```

The server validates the password and responds with `MFA_NEED_SEND_CODE`, indicating that MFA is required. Note: using the wrong RSA token endpoint (`thing.m.user.email.token.create` v1.0 instead of `thing.m.user.username.token.get` v2.0) produces a misleading `USER_PASSWD_WRONG` error even with valid credentials.

**Step 3 — Request MFA code (requires new RSA token):**
```
Action: thing.m.user.username.token.get  (v2.0)  — fresh token needed
Action: thing.m.user.username.mfa.code.get  (v1.0)
PostData: {
  "countryCode": "39", "username": "user@example.com",
  "passwd": "<RSA encrypted MD5 with new token>", "token": "<new token>",
  "ifencrypt": 1, "options": "{\"group\": 1, \"mfaCode\": \"null\"}"
}
Result: {"result": {"countryCode": "39", "email": "user@example.com"}, "success": true}
```

The server sends a 6-digit code to the user's email.

**Step 4 — Complete login with MFA code (requires new RSA token):**
```
Action: thing.m.user.username.token.get  (v2.0)  — fresh token needed
Action: thing.m.user.email.password.login  (v3.0)
PostData: {
  "countryCode": "39", "email": "user@example.com",
  "passwd": "<RSA encrypted MD5 with new token>", "token": "<new token>",
  "ifencrypt": 1, "options": "{\"group\": 1, \"mfaCode\": \"179958\"}"
}
Result: {
  "sid": "eu16619...(new session token)...",
  "uid": "eu166195...",
  "nickname": "...",
  "domain": { "mobileMqttsUrl": "m1.tuyaeu.com", ... }
}
```

This flow is identical to the vendor app's behavior. Each password submission requires a fresh RSA token (tokens are single-use). The MFA code is passed inside the `options` JSON field as `mfaCode`, not as a separate `code` parameter.

### Alternative: Email OTP Login (Passwordless)

The Tuya SDK also exposes a passwordless login endpoint that bypasses both password and MFA:

```
Action: thing.m.user.email.code.send  (no SID needed)
PostData: {"email": "user@example.com", "countryCode": "39", "type": 1}
→ sends 6-digit code to email

Action: thing.m.user.email.code.login  (no SID needed)
PostData: {"email": "user@example.com", "code": "123456", "countryCode": "39"}
→ returns SID directly
```

This endpoint is likely intended for "forgot password" or passwordless onboarding flows. It produces a valid SID with full account access. However, it is not used by the vendor app's normal login flow and uses a different API action, making it potentially distinguishable server-side.

**For the integration, we use the password + MFA flow** (same as the app) to be indistinguishable from normal app traffic. The OTP passwordless flow is documented here as a fallback.

### SID Lifecycle

- **Issued at:** login (password + MFA, or OTP)
- **Expires after:** unknown (estimated weeks/months based on app behavior)
- **Renewal:** re-trigger the full password + MFA flow
- **Scope:** full account access (all devices, all APIs)

### Home Assistant Config Flow

The login maps naturally to a multi-step HA config flow:

1. **Step 1:** User enters email + password
2. **Step 2 (automatic):** Integration encrypts password, calls login API, triggers MFA
3. **Step 3:** User checks email, enters 6-digit MFA code in HA
4. **Step 4 (automatic):** Integration completes login, stores SID
5. **On expiry:** Integration prompts user to re-authenticate (same flow)

---

## 8. Phase 5: Key Extraction Industrialization

### Static Components (Extractable from APK)

| Component | Source | Tool |
|-----------|--------|------|
| `packageName` | AndroidManifest.xml | jadx / aapt |
| `certSHA256` | APK signing certificate | `keytool -printcert -jarfile` |
| `appKey` | `BuildConfig.THING_SMART_APPKEY` | jadx + grep |
| `appSecret` | `BuildConfig.THING_SMART_SECRET` | jadx + grep |
| `ttid` | `BuildConfig.THING_SMART_TTID` | jadx + grep |

All five are constants compiled into the APK. They persist across app updates (same signing key, same build configuration). They change only if the vendor re-signs the APK or updates their Tuya credentials.

### Embedded Key (`assets/cers`)

The `cers` file in the APK assets contains encoded key material. It is decoded by a native function (`read_keys_from_content` in `libthing_security_algorithm.so`) using a proprietary format:

- Base64-encoded content
- First segment: hex seed (variable length)
- Remainder: encrypted key material
- Decryption algorithm: proprietary (embedded in native code)

**Extraction methods:**
1. **Frida (one-shot):** Attach to running app, dump the signing key from memory offset `0x39070` in `libthing_security.so`. This gives all components at once.
2. **Native decoder reversal:** Possible but time-intensive. The algorithm uses mbedTLS primitives.

### Docker Container for Static Extraction

A Docker container automates extraction of the 4 static components:

```bash
docker build -t apk-key-extractor tools/apk-key-extractor/
docker run --rm \
  -v /path/to/app.apk:/input/app.apk \
  -v $(pwd)/output:/output \
  apk-key-extractor
```

Output: JSON file with all extracted components. The embedded key requires one additional Frida run if the `cers` decoder is not reversed.

### For Other Tuya Whitelabel Apps

The entire process is generic. Any app built on the Tuya Thing SDK uses the same signing mechanism. The steps for a new target:

1. Obtain the APK
2. Run the Docker extractor → 4/5 components
3. Run Frida one-shot on a phone with the app → embedded key
4. Combine → complete signing key
5. Use OTP login flow → SID
6. Full API access

---

## 9. Complete API Reference

### Authentication

| Action | Version | Description | Requires SID |
|--------|---------|-------------|:---:|
| `thing.m.user.username.token.get` | 2.0 | Get RSA token for password encryption | No |
| `thing.m.user.email.password.login` | 3.0 | Password + MFA login → returns SID | No |
| `thing.m.user.username.mfa.code.get` | 1.0 | Trigger MFA code email | No |
| `thing.m.user.email.code.send` | 1.0 | Send OTP to email (passwordless) | No |
| `thing.m.user.email.code.login` | 1.0 | Login with OTP → returns SID (passwordless) | No |

### Device Management

| Action | Description | PostData |
|--------|-------------|----------|
| `tuya.m.device.get` | Full device info | `{"devId": "..."}` |
| `m.life.home.space.list` | List homes | — |
| `smartlife.m.user.info.get` | User profile + domain URLs | — |

### Streaming

| Action | Description | PostData |
|--------|-------------|----------|
| `smartlife.m.rtc.config.get` | WebRTC config (ICE/STUN/TURN) | `{"devId": "..."}` |
| `smartlife.m.p2p.main.pre.link.get` | Pre-connect signal | `{"devId": "..."}` |
| `smartlife.m.token.get` | MQTT token | — |

### Request Signing

```python
import hmac, hashlib

WHITELIST = {"a","v","lat","lon","lang","deviceId","appVersion","ttid",
             "isH5","h5Token","os","clientId","postData","time",
             "requestId","et","n4h5","sid","chKey","sp"}

def sign(params, signing_key):
    filtered = {k: v for k, v in params.items() if k in WHITELIST and v}
    if "postData" in filtered:
        md5 = hashlib.md5(filtered["postData"].encode()).hexdigest()
        # Swap 8-char blocks: [A][B][C][D] → [B][A][D][C]
        filtered["postData"] = md5[8:16]+md5[0:8]+md5[24:32]+md5[16:24]
    param_str = "||".join(f"{k}={filtered[k]}" for k in sorted(filtered))
    return hmac.new(signing_key.encode(), param_str.encode(), hashlib.sha256).hexdigest()
```

---

## 10. Integration Architecture

### Home Assistant Custom Component

```
┌─────────────────────────────────────────────┐
│                Home Assistant                │
│                                              │
│  ┌──────────────────────────────────────┐   │
│  │      Config Flow (like Tado)         │   │
│  │  1. Enter email                      │   │
│  │  2. Receive OTP via email            │   │
│  │  3. Enter OTP → get SID             │   │
│  │  4. Auto-discover cameras            │   │
│  └──────────┬───────────────────────────┘   │
│             │                                │
│  ┌──────────▼───────────────────────────┐   │
│  │      Tuya Mobile SDK Client          │   │
│  │  - HMAC-SHA256 signing               │   │
│  │  - SID management + auto-refresh     │   │
│  │  - WebRTC config fetching            │   │
│  └──────────┬───────────────────────────┘   │
│             │                                │
│  ┌──────────▼───────────────────────────┐   │
│  │      WebRTC → RTSP Bridge            │   │
│  │  - MQTT signaling                    │   │
│  │  - ICE/STUN/TURN negotiation         │   │
│  │  - RTP forwarding to RTSP server     │   │
│  └──────────┬───────────────────────────┘   │
│             │                                │
│  ┌──────────▼───────────────────────────┐   │
│  │      camera entity                   │   │
│  │  rtsp://localhost:8554/CameraName    │   │
│  └──────────────────────────────────────┘   │
└─────────────────────────────────────────────┘
```

### Credential Hierarchy

```
Static (per APK version, extract once):
  ├── packageName        ← from AndroidManifest.xml
  ├── certSHA256         ← from APK signing certificate
  ├── embeddedKey        ← from assets/cers (Frida one-shot)
  └── appSecret          ← from BuildConfig.java

Dynamic (per user session):
  └── SID                ← from OTP login (renewable via email)
```

---

## 11. Failures and Dead Ends

### 11.1 Direct RTSP Authentication (Weeks of Effort)

**Approach:** The camera's RTSP server on port 554 is open (no auth) but streams AES-encrypted video. A custom proxy (`baby_rtsp_proxy.py`) successfully handled the RTSP handshake and received video frames — but they were encrypted with an AES key negotiated on port 6000 using the Tuya LAN protocol.

**What worked:** DESCRIBE, SETUP, PLAY all succeed. Raw H.264 NALUs are received.
**What failed:** Video is AES-encrypted. The key is negotiated in a binary handshake on port 6000 that involves the device's `localKey` and a session-specific derivation.
**Why abandoned:** The LAN encryption protocol is complex, firmware-specific (AltoBeam chipset), and the key derivation involves multiple rounds of AES/MD5 that proved impractical to reverse without firmware dumps.

### 11.2 Smart Life Web Portal Login

**Approach:** Use the existing open-source tool `tuya-ipc-terminal` which authenticates via the Smart Life web portal (`protect-eu.ismartlife.me`).

**What happened:** The user's email is recognized by the Smart Life endpoint (the `GetLoginToken` API succeeds, returning an RSA public key). However, the password login fails with `USER_PASSWD_WRONG`.

**Root cause:** Tuya whitelabel apps use isolated namespaces. An account created in the vendor's app exists in Tuya's unified user database (the email is recognized) but the password is scoped to the vendor's namespace. The Smart Life web portal cannot authenticate vendor-namespaced accounts.

**Time wasted:** ~2 hours trying different country codes, credential formats, and endpoint variations.

### 11.3 Wrong RSA Token Endpoint for Password Login

**Approach:** Implement the password login flow on the mobile SDK API using `thing.m.user.email.token.create` (v1.0) to obtain the RSA token for password encryption.

**What happened:** Password login consistently returned `USER_PASSWD_WRONG` despite using the exact same password and MD5+RSA encryption verified by Frida hooks on the real app. The real app also appeared to receive the same error and retry multiple times.

**Root cause:** The app uses `thing.m.user.username.token.get` (v2.0) for the RSA token, not `thing.m.user.email.token.create` (v1.0). These are different endpoints that return different RSA keys. Encrypting the password with a token from the wrong endpoint produces a valid-looking ciphertext that the server cannot decrypt, resulting in a misleading `USER_PASSWD_WRONG` instead of a more informative error.

**Additional discovery:** The correct flow returns `MFA_NEED_SEND_CODE` (not `USER_PASSWD_WRONG`) when the password is valid but MFA is required. The MFA code is passed inside `options.mfaCode`, not as a separate `code` field. Each password submission also requires a fresh single-use RSA token.

**How it was found:** By doing a clean first-login capture (app data wiped) with Frida hooks on `ThingApiParams` constructor + `putPostData`, the exact sequence of API actions, versions, and parameter names was recorded.

**Time wasted:** ~3 hours across multiple sessions.

### 11.4 Tuya Cloud OpenAPI

**Approach:** Use `tinytuya` (Python library) with the vendor's `appKey`/`appSecret` to access the Tuya Cloud OpenAPI.

**What happened:** `tinytuya.Cloud(apiKey=..., apiSecret=...)` returns `"clientId is invalid"`.

**Root cause:** The vendor's `appKey`/`appSecret` are for the Mobile SDK API, not the Cloud OpenAPI. These are entirely different API surfaces with different credential systems. The Cloud OpenAPI requires IoT Platform developer credentials, not mobile app credentials.

### 11.5 MMKV/SharedPreferences Token Extraction

**Approach:** Extract session tokens directly from the app's local storage on a rooted phone.

**What happened:**
- SharedPreferences: fully encrypted (keys are SHA-256 hashes, values are encrypted blobs)
- MMKV: uses Tuya's custom MMKV wrapper (`com.thingclips.smart.mmkv.MMKV`, not `com.tencent.mmkv.MMKV`) with encryption enabled
- The MMKV files when dumped and parsed show only encrypted/garbled data

**Why abandoned:** The encryption keys for MMKV and SharedPreferences are derived from the Android Keystore, making offline extraction impractical.

### 11.6 Frida Java Bridge via Python API

**Approach:** Use Frida's Python API (`frida.attach()`, `session.create_script()`) to call Java methods for signing requests — creating a "signing proxy" where the phone signs requests and the PC makes API calls.

**What happened:** `Java is not defined` error. The Java bridge is not available when using `session.create_script()` in some Frida versions. The bridge works fine when using `frida` CLI with `-l script.js`, but not via the Python API.

**Workaround:** Used Frida CLI with script output parsed by Python. Clumsy but functional.

**User's valid criticism:** *"A phone running 24/7 as an HSM to sign API requests? I'd rather buy a different camera."* This drove the effort to fully reverse the signing algorithm.

### 11.7 Brute-Forcing the Signing Key Derivation

**Approach:** With a known input-output pair (parameter string → signature), try all plausible key derivations from `appSecret`, `appKey`, and the APK certificate hash.

**Attempts (all failed):**
- HMAC-SHA256 with appSecret, appKey, or any combination
- SHA-256 with various concatenations
- HMAC with MD5 of credentials
- XOR combinations
- Certificate hash (SHA-256, SHA-1) as key or mixed with credentials
- Swapped/rearranged credential strings

**Why it failed:** The actual key includes a fourth component (`embeddedKey` from `assets/cers`) that was not part of any obvious derivation from the known credentials. Over 40 combinations were tested before pivoting to memory extraction.

**Total time:** ~1 hour of systematic elimination.

### 11.8 Frida Hooks Crashing the App

Multiple Frida hook attempts crashed the target app:

- **JSONObject constructor hook:** Hooked `new JSONObject(String)` to intercept API responses. Caused infinite recursion (the hook itself triggers JSON parsing) → stack overflow → app crash.
- **MessageDigest hook:** Successfully hooked but SHA-256 calls during signing were NOT going through Java's `MessageDigest` — the native code uses mbedTLS directly, not JNI callbacks to Java crypto.
- **Java.perform + Module operations:** Mixing Java bridge operations with native `Module`/`Memory` operations in the same callback caused access violations.
- **App PID instability:** The app's PID changed frequently (app restarts after Frida detaches ungracefully), requiring PID re-detection before each hook attempt.

### 11.9 r2ghidra Decompilation Limitations

The r2ghidra decompiler produced useful but incomplete results for the 769-line `doCommandNative` function. Key limitations:

- Variable names are meaningless (`iVar25`, `puVar15`)
- Complex JNI call chains (vtable dispatches via `[x8, #0x108]`) are not resolved
- The decompiler cannot identify which Java methods are being called through JNI
- String references are resolved but method signatures require manual cross-referencing

The decompilation was useful for understanding the high-level flow (init → cert hash → store key → sign), but the actual key construction required memory dumping rather than static analysis.

### 11.10 MQTT Credential Derivation (Multiple Failed Attempts)

**Context:** After successfully reversing the API signing algorithm (Phase 3) and login flow (Phase 4), the final obstacle was connecting to the MQTT broker for WebRTC signaling. The existing open-source tool (`tuya-ipc-terminal`) uses MQTT credentials from the Smart Life web portal (`/api/jarvis/mqtt`), which is inaccessible to vendor-namespaced accounts.

**Attempt 1 — Web portal MQTT config API:** The endpoint `smartlife.m.mqtt.config.get` and `tuya.m.mqtt.config.get` both return `API_OR_API_VERSION_WRONG` on the mobile SDK. These APIs don't exist on the mobile platform.

**Attempt 2 — Token as MQTT password:** The `smartlife.m.token.get` API returns a UUID token. Using it as MQTT password with various username formats (`web_{uid}`, `uid`, `ecode`) → all rejected with "Not authorized".

**Attempt 3 — MD5(MD5(appKey) + ecode) as password:** From the Frida login capture, the app computes this hash after login. Used as MQTT password → still "Not authorized".

**Attempt 4 — SID cookie on web portal:** Tried calling the Smart Life web portal's `/api/jarvis/mqtt` with the mobile SDK SID set as a cookie → `USER_SESSION_LOSS`. Different session systems.

**Attempt 5 — Client ID only (no username/password):** The app's MQTT client doesn't call `setUserName()`/`setPassword()` through the standard Java setters. Tried connecting with just the client ID → "Server unavailable".

**Breakthrough:** Hooked `MqttConnectOptions.getUserName()` and `getPassword()` instead of the setters. The MQTT library reads credentials through getters during connection, revealing that credentials ARE set — just through a different code path (the `SdkMqttCertificationInfo` class).

**Root cause of all failures:** The MQTT password uses `doCommandNative(cmd=2)` — a different native function than the API signing (cmd=1). Command 2 computes `MD5(MD5(signing_key) + ecode)` and returns the middle 16 characters. The signing key (not appKey or appSecret alone) is the HMAC key, which is why no combination of partial credentials worked.

**Time wasted:** ~2 hours across MQTT credential attempts, decompilation of multiple MQTT classes, and testing 20+ derivation formulas.

---

## 12. Phase 6: MQTT Credential Derivation

The final piece of the puzzle: connecting to the MQTT broker for WebRTC signaling.

### The Problem

The existing WebRTC bridge (`tuya-ipc-terminal`) obtains MQTT credentials from the Smart Life web portal. This endpoint is inaccessible for vendor-namespaced accounts. The mobile app uses a completely different credential derivation.

### Discovery Method

1. **Frida hook on `MqttConnectOptions.getUserName()` / `getPassword()`** — the standard `setUserName`/`setPassword` methods were never called. The Tuya SDK uses a custom certification info class (`SdkMqttCertificationInfo`) that provides credentials through getter methods.

2. **Decompilation of `SdkMqttCertificationInfo`** (jadx) revealed:
   - Username is constructed from `partnerIdentity`, `appKey`, `chKey`, SID, and a hash suffix
   - Password comes from `doCommandNative(context, 2, ecode.getBytes(), null, debug)`, taking the middle 16 characters

3. **Native cmd=2 reversal** (r2ghidra) showed the algorithm: two rounds of hashing with the signing key and ecode. Tested as `MD5(MD5(signing_key) + ecode)` → exact match.

### MQTT Credential Formulas

**Username:**
```
{partnerIdentity}_v1_{appKey}_{chKey}_mb_{SID}{MD5(MD5(appKey) + ecode)[-16:]}
```

**Password:**
```python
md5_key = hashlib.md5(signing_key.encode()).hexdigest()
full_hash = hashlib.md5((md5_key + ecode).encode()).hexdigest()
password = full_hash[8:24]  # middle 16 characters
```

**Client ID:**
```
{packageName}_mb_{deviceHash}_{MD5(uid + "sdkfasodifca")}_DEFAULT
```

**MQTT Subscribe Topic:**
```
{partnerIdentity}/mb/{uid}
```

### Broker Endpoints

| Transport | URL | Port |
|-----------|-----|------|
| TLS (app uses this) | `ssl://m1.tuyaeu.com` | 8883 |
| WebSocket (web portal) | `wss://m1.tuyaeu.com/mqtt` | 443 |

Both transports accept the same credentials.

### Validation

All MQTT credentials are derived from values available in the login response (`sid`, `ecode`, `partnerIdentity`, `uid`) combined with the static signing key. No additional API calls or phone access needed after login.

```python
# Complete MQTT connection — no phone, no web portal, no Frida
client = mqtt.Client(client_id=mqtt_client_id)
client.username_pw_set(mqtt_username, mqtt_password)
client.tls_set(cert_reqs=ssl.CERT_REQUIRED)
client.connect("m1.tuyaeu.com", 8883)
# → Connected successfully
```

---

## 13. Security Considerations

### Responsible Disclosure

This research targets personal devices owned by the researcher. No third-party systems were accessed. The methodology is documented to enable interoperability and consumer choice, not to compromise security.

### Credential Scope

- The signing key is static per APK version and grants no access by itself
- A valid SID (from OTP login) is required for any meaningful API call
- OTP login requires access to the registered email address
- No credentials from other users are accessible or compromised

### Recommendations for Tuya

1. **Signing key in native code:** Storing the complete key as a contiguous string in `.bss` makes memory extraction trivial. Consider computing the HMAC in-place without materializing the full key.
2. **Embedded key in assets:** The `cers` file is a static asset. If it contained per-install randomness, the signing key would not be extractable from the APK alone.
3. **et parameter:** The API accepts `et: '0.0.1'` (plaintext) even though the app sends `et: '3'` (encrypted). If plaintext mode is not intended for production, it should be disabled server-side.

---

## 14. Reproducibility Guide

### Requirements

- A Tuya whitelabel camera and its companion APK
- A rooted Android phone (for Frida one-shot key extraction)
- Python 3.10+ with `pycryptodome`, `requests`, `paho-mqtt`
- Docker (for APK static analysis)
- `jadx`, `keytool`, `frida-tools`

### Step-by-Step

**1. Extract static keys (Docker, no phone):**
```bash
docker run --rm -v /path/to/app.apk:/input/app.apk -v $(pwd):/output apk-key-extractor
# → outputs: packageName, certSHA256, appKey, appSecret
```

**2. Extract embedded key (Frida, one-time):**
```bash
# On rooted phone with app running:
frida -U -p $(adb shell pidof com.vendor.app) -e '
var lib = Process.getModuleByName("libthing_security.so");
var addr = lib.base.add(0x39070);
var len = addr.add(8).readU64();
var ptr = addr.add(16).readPointer();
var key = "";
for (var i = 0; i < len; i++) key += String.fromCharCode(ptr.add(i).readU8());
console.log("SIGNING_KEY=" + key);
'
# → outputs: complete signing key (parse with underscore separators)
```

**3. Login (Python, no phone — same flow as the vendor app):**
```python
# Step 3a: Get RSA token
token = call_api("thing.m.user.username.token.get", "2.0",
                 {"countryCode": "39", "username": "user@example.com", "isUid": False}, sid="")

# Step 3b: Encrypt password (MD5 + RSA)
md5_pass = hashlib.md5(password.encode()).hexdigest()
rsa_key = RSA.import_key(f"-----BEGIN PUBLIC KEY-----\n{token['pbKey']}\n-----END PUBLIC KEY-----")
encrypted = PKCS1_v1_5.new(rsa_key).encrypt(md5_pass.encode()).hex()

# Step 3c: Login attempt → triggers MFA
call_api("thing.m.user.email.password.login", "3.0",
         {"countryCode": "39", "email": "user@example.com", "passwd": encrypted,
          "token": token["token"], "ifencrypt": 1,
          "options": '{"group": 1, "mfaCode": ""}'}, sid="")
# → returns MFA_NEED_SEND_CODE

# Step 3d: Request MFA code (needs fresh RSA token)
token2 = call_api("thing.m.user.username.token.get", "2.0", ...)
encrypted2 = encrypt_password(password, token2["pbKey"])
call_api("thing.m.user.username.mfa.code.get", "1.0",
         {"countryCode": "39", "username": "user@example.com", "passwd": encrypted2,
          "token": token2["token"], "ifencrypt": 1,
          "options": '{"group": 1, "mfaCode": "null"}'}, sid="")
# → server sends 6-digit code to email

# Step 3e: Complete login with MFA code (needs fresh RSA token)
token3 = call_api("thing.m.user.username.token.get", "2.0", ...)
encrypted3 = encrypt_password(password, token3["pbKey"])
result = call_api("thing.m.user.email.password.login", "3.0",
                  {"countryCode": "39", "email": "user@example.com", "passwd": encrypted3,
                   "token": token3["token"], "ifencrypt": 1,
                   "options": '{"group": 1, "mfaCode": "179958"}'}, sid="")
sid = result["sid"]
ecode = result["ecode"]
partner = result["partnerIdentity"]
```

**4. Derive MQTT credentials (no additional API calls):**
```python
# Username
md5_appkey = hashlib.md5(app_key.encode()).hexdigest()
username_tail = hashlib.md5((md5_appkey + ecode).encode()).hexdigest()[-16:]
mqtt_user = f"{partner}_v1_{app_key}_{ch_key}_mb_{sid}{username_tail}"

# Password
md5_signkey = hashlib.md5(signing_key.encode()).hexdigest()
pw_full = hashlib.md5((md5_signkey + ecode).encode()).hexdigest()
mqtt_pass = pw_full[8:24]  # middle 16 chars

# Client ID
uid_hash = hashlib.md5((uid + "sdkfasodifca").encode()).hexdigest()
mqtt_client = f"{pkg}_mb_{device_hash}_{uid_hash}_DEFAULT"
```

**5. Connect MQTT and stream:**
```python
# MQTT connection
client = mqtt.Client(client_id=mqtt_client)
client.username_pw_set(mqtt_user, mqtt_pass)
client.tls_set(cert_reqs=ssl.CERT_REQUIRED)
client.connect("m1.tuyaeu.com", 8883)

# Get WebRTC config
rtc = call_api("smartlife.m.rtc.config.get", post_data={"devId": "camera_id"}, sid=sid)

# WebRTC signaling via MQTT → RTP → RTSP bridge
```

**6. Or use the bridge tool:**
```bash
./tuya-ipc-terminal direct \
  --signing-key "pkg_cert_key_secret" \
  --sid "$SID" \
  --app-key "$APP_KEY" \
  --device-id "$DEVICE_HASH" \
  --camera-id "$CAMERA_ID" \
  --camera-name "MyCamera" \
  --port 8554

# → rtsp://localhost:8554/MyCamera
```

---

*This document describes research conducted on personally-owned devices for interoperability purposes. All credentials shown are anonymized or redacted. The Tuya trademark belongs to Tuya Inc.*

---

No additional API calls or phone access needed after login.
