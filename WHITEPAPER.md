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
13. [Phase 7: LAN Protocol & DPS Push Discovery](#13-phase-7-lan-protocol-dps-push-discovery)
14. [Phase 8: Streaming Path — WebRTC Signaling and Media](#14-phase-8-streaming-path-webrtc-signaling-and-media)
15. [Phase 9: Native SDK Analysis (`libThingP2PSDK.so`)](#15-phase-9-native-sdk-analysis-libthingp2psdkso)
16. [Phase 10: The Local-Signaling Path (Zero-WAN)](#16-phase-10-the-local-signaling-path-zero-wan)
17. [Completed Architecture](#17-completed-architecture)
18. [Security Considerations](#18-security-considerations)
19. [Reproducibility Guide](#19-reproducibility-guide)
20. [Local streaming (LAN)](#20-local-streaming-lan)
21. [Stream sessions and the lullaby (issue #72): a hypothesis under test](#21-stream-sessions-and-the-lullaby-issue-72-a-hypothesis-under-test)

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
- **Local signaling needs no cloud credential at all** (section 16, captured by @leonardpitzu): the
  camera negotiates a stream over its own TCP 6668 session, with the `localKey` as the only
  cloud-derived secret and `smartlife.m.rtc.config.get` not required

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
| 554 | RTSP | Answers, but gated behind the P2P/KCP tunnel — not a consumable stream (see section 14) |
| 6000 | Tuya Auth | RTSP credential negotiation |
| 6668 | Tuya LAN | Tuya localKey control channel (DPS, commands); carries no media |
| 8686 | Unknown | Discovery/control |
| 8687 | Unknown | Discovery/control |

### App-to-Cloud Communication

The mobile app maintains three persistent connections:

1. **HTTPS** to `a1.tuyaeu.com` — API calls (REST over form-encoded POST)
2. **MQTT/TLS** to `m1.tuyaeu.com:8883` — Real-time signaling, device events
3. **TCP** to camera `192.168.x.x:6668` — Direct LAN video transport

When streaming, the app authenticates via the cloud and holds a direct LAN connection to the camera
on port 6668.

**Later correction (section 14):** port 6668 is the Tuya localKey control channel, not the video
path. The media is WebRTC: signaling over cloud MQTT, then H.264 over UDP between phone and camera.
Because the camera advertises a direct-LAN host ICE candidate, the media does stay on the LAN when
both ends are on it, which is what made this reading look right at the time.

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

**Key insight:** the video path is local while authentication and signaling go through the cloud.
The initial reading of this capture put the media on port 6668; section 14 corrects that to WebRTC
media over UDP, negotiated through cloud MQTT. The conclusion that matters for a third-party client
holds either way: after login, the frames come from the camera, not from Tuya.

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

### Data Centers and Account Region

The captures above were all taken with an Italian account, which hides a
constraint that only shows up outside Europe: **a Tuya account belongs to one
data center, and a SID issued by one is rejected by the others.**

| Region | API host | Example MQTT host | Countries |
|--------|----------|-------------------|-----------|
| Central Europe | `a1.tuyaeu.com` | `m1.tuyaeu.com` | Europe, Middle East, Africa |
| Western America | `a1.tuyaus.com` | `m1.tuyaus.com` | North and South America |
| India | `a1.tuyain.com` | `m1.tuyain.com` | India |
| China | `a1.tuyacn.com` | `m1.tuyacn.com` | China, Hong Kong, Macao, Taiwan |

The vendor app resolves this before any traffic: the country picker on its login
screen selects both the `countryCode` sent in the login payloads and the host
the SDK talks to, from a country table bundled in the APK.

Two observations matter for a reimplementation:

- The earlier steps of the login are **not** a reliable region check. Against
  the wrong data center, `thing.m.user.username.token.get` still returns a
  token and `thing.m.user.username.mfa.code.get` still emails a code; only the
  final `thing.m.user.email.password.login` fails, with `USER_SESSION_INVALID`.
  That asymmetry is what made this look like an MFA bug rather than a routing
  bug (issues #44, #58).
- The successful login response is **self-describing**. Its `domain` block
  carries the account's own endpoints, so nothing downstream has to guess:

```
"domain": {
  "mobileApiUrl": "a1.tuyaeu.com",
  "mobileMqttsUrl": "m1.tuyaeu.com",
  "regionCode": "EU",
  ...
}
```

The integration therefore maps country to data center for the login attempt,
then replaces its own guess with `domain.mobileApiUrl` once the login succeeds,
and hands that host to the bridge in the config JSON (`api_host`). MQTT is never
hard-coded on either side: the bridge reads `domain.mobileMqttsUrl`.

### SID Lifecycle

- **Issued at:** login (password + MFA, or OTP)
- **Expires after:** unknown (estimated weeks/months based on app behavior)
- **Renewal:** re-trigger the full password + MFA flow
- **Scope:** full account access (all devices, all APIs)
- **Bound to:** the data center that issued it (see above)

### Home Assistant Config Flow

The login maps naturally to a multi-step HA config flow:

1. **Step 1:** User enters email + password, and confirms the account country
   (pre-filled from Home Assistant's own country setting)
2. **Step 2 (automatic):** Integration resolves the data center, encrypts the
   password, calls login API, triggers MFA
3. **Step 3:** User checks email, enters 6-digit MFA code in HA
4. **Step 4 (automatic):** Integration completes login, stores SID plus the
   account's API host and country code
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

## 13. Phase 7: LAN Protocol & DPS Push Discovery

### Cloud MQTT — Dead End for DPS Push

After successfully reversing the MQTT credentials (Phase 6), we tested whether DPS status updates arrive on the mobile SDK's MQTT topic (`{partnerIdentity}/mb/{uid}`).

**Result: No.** The Tuya cloud MQTT broker for mobile SDK connections is used **exclusively for WebRTC signaling** (protocol 302). DPS changes made via the REST API are not forwarded to the MQTT topic. We tested with three topic variants (`{partner}/mb/{uid}`, `/av/u/{uid}`, `smart/device/out/{devId}`) — all subscribed successfully but delivered zero DPS messages during controlled tests.

The mobile app likely receives push notifications via Firebase Cloud Messaging, not MQTT.

### LAN Protocol — Working Real-Time Push

The device responds to the Tuya LAN protocol (version 3.3) on the local network. Discovery via UDP broadcast on ports 6666/6667 reveals the device at its LAN IP.

**Connection sequence:**
1. `tinytuya.deviceScan()` → finds device by `gwId` match
2. Create `Device(device_id, ip, local_key, version=3.3)` with `localKey` from API discovery
3. `set_socketPersistent(True)` + `updatedps([dps_list])` → primes the connection
4. `receive()` in a loop → blocks until data arrives or socket timeout

**DPS push behavior:**
- After `updatedps()`, the device dumps its full current state
- Device-initiated changes push in real-time (temperature fluctuations, alert events)
- Cloud API-initiated changes (commands we send) are NOT echoed on LAN
- The persistent socket drops periodically (error 904 "Unexpected Payload"); auto-reconnect handles this

*Later corrections.* `updatedps()` turned out to crash this firmware, and the integration no
longer sends it. A 3.3 session does not answer `DP_QUERY` either, so on 3.3 the starting state
comes from the cloud poll. From 2026.11.0-dev3, a session negotiated at 3.4 or newer sends one
`DP_QUERY` when it comes up and takes the answer as its starting state (see below).

### Alert Event Discovery

Controlled testing with motion and sound detection revealed the event DPS codes:

| DPS | Value | Meaning |
|-----|-------|---------|
| 250 | `"motion_detection"` | Motion detected by camera |
| 141 | `"decibel_upload"` | Sound detected above threshold |

**DPS 250** is the primary alert event channel. Motion events arrive on this DPS even when DPS 134 (motion switch) is set to `false` — the switch controls whether the app shows notifications, not whether the device detects motion.

**DPS 141** fires when the microphone picks up sound above the configured sensitivity threshold.

Both events arrive via LAN push with sub-second latency, making them suitable for HA automations (e.g., turn on a light when motion is detected, send a notification when crying is heard).

### Updated DPS Reference

| DPS | Name | Type | Push via LAN? |
|-----|------|------|---------------|
| 207 | Temperature (°C × 100) | int | Yes |
| 250 | Alert event | string | Yes — `"motion_detection"` |
| 141 | Sound event | string | Yes — `"decibel_upload"` |
| 138 | Night light | bool | Yes (initial state) |
| 134 | Motion switch | bool | Yes (initial state) |
| 139 | Sound switch | bool | Yes (initial state) |
| 158 | Brightness | int | Yes (initial state) |
| 246 | Lullaby state | string | Yes — `"playing"` / `"stopping"` |
| 101 | Unknown status | bool | Yes |
| 209 | Volume | int | Initial state only |
| 248 | Lullaby track info | JSON string | Initial state only |

### Integration Architecture

```
Baby Monitor (LAN, 192.168.85.x)
    │
    ├── Tuya LAN protocol 3.3 ──► TuyaLANClient (lan.py)
    │   Real-time push: temperature, motion, sound       ──► Coordinator
    │                                                          │
    └── Tuya Cloud API (a1.tuyaeu.com) ──► PhilipsAventAPI    │
        Polling every 30 s or 120 s; paused behind a     ──► Coordinator
        3.4+ session (see below)                               │
                                                          All HA entities
```

The diagram shows 3.3, which is what this work started on. Since 2026.11.0-dev3 the integration
follows a fork measurement: on a session negotiated at 3.4 or newer, the monitor carries its state
and its alarms (DPS 212) on its own. Leonard Pitzu's fork measured it on an SCD973/26 at 3.5. The
gate is the version the live session negotiated. A monitor that announces 3.5 but fell back to a
3.3 session therefore gets the 3.3 behaviour.

- **3.4 and newer.**
  - The session sends one `DP_QUERY` when it comes up. The answer seeds state, but it is not
    treated as news, so a retained alert value cannot fire a sensor on reconnect.
  - Once the monitor has reported an alarm in DPS 212, the cloud poll pauses while the session is
    up, and a command the LAN delivered is not repeated through the cloud.
  - The cloud is still asked for what only it knows: the WiFi signal every 5 minutes, and on
    SenseIQ monitors the nightly summary every 15 minutes.
  - Such a monitor polls on the 30 s tick, so the cloud poll is back within 30 s of the session
    dropping.
  - A 3.4+ monitor that has never reported DPS 212 may raise its alerts in 250 and 141, which
    nothing shows pushing, so it keeps the poll and the cloud copy of commands.
  - Turning one alert detector on turns the other off, since the monitor runs one at a time.
    This one is gated on the confirmed protocol published to the bridge rather than on the live
    session.
- **3.3.**
  - The cloud poll continues as before, and so does the cloud copy of each command (except
    relative lullaby steps, which were never copied). Alerts arrive only through the poll.
  - LAN pushes no longer postpone that poll. Before dev3, every push rescheduled it a full
    interval out, so a SenseIQ status every few seconds kept it from running at all.
- **Every protocol.**
  - A push the keep-alive read in place of its acknowledgement is published, where it used to be
    dropped. In one test on the fork's 3.5 monitor, that had cost about half of the sound alarms.
  - A write whose socket never opened (tinytuya errors 905 and 914) goes through the cloud. It
    used to count as sent.
  - After an ambiguous error (901, 902), a relative lullaby step is not sent again, because it
    may already have reached the monitor. An absolute value is sent again through the cloud.
  - While a session that has proven its key is up, a failing cloud poll leaves the entities
    available, with one warning and one recovery line. On 3.3 a session proves its key with its
    first decrypted push.
  - The exception is a monitor whose alerts come only through the poll: one below 3.4, or a 3.4+
    monitor that has not reported DPS 212. There the motion, sound and crying sensors read
    unavailable until the cloud answers, because they cannot hear anything in the meantime.

---

## 14. Phase 8: Streaming Path — WebRTC Signaling and Media

*This phase was contributed by @leonardpitzu in [issue #51](https://github.com/thekoma/aventproxy/issues/51), from work on the sibling `aventlocal` project. The findings are reproduced here with light edits: section cross-references renumbered, and one note added where the capture and this project's working bridge disagree.*

Authentication (sections 6 to 8) yields API access; this phase documents how the **video itself** is obtained. The camera streams over **WebRTC**, negotiated in two independent phases:

1. **Signaling** — a short SDP **offer/answer + ICE-candidate** exchange. The official app carries it over the **Tuya cloud MQTT** broker. This is the only part that touches the internet.
2. **Media** — once ICE picks a path, H.264 video flows **peer-to-peer over UDP**. The camera advertises a **direct-LAN host candidate** (e.g. `192.168.0.15:<port>`), so on the same LAN the media never leaves the network.

### Cloud signaling frame format

```
publish  → /av/moto/<motoId>/u/<deviceId>      # offer + local ICE candidates → camera
subscribe← /av/u/<uid>                          # answer + camera ICE candidates ← camera

{ "protocol": 302, "pv": "2.2", "t": <ms>,
  "data": {
    "header": { "type": "offer|answer|candidate|disconnect",
                "from": <msid>, "to": <deviceId>, "sub_dev_id": "",
                "sessionid": <32-hex>, "moto_id": <motoId>, "tid": "", "seq": 0, "rtx": 0 },
    "msg":    { "mode": "webrtc", "sdp": "...", "auth": <from rtc.config.get>,
                "token": [<ice servers>], "stream_type": 1 } } }
```

The publish topic matches this project's bridge exactly (`pkg/tuya/mqttCamera.go`). For the
subscribe topic the capture reads `msid`; the bridge subscribes with the account `uid` from
`smartlife.m.user.info.get` and receives answers reliably, so `uid` is the tested value here.
`msid` is a different derived string (see section 12), which suggests either the app uses one and
we use the other, or the broker accepts both.

A later note, from the issue #72 work (section 21): the app's own offers are probably `imm`, not `webrtc`. Its P2P SDK only *accepts* `webrtc` offers, from third-party clients. The frame above probably came from the `aventlocal` client, which used aiortc. Read its `mode` as what a third-party client sends. This is inference from the app's libraries, not a capture. The libraries also show where the app sends its own frames: on the device's own MQTT channel (`smart/mb/out/<devId>`, encrypted with the local key), rather than on the `/av/moto` topic above (section 21).

`msg.token` is the **ICE-server list** (`rtc.config.get → p2p_config.ices`), not a string. `auth`, `motoId`, and the ICE servers all come from the `smartlife.m.rtc.config.get` call introduced in section 9.

### Two media modes

| Mode | Codec / transport | Crypto | Consumable by |
|------|-------------------|--------|---------------|
| `webrtc` | RTP/H.264 over **DTLS-SRTP** (standard WebRTC) | SRTP keys from the DTLS handshake | any standard stack (`aiortc`, `pion`) |
| `imm` | Tuya `AES/KCP` custom codec (`m=application 9 imm 6001`, `a=rtpmap:6001 AES/KCP 330`) | AES-128, key shipped plaintext in SDP `a=aes-key` | custom decoder |

Over the **cloud** path, requesting `mode: webrtc` yields an ordinary DTLS-SRTP/H.264 stream, which is what this project's bridge does and why `pion` decodes it with no proprietary cipher.

### Correction: the LAN path does not offer DTLS-SRTP

The captured local session (section 16) never negotiates DTLS-SRTP. Its offer is `m=application 9 imm 6001` with `a=rtpmap:6001 AES/KCP 330`, and the answer comes back `AES/KCP 3`. The `mode` field is **absent entirely** from a LAN offer: the codec is carried in the m-line instead. So `imm` is not merely an alternative documented for completeness, it is what the local path gives you.

The saving grace is that `a=aes-key:` sits in the SDP in cleartext, so `imm` is decodable without reversing a key schedule. Anyone implementing local streaming should plan for AES/KCP rather than expecting to reuse a standard WebRTC stack.

### Correction: port 554 is not a shortcut

The reconnaissance note (section 3) that 554 carries "AES-encrypted RTSP" was tested to conclusion: the port answers `400 Bad <method>` to every standard RTSP verb and path/auth combination. It is a **P2P/KCP-tunnel-gated** endpoint, not a standalone consumable RTSP server. The real local stream is the WebRTC media leg above, not port 554.

## 15. Phase 9: Native SDK Analysis (`libThingP2PSDK.so`)

The streaming engine is the native P2P SDK (`libThingP2PSDK.so`, ARM64), pulled from the device and analyzed statically (`nm -D`, `strings`, `objdump`).

**Media is off-the-shelf WebRTC — confirmed by symbols:**
- libsrtp: `srtp_protect`, `srtp_unprotect_aead`
- mbedTLS DTLS: `mbedtls_ssl_*`, `mbedtls_x509write_crt_*`, `mbedtls_ctr_drbg_seed`
- `imm_p2p_rtc_sdp_set_dtls_cert_fingerprint`, plus a full SDP builder API: `imm_p2p_rtc_sdp_{add_media,add_video_codec,add_audio_codec,add_candidate,set_aes_key,get_aes_key}`

This is decisive for the cloud path: the `webrtc` mode is genuine DTLS-SRTP that `aiortc` decodes natively. Note that on the **local** path the choice does not exist, as the captured LAN offer carries no `mode` field and negotiates AES/KCP (sections 14 and 16).

**Signaling command surface (string-extracted JSON templates):**

```jsonc
{"cmd":"connect_v3","args":{"remote_id":"%s","dev_id":"%s","token":%.*s,
   "trace_id":"%s","timeout_ms":%d,"lan_mode":%d,"preconnect_enable":%d,
   "connect_session":"%s"}}
{"cmd":"signaling_result","args":{"code":%d,"remote_id":"%s","signaling":%.*s}}
{"cmd":"retransmit_signaling","args":{"sessionid":"%s","remote_id":"%s","path":"%s"}}
```

The cloud-vs-LAN selector inside the SDK is the integer **`lan_mode`** flag on `connect_v3`/`connect_v2`/`connect` — not the `header.path` string (the only `path` literals in the binary are `mqtt` and `relay`). Threads `create signaling lan thread` and `create signaling mqtt worker thread` confirm two distinct signaling transports behind one API.

## 16. Phase 10: The Local-Signaling Path (Zero-WAN)

*Captured live and contributed by @leonardpitzu in [issue #51](https://github.com/thekoma/aventproxy/issues/51). This section previously described a static, partly wrong reconstruction; the capture replaces it and the corrections are called out below so the earlier reasoning stays traceable.*

The official app keeps streaming with the camera's WAN unplugged. The capture that settles how: camera blocked from WAN at the gateway, app streaming throughout, traffic taken **on the access point the camera associates with** and decrypted offline with the device `localKey`.

**The headline: local signaling needs no cloud credential.** The offer carries no `auth`, no `mode` and no `stream_type`, and `moto_id` is empty. `smartlife.m.rtc.config.get` is not required to negotiate a stream. The only cloud-derived secret anywhere in the flow is the `localKey`, which is a one-time fetch.

### The wire sequence

All of it on **TCP 6668**, one connection:

| # | Direction | Tuya seq | Cmd | Frame | Payload |
|---|-----------|----------|-----|-------|---------|
| 1 | app → cam | 1 | 3 | `SESS_KEY_NEG_START` | `local_nonce[16]` |
| 2 | cam → app | — | 4 | `SESS_KEY_NEG_RESP` | `remote_nonce[16]` + `hmac[32]` |
| 3 | app → cam | 2 | 5 | `SESS_KEY_NEG_FINISH` | `hmac_sha256(localKey, remote_nonce)` |
| 4 | app → cam | 3 | 10 | `DP_QUERY` | primes the session |
| 5 | cam → app | — | 10 | reply | ~616 B |
| 6 | app → cam | 4 | 32 | `IPC_LAN_302` — `type: "offer"` | ~1633 B |
| 7 | cam → app | — | 32 | `IPC_LAN_302` — ACK | 32 B |
| 8 | cam → app | — | 32 | `IPC_LAN_302` — `type: "answer"` | ~1246 B |
| 9 | both | 5…14 | 32 | `IPC_LAN_302` — `type: "candidate"` | trickle, both directions |
| 10 | cam → app | — | 8 | `STATUS` | DPS pushes |

**Offer shape** (values redacted):

```jsonc
"header": { "from": "<account uid>", "to": "<devId>", "moto_id": "",
            "path": "lan", "type": "offer", "is_pre": 0,
            "p2p_skill": 1635, "security_level": 3,
            "sessionid": "<devId><epoch><8 rand>",
            "trace_id": "<UUID>_<devId>_<epoch_ms>" }
"msg": { "sdp": "...", "preconnect": true, "log": {...},
         "token": [ ...stun/turn... ], "tcp_token": {...} }
```

The answer is the same shape with `p2p_skill: 1123` and `type: "answer"`.

`token` and `tcp_token` are present but pointed at cloud STUN/TURN and a `tcp4:...:1443` relay that were unreachable for the whole capture. ICE probed them, failed, and settled on the direct LAN host pair: media ran `cam:47154 → phone:50218`, 753 packets, no cloud involvement. So those blocks look carriable-but-ignorable rather than required. Whether the camera rejects an offer that omits them is untested.

### Correction: why the earlier frametype-32 attempt only got an ACK

The previous version of this section blamed `ThingSmartP2PSDK::SendMessageThroughLAN` for enumerating interfaces with `getifaddrs` and answering on the SDK's own UDP socket. **That does not hold.** Filtering the capture for it: the only TCP between phone and camera is 6668, the phone's only UDP to the camera is the media port pair itself, and the answer comes back on the same 6668 session. There is no LAN UDP signaling port to find.

The decompiled chain agrees, and there is no socket in `libThingP2PSDK.so` at all:

```
SendMessageThroughLAN(devId, json, len)                      # native → Java, a JNI upcall
  → P2PMQTTServiceManager.send302MessageThroughMqtt(byLan=true, devId, json)
  → homeCamera.lan302Publish(devId, json, cb)
  → ThingSmartDevice.lanControl(devId, json, FrameTypeEnum.IPC_LAN_302.type=32, cb)
answer ← registerDeviceHardwareResponseListener(FrameTypeEnum.IPC_LAN_302.getType(), ...)
```

The real reason the earlier attempt failed is simpler: **it published cold.** The camera answers only on a session that has been through `SESS_KEY_NEG 3 → 4 → 5` and then primed with `DP_QUERY(10)`. The 32-byte cmd-32 frames that test saw are exactly the ACK at step 7 above. It stopped one step short rather than being wrong about the frame type.

### What the local channel does not carry: alert events

A second capture answered the obvious follow-up, whether a correctly negotiated
local session would also deliver motion and sound alerts. **It does not.** The
local channel carries the alert *configuration* and none of the alert *events*.

The method matters here, because the first attempt was invalid: with the camera's
WAN cut, an alarm record referencing an uploaded snapshot cannot be produced at
all, so a negative result could not distinguish "the LAN never carries it" from
"no alarm was ever recorded". The controlled run restored WAN, which makes the
cloud path a ground truth running in parallel with the LAN capture. Same trigger,
three observers:

| Channel | Saw the event? |
|---------|----------------|
| Phone app push notification | yes |
| LAN `STATUS(8)` on 6668 | no, one frame carrying `248` only |
| Integration's cloud poll | no, the binary sensor never moved |

Grepping the whole decoded capture for `212`, `250` and `141` returns nothing.
The DP set the camera answers `DP_QUERY` with says the same:

| Present on the LAN | Absent from the LAN |
|--------------------|---------------------|
| `134` motion switch, `139` sound switch | `212` alarm record |
| `106` / `140` sensitivities | `250` alert event |
| `237` privacy, `248`, `207` temperature, lullaby DPs | `141` decibel event |

Full LAN `DP_QUERY` key set, for the record:

```
1 2 5 7 8 10 11 12 13 16 21 101 106 134 138 139 140 158 168 169
203 207 209 231 233 234 237 239 241 243 244 246 247 248 251 252 253
```

**Correction from a later test.** The capture above was taken while the observing
Home Assistant box held a **3.3** session. On a session negotiated at **3.5**,
`212` *does* arrive as a push: an SCD951 owner on 2026.7.0-rc9 measured a motion
record pushed with the sensor firing 1.3 s later, against 35 s through the cloud
poll ([issue #61](https://github.com/thekoma/aventproxy/issues/61)). Both
observations hold together: `212` is absent from the `DP_QUERY` snapshot, and a
3.3 session cannot read the frames that carry it, so it looked absent entirely.

Sound on that same monitor still arrived through the poll rather than a push, so
the picture is not uniform even within one device. Polling remains the floor:
`212` holds one slot, briefly, and nothing guarantees a poll lands inside it.

**Later measurements, from Leonard Pitzu's fork** (an SCD973/26 at 3.5):

- From 3.4 up, three motion records were pushed in `212` at 2.4, 0.2 and 0.6 s after the event,
  with no poll involved. The same monitor left `250` and `141` empty throughout. Which key carries
  alarms follows the firmware, not the model.
- Asked over 3.5, the monitor answers `DP_QUERY` with all 37 keys listed above. Its values matched
  what the cloud reported at the same moment.
- The monitor runs one detector at a time. Whichever of `134` and `139` was written last is the
  one running.
- Our own keep-alive was losing pushes. tinytuya's `heartbeat(nowait=False)` returns the first
  frame that arrives, and a push landing in that window was dropped in place of the
  acknowledgement. On the fork's monitor, of ten sound alarms in eight minutes, five arrived as
  pushes and four only on the next poll, 16 to 29 s late. Fixed in 2026.11.0-dev3 (`088dbe2`).

The last two points may explain the sound alerts on the SCD951 above, which arrived through the
poll: they were either one detector out of two or pushes lost to the keep-alive. Nobody has
checked that on that monitor.

With these, a 3.4+ session can stand in for the cloud poll, and from dev3 it does so for monitors
that report alarms in `212` (section 13). Polling stays the floor on 3.3.

**Where the app learns about alerts is still open.** In the controlled run the app
was notified while neither the LAN session nor the cloud poll saw anything, which
points at Tuya's push-notification service. That has not been reverse engineered.

### Consequences for this project

- **The camera fans cmd-32 frames out to every connected LAN client.** A Home Assistant box holding a tinytuya session was receiving copies of the answer and candidate frames and discarding them. If the integration ever wants the LAN signaling path, the traffic is already being handed to it. This is also the likely identity of the `Unexpected Payload from Device` frames behind [issue #62](https://github.com/thekoma/aventproxy/issues/62): real frames, on a session negotiated at a version that cannot read them.
- **The camera announces `"version": "3.5"`** in its 6667 discovery broadcast (6699/GCM framing). `lan.py` built a 3.3 session and discarded the announcement until 2026.7.0-rc9, which tries the announced version and falls back to 3.3.
- **A LAN signaling path would route around `smartlife.m.rtc.config.get` entirely.** That call is what fails with `PERMISSION_DENIED` on [issue #48](https://github.com/thekoma/aventproxy/issues/48) for an account whose session is otherwise fine, and it is also what ties streaming to the regional server holding the account.
- **The 904 frames have a concrete mechanism.** The camera's `STATUS` pushes arrive as `{"protocol":4,"t":…,"data":{"dps":{…}}}` behind a **15-byte 3.5 version header**. A decoder that only knows 3.1 through 3.4 falls through to hex on them, and tinytuya constructed with `version=3.3` reports them as unparseable, which is exactly the `Unexpected Payload from Device` seen on #62.
- **Local control is the user-facing prize, not local streaming.** With the camera's WAN cut, streaming kept working while control did not: the app reported the camera offline and switch changes never applied. A correctly negotiated local session should let `set_dps` keep working with no internet, which is a visible win independent of the signaling work. The integration has sent commands over its LAN session since before this capture. From 2026.11.0-dev3 it stops repeating them through the cloud when a 3.4+ session is up and the monitor reports its alarms in DPS 212 (section 13).

## 17. Completed Architecture

With the streaming path mapped, the end-to-end local pipeline is:

```
cloud login + smartlife.m.rtc.config.get   (one-time; motoId, auth, ICE servers, MQTT creds)
        │
        ▼
pluggable signaling transport
   ├─ CloudMqttSignaling   offer/answer over Tuya MQTT          ✅ implemented
   └─ LanSignaling         offer/answer over the SDK LAN socket  🚧 pending the section 16 capture
        │
        ▼
aiortc PeerConnection (DTLS-SRTP, H.264)  ← direct-LAN host ICE pair, media stays local
        │
        ▼
RTSP re-publish (ffmpeg/go2rtc)  →  Home Assistant / VLC
```

The reference implementation lives in the sibling `aventlocal` project; its `SignalingTransport` ABC is the seam that lets `LanSignaling` slot in behind the same interface once the section 16 capture is done, with the media→RTSP pipeline unchanged. Because login + `rtc.config.get` is the *only* remaining cloud touch, caching that config (or serving it locally) removes even that step — yielding true zero-internet operation.

---

## 18. Security Considerations

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

## 19. Reproducibility Guide

### Decrypting a local session from a packet capture

*Contributed by @leonardpitzu in [issue #51](https://github.com/thekoma/aventproxy/issues/51). Each of these cost real time, so they are written down rather than rediscovered.*

- **Capture on the access point, not on a neighbouring host.** The AP bridges station-to-station traffic locally, so phone-to-camera frames never reach a third party on the same subnet.
- **`tinytuya.parse_header` rejects frames larger than `MAX_PAYLOAD_LENGTH`.** The offer is 1633 bytes and trips it, so a naive frame walker silently skips the single most interesting frame and the sequence numbers merely appear to jump from 3 to 5. Pass `header=` into `unpack_message` as well, which otherwise re-parses and re-applies the same ceiling.
- **For 6699/GCM frames a wrong key does not raise.** `unpack_message` returns `crc_good=False`. Select the key by `crc_good`, not by catching exceptions.
- **Camera→app frames carry a 4-byte retcode and app→camera frames do not.** So `local_nonce` sits at offset 0 in cmd 3 while `remote_nonce` sits at offset 4 in cmd 4. Get that wrong and you derive a plausible-looking session key that decrypts nothing. Self-test: `RESP[16:48] == hmac_sha256(localKey, local_nonce)`.
- **A working decoder lives in `tools/lan302_decode.py`.** It takes a pcap and a `localKey`, reassembles the TCP streams, replays the `SESS_KEY_NEG` exchange to recover the session key, and pretty-prints every frame with `IPC_LAN_302` flagged.
- **Session key, the 3.5 derivation:** `AES(localKey).encrypt(xor(local_nonce, remote_nonce), pad=False, iv=local_nonce[:12])[12:28]`, with GCM AAD being frame bytes `[4:18]`.

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

## 20. Local streaming (LAN)

Section 16 showed that the official app can stream with the monitor cut off from the internet. Leonard Pitzu ([@leonardpitzu](https://github.com/leonardpitzu), [#51](https://github.com/thekoma/aventproxy/issues/51)) worked out how, down to the wire format, in his fork's [PROTOCOL.md](https://github.com/leonardpitzu/aventproxy/blob/main/PROTOCOL.md), sections 14 to 16 and 20 to 21. The bridge now has a port of that path, hardened. This section is the short version. His document has the byte-level detail and is the reference.

What the local path changes is the signalling, not where the video travels. A cloud-signalled session is negotiated through Tuya's servers (`rtc.config.get` and the MQTT offer and answer). Its media still comes straight from the monitor over the LAN whenever ICE picks the monitor's host candidate, as section 14 describes for the app. A capture on the maintainer's SCD973 confirmed it: 389 media packets in 6 seconds from the monitor's LAN address, and none through the TURN relay. The gain of the local path is a stream that starts with no cloud call at all: no session, no MQTT, no `rtc.config.get` per stream. It keeps working through a cloud or WAN outage, and with an expired session. The bridge log names each path by its signalling: `Streaming <camera> with cloud signalling` or `Streaming <camera> with LAN signalling`.

### The local path

Inputs are the camera's `localKey`, its P2P password (from `smartlife.m.rtc.config.get`, fetched once per setup) and its LAN address. The address comes from the integration's own tinytuya session and is written to the bridge config, together with the protocol version that session negotiated. The bridge does no discovery of its own.

The path below speaks only Tuya LAN protocol 3.5, so the bridge skips the LAN for anything older. Monitors on 3.3 (seen on an SCD973 with firmware 1.4.0) ignore 3.5 frames; they keep using the cloud, and the bridge log says `Not using LAN signalling for <camera>: protocol 3.3`. The fork kept them off the LAN with a 3.5 check in its own discovery, which the port dropped, so the integration now passes the version along.

Signalling runs on TCP 6668, in Tuya 3.5 framing (prefix 0x6699, AES-GCM). The AES-GCM IV is the ASCII bytes of the trace id, not random bytes. The offer names a placeholder STUN server, and the monitor never uses it. The monitor is the ICE-controlling side, and its STUN binding success responses carry no MESSAGE-INTEGRITY, so a stock ICE stack rejects them. The bridge therefore runs a small hand-rolled ICE: as the controlled side it sends its own checks, answers the monitor's requests and treats the first success response as connected. There is a single pair, so nothing is nominated.

Media is KCP over that UDP pair. Each datagram is AES-CBC encrypted and carries an HMAC. Frames inside the KCP stream have a sub-header in one of two forms, and the bridge reads both. Video is H.264. Audio is 16 kHz PCM, which the bridge downsamples to 8 kHz and encodes as mu-law, so RTSP consumers see the same description as on the cloud path: H.264 video and PCMU/8000 audio.

One condition is outside our control. The monitor must have reached the internet once since it booted. After that a WAN outage does not stop LAN streams. A monitor that booted with no internet answers the offer but sends no media.

### Hardening

The bridge counts a LAN attempt as working only when an H.264 SPS or IDR frame arrives within 4 seconds. A connection that negotiates and then stays silent would otherwise leave a black stream reported as healthy. If the check fails, the same start falls back to the cloud and that camera skips the LAN for 5 minutes. A LAN stream that dies later restarts on the cloud, with the same cool-down. The cool-down ends early when cloud signalling fails as well: with no working cloud there is nothing to wait for, so that start clears it and the next start tries the LAN again.

When the 5 minutes are over, a stream running on the cloud probes the LAN in the background. If the probe works, its viewers are reconnected onto the LAN, with one short reconnect.

Each camera's last published LAN address, with its protocol, is kept in the bridge config and seeded from the previous file. Rediscovery at the same address does not rewrite it, and losing the address never does either, so a blip on the integration's LAN session cannot restart the bridge. If the cloud is unreachable when the bridge starts, it still serves LAN streams using its stored session. Its warning says `Cloud unreachable, …` only for a network error. An expired session or an API error reads `Cloud check failed, serving streams with LAN signalling: …`. With LAN streaming off, the config carries no local key or P2P password, so there are no LAN targets. The bridge then exits on a failed cloud check, and `run.sh` restarts it every 5 seconds until the cloud answers, as before LAN streaming existed.

Talkback and `/sd` streams stay on the cloud. An HEVC monitor tries the LAN, never delivers an H.264 SPS or IDR, hits the 4 second deadline, falls back to the cloud and skips the LAN for 5 minutes. This has only been tested on an SCD973/26.

## 21. Stream sessions and the lullaby (issue #72): a hypothesis under test

*Nothing in this section has been confirmed on a monitor yet. It comes from the logs on [#72](https://github.com/thekoma/aventproxy/issues/72), from reading the Philips app's native libraries (app 1.9.1: strings and disassembly, no capture) and from our own code. Thanks to DeutscheMark and bojanmk1, whose reports and timings on #72 made it traceable.*

### What is observed

Opening the camera in Home Assistant pauses a lullaby that is playing. The lullaby comes back about 30 seconds after the last viewer leaves. That is the bridge's shutdown delay before it sends the session's `disconnect`. A snapshot is enough to trigger it, because Home Assistant takes a snapshot by reading a frame from the RTSP stream, which opens a full session. The app's live view leaves the lullaby alone, and the app's talk button pauses it.

In the logged sessions, DPS 253 went to `1` 100 to 200 ms after the WebRTC connection came up, and went back to `0` at the end. One session showed no change, which nobody has explained yet. In the product schema, DPS 253 is named `app_talking`. Earlier rounds ruled out three suspects:
- Talk audio from Home Assistant: no client ever sent any.
- The offer's audio direction: 253 flipped with `sendrecv` (rc9) and with `recvonly` (dev1).
- The RTSP backchannel: 253 flipped with none set up (dev2).

Neither the integration nor the bridge writes a DPS when a stream opens. The audible effect seems to depend on the firmware: on the maintainer's SCD973 (firmware 1.4.0), 253 flips as well, yet the lullaby was heard to keep playing.

### The hypothesis: the session mode

The bridge negotiates its cloud sessions in `"mode": "webrtc"` (section 14). As far as its libraries show, the app never does. In `libThingP2PSDK.so` the only `webrtc` string sits in the handler for *incoming* offers, which is how a browser or a smart display gets a session. The app itself offers `imm`, and then starts a preview with explicit control commands. `libThingCameraSDK.so` frames those commands as `magic 0x12345678 | requestId u32 | 0 u32 | cmd u16 | sub u16 | len u32 | payload`. Disassembly maps the numbers:

| cmd | Function |
|-----|----------|
| 2 | `QueryAudioInfo` |
| 3 | `QueryPlaybackInfoByMonth` |
| 4 | `QueryVideoStreamParams` |
| 5 | `QueryVideoClarity` |
| 6 | `IOCtrlVideo` / `IOCtrlAudioMic` (the operation is in the payload; 4 is the microphone) |
| 7 | `IOCtrlPlayBack` |
| 8 | `IOCtrlAudioSpeaker`, used by `StartAudioTalk` / `StopAudioTalk` |
| 9 | `IOCtrlSetVideoClarity` |
| 10 | `SendAuthorizationInfo` |

`StartPreview` sends commands 6, 9 and 6 with sub 4. It never sends 8, which only the talk button sends. A webrtc-mode client has no way to send any of these. The hypothesis is that the monitor therefore opens its speaker for the whole session, as it would for a parent talking: 253 reads `1` and the lullaby pauses.

The local path (section 20) supports this reading. Its six startup control messages (`pkg/lan/media.go`) decode with the same table:

| # | requestId | cmd | Payload | Meaning |
|---|-----------|-----|---------|---------|
| 1 | 0 | 10 | `00000100` | authorisation |
| 2 | 2 | 2 | 0 | `QueryAudioInfo` |
| 3 | 0x00010004 | 9 | {0, 2} | set video clarity |
| 4 | 0x00010003 | 6 | {0, 0} | video start |
| 5 | 0x00010005 | 6, sub 4 | {0, 4} | microphone audio start |
| 6 | 6 | 5 | 0 | `QueryVideoClarity` |

That is `StartPreview`, plus two queries and the authorisation, with no command 8. So a stream with LAN signalling (3.5 monitors only) is probably not affected, although nobody has tried it with a lullaby playing. One correction to the fork's PROTOCOL.md also follows: `0x00010003` and `0x00010005` read better as the requestIds of the video and audio start requests, which the monitor echoes at the head of every media message, than as "the video and audio commands".

### The experiment (2026.11.0-dev3, Dev card only)

The Dev card's `speaker_off` option is on by default. With it, once a cloud session without talkback connects, the bridge sends the 312 `speaker` control with `cmdValue: 0`. It is presumably the webrtc-mode counterpart of command 8. `SendSpeaker` came with the tuya-ipc-terminal import and had never been called. The bridge logs `Asked <camera> to keep its speaker off (issue #72)`.

Three outcomes are possible:
- the firmware ignores the control;
- it fixes the problem;
- it mutes the viewer's audio instead, if Tuya's "speaker" means the listening side.

Every cloud session therefore logs what it negotiated (`Offer to …`, `Answer from …`) and, when it ends, `Cloud session for <camera> carried <V> video and <A> audio packets`. An always-on consumer such as Frigate never ends its session, so the bridge also checks once, 10 seconds after the request. It logs `Audio from <camera> still flows with its speaker kept closed: <n> packets in 10s` if audio still arrives. If none does, it logs a warning that says to turn `speaker_off` off. A lullaby that keeps playing while audio keeps flowing would confirm the hypothesis. The second option, `webrtc_audio: none`, leaves audio out of the offer altogether. It tells whether the monitor needs a negotiated audio section before it opens its speaker. The stable and beta cards send no speaker command.

If the speaker control does not help, the structural fix for 3.3 monitors would be to send the app's own `imm` offer over cloud MQTT and reuse the local path's KCP/AES media. That way every session would look like the app's.

### imm over cloud signalling (2026.11.0-dev4, Dev card, experimental)

*Untested on hardware. Everything below comes from disassembling the app's `libThingP2PSDK.so` and decompiling single Java classes (app 1.9.1), and from the LAN capture in section 16.*

The Dev card's `cloud_transport: imm` option builds that fix. For a stream that does not take the LAN, the bridge first sends an imm offer through the cloud broker, then runs the local path's ICE, KCP, login and media code. If that fails, the same start falls back to the webrtc session and the camera skips imm for 5 minutes. Nothing promotes a running webrtc stream back to imm, so behind an always-on consumer the camera stays on webrtc until that consumer reconnects.

**The offer is the LAN one, with a different path.** The P2P SDK builds every offer and answer in one routine: header `from`, `to`, `sessionid`, `moto_id`, `type`, `trace_id`, `is_pre`, `p2p_skill`, `security_level`; msg `sdp`, `preconnect`, `token` (the ICE servers), and `udp_token`, `tcp_token` and `log` when it has them. It never adds `mode`, `auth` or `stream_type`. Its send routine then adds `path`: `lan` on the 6668 session, `mqtt` through the cloud. The receiving parser treats an offer whose `mode` is not `webrtc` as imm, and reads a missing or unknown `path` as `mqtt`. So the bridge sends the offer of section 20 with `path: "mqtt"`, the real `moto_id` and the ICE servers from `rtc.config.get`. Candidates trickle as separate `candidate` frames, as on the LAN, and may also appear in the answer's SDP. The bridge reads both, and uses only the monitor's IPv4 host candidates: the first version reaches the monitor on the local network, with no relay.

**Two routes, moto first.** The app does not publish these frames on `/av/moto/<motoId>/u/<devId>`. Its Java layer (`P2PMQTTServiceManager.send302MessageThroughMqtt` → `IThingHomeCamera.publish` → `IMqttServer.publishDevice`, decompiled from `classes9`/`classes14`) sends them on the device's own channel: out on `smart/mb/out/<devId>` at QoS 0, answers in on `smart/mb/in/<devId>`, each frame encrypted with the device's local key. With pv 2.2 a frame is `"2.2"`, a big-endian CRC32 of the rest, a 4-byte counter, a 4-byte random tag, then AES-ECB with PKCS#7 padding over `{"data": <the SDK frame>, "protocol": 302, "t": <seconds>}`. With pv 2.3 it is `"2.3"`, the counter, the tag and a zero byte, used as the AAD of an AES-GCM ciphertext with its 12-byte nonce in front. The app picks the framing from the device's pv; it reads either one, or plain JSON, by the first bytes. The bridge sends the offer on the `/av/moto` topic its webrtc sessions use first, because that route is known to reach the monitor's P2P SDK. If no answer comes within 3 seconds, it sends the same offer, with the same session id, on the device channel too, followed by the candidates sent so far. It reads its answer on both routes, and the log names the route that answered. The monitor's SDK drops a repeated offer for a session it already has, so the two routes cannot make two sessions. The device channel is a separate MQTT connection under its own client id, one per account and shared by its cameras, with one subscription per camera, as the app runs one connection per tag for all its devices. The first camera that needs it opens it, and it stays open while any session uses it. A broker that refused the publish could drop only that connection, never the one the webrtc sessions share. The device's pv comes from `tuya.m.device.get`, with 2.2 when it is missing. Which route the monitor answers on is still untested.

**The media needs two credentials,** the local key and the P2P password, for the login `md5(password + "||" + localKey)`. `rtc.config.get`, which every cloud session already calls, returns both, so the bridge does not depend on the integration's LAN data. The session sends the same six startup messages as the LAN path: `StartPreview`, with no talk command 8. It sends no 312 `speaker` request either, because that is a webrtc-mode control. Like the webrtc session, it logs its audio after 10 seconds and its message counts when it ends.

**Reading the log.** A working session says `Streaming <camera> with imm over cloud signalling`. `Answer from <camera> via moto` or `via device channel` says which route worked, and `No answer from <camera> via moto in 3s` marks the switch. A failed one logs one warning naming the step: `cloud setup`, `refused` (the monitor sent a disconnect), `no answer` (on either route), `answer unusable`, `answer without usable candidate`, `ICE timeout`, `login refused or one-way path` (no reply at all after the login), `no media after login`, `no video` or `no SPS/IDR`. The offer and the answer are logged through the same SDP summary as the webrtc ones, which leaves out the ICE credentials and the aes-key.

---

*This document describes research conducted on personally-owned devices for interoperability purposes. All credentials shown are anonymized or redacted. The Tuya trademark belongs to Tuya Inc.*

---

No additional API calls or phone access needed after login.
