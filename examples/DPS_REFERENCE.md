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
| 202 | `play` | Play specific track | string | rw | track identifier |
| 203 | `play_mode` | Loop mode | enum | rw | `loop`, `loop1`, `shuffle` |
| 209 | `play_volume` | Volume | value | rw | 1–100 (step 1) |
| 243 | `lullaby_timer_switch` | Timer enabled | bool | rw | true/false |
| 244 | `lullaby_timer` | Auto-stop timer (seconds) | value | rw | 0–5400 |
| 245 | `lullaby_display` | Timer remaining (seconds) | value | ro | -1–86400 |
| 246 | `play_state` | Current state | enum | rw | `playing`, `stopping` |
| 248 | `play_current` | Currently playing | string | rw | JSON: `{"bizcode":"phi-no-bm","id":3542155,"errcode":0}` |
| 249 | `voice_upgrade` | Custom recording update | string | rw | — |

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

### Two-Way Audio

| ID | Code | Name | Type | Mode | Values/Range |
|----|------|------|------|------|-------------|
| 252 | `pu_talking` | Parent unit talkback | enum | rw | `0` (off), `1` (on) |
| 253 | `app_talking` | App talkback | enum | rw | `0` (off), `1` (on) |
| 251 | `background_mode` | Background audio mode | bool | rw | true/false |

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
