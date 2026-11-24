# Camera DPS (Data Points) Reference

DPS are Tuya's mechanism for device control. Each data point has an ID, a code name,
a type, and read/write permissions. Values are read via `tuya.m.device.get` and
written via `tuya.m.device.dp.publish`.

## Reading DPS

```python
device = client.get_device("YOUR_DEVICE_ID")
dps = device["dps"]
temperature = dps["207"]  # raw value, divide by 100 for °C
```

## Writing DPS

```python
client.set_dps("YOUR_DEVICE_ID", {"138": True})   # night light on
client.set_dps("YOUR_DEVICE_ID", {"158": 50})      # brightness 50%
client.set_dps("YOUR_DEVICE_ID", {"201": "play"})   # play lullaby
```

## Complete DPS Map

### Video & Image

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 101 | `basic_indicator` | LED status | bool | rw | true/false |
| 102 | `ipc_flip` | Image rotation | enum | rw | `flip_none`, `flip_horizontal_mirror`, `flip_vertical_mirror`, `flip_rotate_180` |
| 237 | `privacy_switch` | Privacy mode (camera off) | enum | rw | `0` (off), `1` (on) |

### Night Light

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 138 | `bulb_switch` | Night light on/off | bool | rw | true/false |
| 158 | `floodlight_lightness` | Brightness | value | rw | 1–100 (step 1) |
| 204 | `nightlight_color` | Color | string | rw | color string |
| 240 | `nightlight_timer` | Auto-off timer (seconds) | value | rw | 1–5400 |
| 241 | `light_timer_switch` | Timer enabled | bool | rw | true/false |
| 242 | `light_timer_display` | Timer remaining (seconds) | value | ro | -1–86400 |

### Lullabies

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 201 | `play_control` | Playback control | enum | rw | `play`, `pause`, `stop`, `next`, `prev` |
| 202 | `play` | Play specific track | string | rw | JSON: `{"bizcode":"phi-no-bm","id":<id>}`, id from the catalogue below |
| 203 | `play_mode` | Loop mode | enum | rw | `loop`, `loop1`, `shuffle` |
| 209 | `play_volume` | Volume | value | rw | 1–100 (step 1) |
| 243 | `lullaby_timer_switch` | Timer enabled | bool | rw | true/false |
| 244 | `lullaby_timer` | Auto-stop timer (seconds) | value | rw | 0–5400 |
| 245 | `lullaby_display` | Timer remaining (seconds) | value | ro | -1–86400 |
| 246 | `play_state` | Current state | enum | rw | `playing`, `stopping` |
| 248 | `play_current` | Currently playing | string | rw | JSON: `{"bizcode":"phi-no-bm","id":3542155,"errcode":0}`; the cloud poll escapes its quotes (`{\"bizcode\":…}`), as it does for 202 |
| 249 | `voice_upgrade` | Recordings changed | string | rw | `upgrade`: refetch custom recordings from the cloud |

#### Catalogue and custom recordings

Track ids differ per unit, so the panel reads them from the cloud rather than
from a table. The presets and custom recordings are entries under the content
code `phi-no-bm`:

```python
client.call_result("tuya.m.audio.by.code.get", "1.0", {"devId": cam_id, "code": "phi-no-bm"})
# {"limitNum": 17, "audioInfoVOList": [
#    {"id": 19204255, "name": "Baa Baa Black Sheep", "isPreset": true, "fileFormat": "wav", ...}, ...]}
```

`limitNum` counts the 15 presets too, which leaves two custom recordings. Known
preset blocks: 3542154-3542168 (SCD973), 12072916-12072930 (firmware 1.1.12),
19204255 down to 19204241 (product `selj2idknqhjnids`, numbered backwards).

The app's "record your own lullaby" is a cloud upload, not a stream to the
camera. From the panel (RN bundle `000000kt5n` 1.3.0) and the app's
`com.thingclips.smart.camera.audiomanager` package:

1. Record a 16 kHz, 16-bit, mono WAV with a 44-byte header, at most 90 seconds.
2. `thing.m.audio.upload.url.get` `{devId, code}` returns a signed storage
   `url`, extra `headers`, a `bucket`, a storage path in `key`, and `localKey`,
   a 16-character AES key for this file.
3. Encrypt the WAV (container below) and `PUT` it to `url` with the headers and
   `Content-Type: audio/x-wav`.
4. `thing.m.audio.by.code.info.upload` `{devId, code, name, duration, size, key,
   bucket, path}` registers it: `duration` in ms and `size` in bytes of the
   plain WAV, `key` the `localKey`, `path` the storage `key`. It returns the new
   id.
5. Write `"upgrade"` to DPS 249. The camera fetches its recordings again.

`tuya.m.audio.by.code.id.delete` `{devId, code, id}` and
`tuya.m.audio.by.code.id.update` `{devId, code, id, name}` delete and rename;
the panel writes `"upgrade"` after a delete too.

The encrypted container, all integers little-endian: a 56-byte header of
version `1`, five zero words, the word `110`, three zero words and a 16-byte
ASCII IV. Then each 1280-byte chunk of the WAV is encrypted on its own with
AES-CBC/PKCS#5 under `localKey` and the same IV (1296 bytes out), and written
after a 24-byte frame header: type `5`, ciphertext length, 16 zero bytes.
`custom_components/philips_avent/audio.py` implements it.

### Temperature Sensor

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 207 | `sensor_temperature` | Temperature (°C × 100) | value | ro | 0–5000 (scale 2). Value 2250 = 22.50°C |
| 208 | `temp_report` | Temperature (°F × 100) | value | ro | 0–500 (scale 2) |
| 231 | `temp_max_switch` | High temp alert on | bool | rw | true/false |
| 232 | `temp_min_switch` | Low temp alert on | bool | rw | true/false |
| 233 | `temp_max_cvalue` | High temp threshold (°C × 100) | value | rw | 0–4000 (step 100) |
| 234 | `temp_min_cvalue` | Low temp threshold (°C × 100) | value | rw | 0–4000 (step 100) |
| 235 | `temp_max_fvalue` | High temp threshold (°F) | string | rw | — |
| 236 | `temp_min_fvalue` | Low temp threshold (°F) | string | rw | — |

### Motion & Sound Detection

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 106 | `motion_sensitivity` | Motion sensitivity | enum | rw | `0` (off), `1` (low), `2` (high) |
| 134 | `motion_switch` | Motion alert on/off | bool | rw | true/false |
| 168 | `motion_area_switch` | Area detection on | bool | rw | true/false |
| 169 | `motion_area` | Detection area | string | rw | JSON: `{"num":1,"region0":{"x":0,"y":0,"xlen":100,"ylen":100}}` |
| 250 | `motion_detection` | Motion event (read-only) | string | ro | event data |
| 139 | `decibel_switch` | Sound detection on/off | bool | rw | true/false |
| 140 | `decibel_sensitivity` | Sound sensitivity | enum | rw | `0` (off), `1` (low), `2` (high) |
| 141 | `decibel_upload` | Sound event (read-only) | string | ro | `decibel_upload` when triggered |
| 239 | `monitor_sensitivity` | Background monitoring | enum | rw | `0`, `1`, `2`, `3` |

The monitor runs one detector at a time. Measured at protocol 3.5 on an SCD973/26 in Leonard
Pitzu's fork: whichever of 134 and 139 was written last wins, and the other switch keeps
reading `true` while it does nothing. The integration therefore turns the partner off, but only
on a monitor whose LAN session has confirmed protocol 3.4 or newer.

### Two-Way Audio

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 252 | `pu_talking` | Parent unit talkback | enum | rw | `0` (off), `1` (on) |
| 253 | `app_talking` | App talkback | enum | rw | `0` (off), `1` (on). Also reads `1` for the whole of a cloud stream session (`webrtc` mode), with or without talkback |
| 251 | `background_mode` | Background audio mode | bool | rw | true/false |

In the bridge sessions logged on [#72](https://github.com/thekoma/aventproxy/issues/72), DPS 253
went to `1` within about 200 ms of the connection. It did so whether the offer's audio was
`sendrecv` or `recvonly`, and with no RTSP backchannel. It went back to `0` when the session
ended. One logged session showed no change, and nobody has explained it yet. This is probably why
a stream pauses a playing lullaby. WHITEPAPER section 21 covers it, as a hypothesis still under
test.

### SenseIQ (Sleep, Breathing & Presence)

Present on the SenseIQ product (productId `7d9t0rygsm7ztnww`). These codes come
from the product schema (`schemaInfo.schema` in `m.life.my.group.device.list`)
and were confirmed live on an SCD9xx. SenseIQ data is **not** in the Tuya message
centre — it rides these ordinary data points that `tuya.m.device.get` returns.

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 1 | `sleepiq_switch` | SenseIQ on/off | bool | rw | true/false |
| 2 | `cry_trans_switch` | Cry translation on/off | bool | rw | true/false (paid) |
| 3 | `sleepiq_status` | Live presence + breathing | string | ro | JSON `{"r":"o","br":0}` — `br` = breaths/min, `r` = presence flag |
| 4 | `sleep_session_data` | Sleep session | raw | ro | base64 → ASCII-hex → JSON (see below) |
| 5 | `sleepiq_consent` | SenseIQ consent | bool | rw | true/false |
| 6 | `senseiq_diagnostics` | Diagnostics | raw | ro | — |
| 7 | `sensiq_diag_consent` | Diagnostics consent | bool | rw | true/false |
| 8 | `awake_delay` | Baby-awake alert delay | value | rw | seconds (e.g. 180) |
| 9 | `cry_trans_result` | Cry translation result | string | ro | `"0"` = none (paid) |
| 10 | `sleepiq_area` | SenseIQ detection area | string | rw | region JSON |
| 11 | `awake_switch` | Baby-awake alert on/off | bool | rw | true/false |
| 12 | `cry_det_switch` | Cry alert on/off | bool | rw | true/false |
| 13 | `no_senseiq_switch` | "No SenseIQ signal" alert on/off | bool | rw | true/false |
| 15 | `no_senseiq_signal` | No SenseIQ signal | bool | ro | NOTE: does **not** track live presence — stayed set overnight while breathing/sleep were reported. Read presence from DPS 3 `r` instead. |
| 16 | `refurbish_counter` | Refurbishment counter | value | ro | — |
| 18 | `device_errors` | Errors | value | ro | 0 = no error |
| 21 | `ext_functions` | Extended functions | value | rw | — |

**DPS 3 `sleepiq_status`** — live JSON string:

```json
{"r":"o","br":0}
```
`br` is breaths per minute (0 while no baby is detected). `r` is a presence/state
flag: `"o"` = no baby (empty crib); `"m"`, `"b"` and `"a"` were all observed
while the baby was detected (over one night on an SCD9xx). Presence is therefore
`r != "o"` (or `br > 0`). Leonard Pitzu's fork, working on an SCD973, reads `b` as lying still
with a breathing rate measured and `m` as moving. The Baby Moving sensor is on while `r` is `m`.

**DPS 4 `sleep_session_data`** — base64 of an ASCII-hex string of JSON:

```json
{"st":1789409794,"sd":10038,"css":"l","cssd":60,
 "ssd":[{"a":216},{"l":86},{"a":946},{"l":350},{"d":1716},{"l":92},
        {"d":3274},{"l":406},{"d":34},{"l":271},{"d":1077},{"l":1377},{"d":133}]}
```
- `st` = session start (epoch seconds), `sd` = total sleep seconds
- `css` = current stage `a`/`l`/`d` (awake/light/deep), `cssd` = seconds in it. Two more codes
  appear, both from the fork on an SCD973. `o` is out of bed. `i` covers the first three to four
  minutes of a session, which the monitor later files as `o`; the integration calls it settling
- `ssd` = stage timeline, each item one `{stage: seconds}` pair
- invariant: `sum(ssd) + cssd == sd`

```python
import base64, json
session = json.loads(bytes.fromhex(base64.b64decode(dps["4"]).decode()).decode())
```

Alarm commands on this family (DPS 212 `initiative_message`): motion is
`ipc_human`, crying is `ipc_baby_cry`.

### System

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 205 | `power_status` | Power state | enum | ro | `0` (battery), `1` (plugged) |
| 206 | `OTA_message` | Firmware update | enum | rw | `0`, `1`, `2` |
| 247 | `device_poweroff` | Power off device | enum | rw | `0`, `1` |
| 254 | `bu_reset` | Base unit reset | string | ro | — |
| 255 | `timer_report` | Report timer | enum | rw | `0`, `1` |

## Video Quality

Video quality is controlled via the WebRTC session, not DPS. The `rtc.config.get`
response includes `vedioClaritys: [2, 4, 8]`:

| Value | Quality |
|-------|---------|
| 2 | HD (1920×1080) — main stream |
| 4 | SD (640×360) — sub stream |
| 8 | Audio only |

Set the desired quality when initiating the WebRTC connection by selecting the
appropriate stream type in the SDP offer.

## Signal Strength

Not available via DPS. Can be read from the device info's network status
or via `tuya.m.device.upgrade.rssi.info.query`.

## Examples

### Turn on night light at 30% brightness
```python
client.set_dps(cam_id, {"138": True, "158": 30})
```

### Play lullaby, volume 40%, auto-stop after 30 minutes
```python
client.set_dps(cam_id, {
    "201": "play",
    "209": 40,
    "243": True,
    "244": 1800,
})
```

### Stop lullaby
```python
client.set_dps(cam_id, {"201": "stop"})
```

### Read temperature
```python
device = client.get_device(cam_id)
temp_raw = device["dps"]["207"]  # e.g. 2250
temp_c = temp_raw / 100          # 22.50 °C
```

### Enable motion + sound alerts
```python
client.set_dps(cam_id, {
    "134": True,   # motion alert on
    "106": "2",    # high sensitivity
    "139": True,   # sound alert on
    "140": "2",    # high sensitivity
})
```

### Enable talkback (two-way audio)
```python
client.set_dps(cam_id, {"253": "1"})  # app talking on
# Audio is sent via WebRTC data channel (backchannel)
```

### Privacy mode (camera off, audio only)
```python
client.set_dps(cam_id, {"237": "1"})  # privacy on
client.set_dps(cam_id, {"237": "0"})  # privacy off
```
