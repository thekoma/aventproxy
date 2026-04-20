# Tuya APK Key Extractor

Extract the signing key components from any Tuya whitelabel APK.
These keys are needed to make authenticated API calls to the Tuya Mobile SDK.

## What It Extracts

| Component | Source | Method |
|-----------|--------|--------|
| `packageName` | AndroidManifest.xml | jadx decompilation |
| `certSHA256` | APK signing certificate | `keytool` |
| `appKey` | `BuildConfig.THING_SMART_APPKEY` | jadx + grep |
| `appSecret` | `BuildConfig.THING_SMART_SECRET` | jadx + grep |
| `embeddedKey` | `assets/cers` | XOR decode (may need Frida fallback) |

The signing key is: `{packageName}_{certSHA256}_{embeddedKey}_{appSecret}`

## Usage

### Docker (recommended)

```bash
# Build
docker build -t apk-key-extractor .

# Run (mount APK and output directory)
docker run --rm \
  -v /path/to/your/app.apk:/input/app.apk \
  -v $(pwd)/output:/output \
  apk-key-extractor

# Check results
cat output/tuya_keys.json
```

### Direct

```bash
# Requirements: python3, jadx, keytool (JDK), openssl
python3 extract.py /path/to/your/app.apk
```

## Output

`tuya_keys.json`:
```json
{
  "status": "complete",
  "package_name": "com.example.smartcamera",
  "cert_sha256": "AA:BB:CC:...",
  "app_key": "xxxxxxxxxxxxxxxxxxxx",
  "app_secret": "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "ttid": "vendorname",
  "embedded_key": "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "signing_key": "com.example.smartcamera_AA:BB:CC:..._embkey_secret",
  "signing_algorithm": "HMAC-SHA256(...)",
  "api_endpoint": "https://a1.tuyaeu.com/api.json"
}
```

## If `embeddedKey` Extraction Fails

The `assets/cers` file uses a proprietary encoding. If static extraction fails,
use the Frida one-shot method (requires rooted phone with the app running):

```bash
# One-time extraction from running app
python3 ../extract_signing_key.py com.example.smartcamera
```

This dumps the complete signing key from process memory. Run once per APK version.

## How the Signing Key Is Used

```python
import hmac, hashlib

signing_key = "..."  # from extraction
params_string = "a=smartlife.p.time.get||v=1.0||..."  # sorted, filtered, || separated

signature = hmac.new(
    signing_key.encode(),
    params_string.encode(),
    hashlib.sha256
).hexdigest()
```

See `TUYA_API_RE.md` in the project root for the complete algorithm documentation.

## Where To Get the APK

- From the device: `adb shell pm path com.vendor.app && adb pull /path/to/base.apk`
- From APK mirror sites (ensure same version as installed)
- Split APKs: only `base.apk` is needed (not the arch-specific splits)

## Notes

- All extracted keys are **static per APK version** — extract once, use forever
- Keys persist across app updates (same signing cert, same build config)
- Keys change only if the vendor re-signs the APK or updates Tuya credentials (rare)
- The `embeddedKey` is the same for ALL users of the same app — it's not per-account
