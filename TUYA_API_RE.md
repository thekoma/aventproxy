# Tuya Mobile SDK API - Reverse Engineering Results

## Signing Algorithm

The Tuya mobile SDK (`a1.tuyaeu.com/api.json`) uses **HMAC-SHA256** for request signing.

### Key Construction

```
signing_key = "{packageName}_{certSHA256}_{embeddedKey}_{appSecret}"
```

Components:
- **packageName**: Android package name (e.g. `com.philips.ph.babymonitorplus`)
- **certSHA256**: SHA-256 of APK signing certificate, uppercase hex with colons (e.g. `D2:D6:95:A1:...`)
- **embeddedKey**: extracted from `assets/cers` in the APK, decoded via `SignFileDecoder` in `libthing_security.so`
- **appSecret**: Tuya app secret passed during SDK init

### Parameter String Construction

1. Filter params to whitelist: `a, v, lat, lon, lang, deviceId, appVersion, ttid, isH5, h5Token, os, clientId, postData, time, requestId, et, n4h5, sid, chKey, sp`
2. Remove empty values
3. If `postData` exists, replace value with `swapSignString(MD5(postData))`
4. Sort keys alphabetically
5. Join as `key=value||key=value||...`

### swapSignString

Rearranges a 32-char hex string in 8-char blocks:

```
input:  [A][B][C][D]   (each block = 8 chars)
output: [B][A][D][C]
```

### Signature

```
sign = HMAC-SHA256(signing_key, params_string)
```

Output is lowercase hex, 64 chars.

## API Endpoint

```
POST https://a1.tuyaeu.com/api.json
Content-Type: application/x-www-form-urlencoded
User-Agent: Thing-UA=APP/Android/1.8.0/SDK/6.7.0
```

## Required Parameters

| Parameter | Value | Notes |
|-----------|-------|-------|
| a | API action name | e.g. `smartlife.p.time.get` |
| v | API version | usually `1.0` |
| clientId | Tuya appKey | |
| sid | session ID | from login |
| sign | HMAC-SHA256 | computed as above |
| time | unix timestamp | seconds |
| et | encryption type | `0.0.1` for plaintext |
| nd | `1` | |
| os | `Android` | |
| lang | `en_US` | |
| ttid | `sdk_international@{appKey}` | |
| deviceId | phone device ID | |
| appVersion | app version | |
| sdkVersion | SDK version | |
| requestId | UUID | |
| postData | JSON string | optional, for POST body |

## Credentials (Example — extract your own)

```
appKey:       <extract from BuildConfig.THING_SMART_APPKEY>
appSecret:    <extract from BuildConfig.THING_SMART_SECRET>
embeddedKey:  <extract from assets/cers via Frida>
certSHA256:   <extract via: keytool -printcert -jarfile app.apk>
packageName:  <from AndroidManifest.xml>
sid:          <from OTP login — see login flow below>
phoneDeviceId: <any unique string>
chKey:        <extract from app traffic or Frida>
```

## Tested API Actions

| Action | Description |
|--------|-------------|
| `smartlife.p.time.get` | Server time |
| `smartlife.m.user.info.get` | User profile + MQTT domain URLs |
| `tuya.m.device.get` | Device info (devId, localKey, dps, online status) |
| `m.life.home.space.list` | Home/space list |
| `smartlife.m.rtc.config.get` | WebRTC config (STUN/TURN, ICE, AES key, session) |
| `smartlife.m.p2p.main.pre.link.get` | P2P pre-link |
| `smartlife.m.token.get` | MQTT token |

## RE Methodology

1. MITM proxy (mitmproxy Docker + Frida SSL unpinning) to capture API traffic
2. Identified all parameters and encrypted responses (`et: '3'`)
3. Frida hooks on `ThingApiSignManager.generateSignatureSdk` to understand Java-side flow
4. Decompiled APK with jadx to read `ThingApiSignManager`, `ThingNetworkSecurity`, `JNICLibrary`
5. Identified native signing in `libthing_security.so` via `doCommandNative(cmd=1)`
6. r2ghidra decompilation of `doCommandNative` and `computeDigest` revealed key stored at global `0x39070`
7. Frida memory dump of the runtime global extracted the full signing key
8. Verified with independent Python HMAC-SHA256 implementation — exact match
