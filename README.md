# Philips Avent Baby Monitor — Home Assistant Integration

<p align="center">
  <img src="custom_components/philips_avent/brand/logo.png" alt="Philips AVENT" width="300">
</p>

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![CI](https://github.com/thekoma/aventproxy/actions/workflows/ci.yml/badge.svg)](https://github.com/thekoma/aventproxy/actions/workflows/ci.yml)
[![HACS](https://img.shields.io/badge/HACS-Custom-41BDF5.svg)](https://github.com/hacs/integration)

Home Assistant integration for Philips Avent SCD973/SCD923 baby monitors, providing local streaming, temperature monitoring, night light control, lullaby playback, and motion/sound alerts.

## Features

| Feature | Entity Type | Description |
|---------|-------------|-------------|
| 📹 Live Video | Camera | 1080p H.264 stream via WebRTC→RTSP bridge, over your local network when possible |
| 🌡️ Temperature | Sensor | Room temperature from built-in sensor |
| 💡 Night Light | Switch + Number | On/off + brightness 1-100% |
| 🎵 Lullabies | Buttons + Number | Play/pause/stop/next/prev + volume |
| 🏃 Motion Alert | Switch | Motion detection on/off |
| 🔊 Sound Alert | Switch | Sound detection on/off |
| 🏃 Motion Detected | Binary Sensor | Fires when motion is detected (auto-clears after 30s) |
| 🔊 Sound Detected | Binary Sensor | Fires when sound is detected (auto-clears after 30s) |
| 🔒 Privacy Mode | Switch | Camera on/off |

The video travels over your local network whenever the bridge can reach the monitor directly. On monitors that allow it, the signalling that starts a stream stays local too, so no cloud call is needed. See [Local streaming](#local-streaming) below.

Monitors with SenseIQ also get sleep and presence entities. Among them are Baby Detected, Baby Moving, Breathing rate, Sleep stage (awake, light, deep, out of bed, settling), the sleep duration and a nightly summary.

Multiple monitors on one Tuya account are supported: the bridge serves each camera from the same port on a distinct RTSP path derived from the camera's display name.

## Supported devices

The integration is model-agnostic (it talks to the Tuya cloud API generically), so other Philips Avent monitors may work even if they are not listed here. Status below reflects what users have actually confirmed:

| Model | Status |
|-------|--------|
| SCD973 / SCD923 | ✅ Fully supported (primary development hardware) |
| SCD953/26 | ✅ Working, including motion and sound alerts (confirmed in 2026.8.0, [#42](https://github.com/thekoma/aventproxy/issues/42)) |
| SCD951 | ✅ Working, including motion and sound alerts (confirmed in 2026.8.0, [#61](https://github.com/thekoma/aventproxy/issues/61)) |
| SCD643/26 | ✅ Working |
| SCD971 | ✅ Working |
| SCD921 | ⚠️ Partial: video works but is intermittent; motion/sound detection and WiFi signal behave inconsistently ([#40](https://github.com/thekoma/aventproxy/issues/40)) |
| SCD860 | ❓ Requested, not yet confirmed ([#59](https://github.com/thekoma/aventproxy/issues/59)) |

Motion and sound alerts do not travel the same way on every monitor. Some report them on DPS 250 and 141, such as an SCD973 on firmware 1.4.0. Others use a timestamped alarm record on DPS 212 instead: the SCD951, the SCD953, and the SCD973/26 in Leonard Pitzu's fork. The choice follows the firmware, not the model, so the integration reads both on every monitor. Both shapes are handled since 2026.8.0. If your monitor raises an alert the app notifies you about and Home Assistant stays quiet, the log now names the alert Tuya sent, which usually turns the fix into one line.

LAN streaming has only been tested on an SCD973/26.

Got a model that isn't listed, or one that misbehaves? A [diagnostics dump](docs/reporting-issues.md) is the fastest way to get it mapped.

## Installation

### Integration (HACS)

[![Open HACS repository](https://my.home-assistant.io/badges/hacs_repository.svg)](https://my.home-assistant.io/redirect/hacs_repository/?owner=thekoma&repository=aventproxy&category=integration)

Or manually:

1. Open HACS → Integrations → Three dots menu → Custom repositories
2. Add `https://github.com/thekoma/aventproxy` as category **Integration**
3. Install "Philips Avent Baby Monitor"
4. Restart Home Assistant

### Add-on (WebRTC Bridge)

[![Add add-on repository](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2Fthekoma%2Faventproxy)

Or manually:

1. Settings → Add-ons → Add-on Store → Three dots → Repositories
2. Add `https://github.com/thekoma/aventproxy`
3. Install "Philips Avent WebRTC Bridge (Addon)"

**Three cards, one choice.** The store shows the bridge three times:

| Card | Install it when |
|------|-----------------|
| Philips Avent WebRTC Bridge (Addon) | You run a stable release of the integration |
| Philips Avent WebRTC Bridge Beta (Addon) | You run a beta or rc build of the integration |
| Philips Avent WebRTC Bridge Dev (Addon) | Someone asked you to test a specific fix off main. No release notes, no promises |

Keep both sides on the same version. The two halves talk over a config file whose contents grow with each release, so a stable bridge paired with an rc integration can miss a field it needs. As an example, bridges before 2026.7.0-rc2 do not read `api_host`, so an account outside Europe logs in and then fails to stream.

### Setup

[![Add integration](https://my.home-assistant.io/badges/config_flow_start.svg)](https://my.home-assistant.io/redirect/config_flow_start/?domain=philips_avent)

1. Go to Settings → Integrations → Add Integration → **Philips Avent Baby Monitor**
2. Enter your email and password (same as the Baby Monitor+ app), and check that the country matches the one your Avent account was created in (it is pre-filled from your Home Assistant country)
3. Check your email for the 6-digit verification code
4. Enter the code — done!

The integration discovers your cameras automatically and creates all entities. The bridge add-on starts streaming.

The country matters: a Philips/Tuya account lives in one regional server, and the session from one is not valid on another. Getting it wrong shows up as a login that fails right after you type the verification code.

### Running the bridge outside Home Assistant

Two-way audio is off by default. Asking the camera for it makes the monitor stop a playing lullaby and restart it from the beginning when you close the stream, so listening does not imply talking. If a client of yours really talks back through the RTSP stream, turn on **Two-way audio (talkback)** under Configure. On some monitors a stream pauses a playing lullaby even with two-way audio off ([#72](https://github.com/thekoma/aventproxy/issues/72)). On an SCD973 the cause turned out to be the webrtc session itself: the monitor treats it as the app talking. The `cloud_transport: imm` option of the Dev and Beta cards avoids it there (see [imm over cloud signalling](#imm-over-cloud-signalling-dev-and-beta-cards-experimental)). The Dev card's `speaker_off` option, described in its changelog, did not help.

If you don't run the add-on but host the bridge yourself, in its own container or on another machine, tell the integration where to find it: **Settings → Integrations → Philips Avent → Configure → Bridge host**. It defaults to `localhost`, which is only correct when the bridge shares the network with Home Assistant. A wrong host makes the camera entity flap between `Unavailable` and `Idle`, because Home Assistant marks a camera unavailable while its stream cannot be opened.

#### Who can reach the RTSP stream

The bridge's RTSP server has no password. The add-on therefore listens on loopback only (`localhost`, i.e. 127.0.0.1 and ::1) by default: Home Assistant runs on the host network next to it and opens `rtsp://localhost:38554/<camera>`, so nothing else on your network can watch the nursery. If another machine must open the stream (VLC, Frigate, a separate go2rtc, or Home Assistant itself when the bridge runs elsewhere), set the add-on option **RTSP bind address** (`rtsp_bind_address`) to `0.0.0.0`, to one address, or to a comma-separated list, and restrict port 38554 on your network. Outside the add-on, pass `--bind` to `avent-webrtc-bridge addon` or set `RTSP_BIND_ADDRESS` for `run.sh` (see `docker-compose.yml`).

### Automations

The integration exposes `binary_sensor.<camera>_sound_detected` and `binary_sensor.<camera>_motion_detected` that turn on for ~30 seconds when the monitor reports an event. They are the trigger points for any automation.

Note: detection must be enabled on the device side via the `🔊 Sound Alert` / `🏃 Motion Alert` switches — otherwise the monitor never sends the event. On a monitor whose LAN session has confirmed protocol 3.4 or newer, only one detector runs at a time. Turning one switch on there turns the other off, and turning one off leaves the other alone.

```yaml
# Mobile notification when sound is detected
automation:
  - alias: "Baby crying alert"
    trigger:
      - platform: state
        entity_id: binary_sensor.baby_monitor_sound_detected
        to: "on"
    action:
      - service: notify.mobile_app_my_phone
        data:
          title: "Sound detected in nursery"
          message: "Baby may be awake"

# Pop up the camera feed on a dashboard when motion is detected
  - alias: "Show baby cam on motion"
    trigger:
      - platform: state
        entity_id: binary_sensor.baby_monitor_motion_detected
        to: "on"
    action:
      - service: browser_mod.popup
        data:
          content:
            type: picture-entity
            entity: camera.baby_monitor
            camera_view: live
```

## How It Works

The integration uses the same Tuya Mobile SDK API as the official Philips Avent Baby Monitor+ app. Authentication uses the same password + MFA flow — your traffic is indistinguishable from the real app.

```
┌─────────────────────────────┐
│       Home Assistant         │
│                              │
│  ┌────────────────────────┐  │
│  │  Custom Integration    │  │   Tuya Cloud
│  │  (login, sensors,      │◄─┼──► a1.tuya**.com
│  │   lights, lullabies)   │  │   (API calls, your
│  │                        │  │    account's region)
│  └────────────────────────┘  │
│                              │
│  ┌────────────────────────┐  │
│  │  Bridge Add-on         │  │   Camera
│  │  (WebRTC → RTSP)       │◄─┼──► via STUN/TURN
│  │  :38554                │  │   (1080p H.264)
│  └────────────────────────┘  │
│                              │
│  ┌────────────────────────┐  │
│  │  Camera Entities       │  │
│  │  rtsp://host:38554/N1  │  │
│  │  rtsp://host:38554/N2  │  │
│  │  ...                   │  │
│  └────────────────────────┘  │
└─────────────────────────────┘
```

### Local streaming

The video already travels over your local network whenever the bridge and the monitor can reach each other directly. ICE picks the monitor's LAN address, and a capture on an SCD973 showed every media packet coming straight from the monitor. What can go through the Tuya cloud is the *signalling*, the short exchange that starts a stream. The bridge log names each stream by it: `Streaming <camera> with cloud signalling` or `Streaming <camera> with LAN signalling`. LAN signalling, on the monitors that allow it (below), needs no cloud call, so a stream still starts when Tuya or your internet connection is down.

The bridge tries LAN signalling first, under these conditions:
- the stream is HD and talkback is off;
- LAN streaming is on;
- the camera has a local key and a P2P password;
- the integration knows the camera's LAN address.

The monitor must speak Tuya LAN protocol 3.5. Monitors on 3.3 (seen on an SCD973 with firmware 1.4.0) keep cloud signalling, and the bridge log says `Not using LAN signalling for <camera>: protocol 3.3`. The stream must deliver H.264, otherwise the first-frame check below sends it to the cloud. Talkback and `/sd` streams always use cloud signalling. The monitor must also have reached the internet once since it booted, or it sends no media to a stream with LAN signalling.

If no H.264 video arrives within 4 seconds of a LAN attempt, the same start falls back to cloud signalling and that camera skips the LAN for 5 minutes. An HEVC monitor ends up here. A LAN stream that dies restarts on the cloud with the same 5-minute wait. Each further LAN failure in a row doubles the wait, up to an hour, so a monitor whose LAN never works is not probed every 5 minutes for good; a LAN stream that works ends the streak. If cloud signalling fails as well, the wait is cleared, so the next start tries the LAN again. Once the wait is over, a stream on the cloud checks the LAN in the background and moves its viewers back, with one short reconnect. RTSP clients see the same H.264 video and PCMU/8000 audio either way.

The **LAN streaming** option in the integration options is on by default. Turn it off if a stream stopped working after an update, and open an issue with your monitor model and the bridge log. While it is off, the bridge config holds no local key or P2P password.

#### imm over cloud signalling (Dev and Beta cards, experimental)

The Dev and Beta cards have a `cloud_transport` option for streams that do not take the LAN, such as every stream from a 3.3 monitor. `webrtc`, the default, is the cloud session the bridge has always used. With `imm`, the bridge first tries the Philips app's own transport (imm, AES/KCP) with cloud signalling: the offer and answer go through Tuya's broker and the media takes the LAN path's code. The offer goes to Tuya's media relay first. If the monitor does not answer within 3 seconds, the bridge also sends it the way the app does, on the monitor's own MQTT channel, over a separate connection. That connection is shared by your cameras, opened the first time a camera needs it, and stays open as long as a session uses it. It may help with a lullaby that pauses when you open the stream ([#72](https://github.com/thekoma/aventproxy/issues/72)), and with monitors that send no video over webrtc ([#59](https://github.com/thekoma/aventproxy/issues/59)). On an SCD973 on firmware 1.4.0 (protocol 3.3), the monitor answered through the media relay and the stream came up in under a second; a playing lullaby kept playing, where the webrtc session stopped it. No other model has tried it yet.

- The first version reaches the monitor on your local network only. If no H.264 video arrives, the same start falls back to the webrtc session, and that camera skips imm for 5 minutes. Nothing moves a running stream back to imm afterwards: with a viewer that never disconnects (Frigate, an always-on go2rtc), the camera stays on webrtc until that viewer reconnects. A start can take up to 16 seconds before it falls back, or 20 when a LAN attempt runs first. A stream that starts while privacy mode is on sends no video, so it uses the webrtc session, which still carries the sound, and that camera skips imm for the next 5 minutes.
- Talkback and `/sd` streams always use webrtc.
- The bridge fetches the local key and the P2P password it needs from the cloud, so it does not depend on the integration's **LAN streaming** option.
- The log says `Streaming <camera> with imm over cloud signalling`, and `Answer from <camera> via moto` or `via device channel` tells which route worked. Otherwise one warning names the step that failed, such as `no answer`, `ICE timeout` or `no SPS/IDR`, before `using webrtc cloud signalling`.

### Local control and alerts

The integration keeps its own LAN session with the monitor, separate from the bridge's streams, for pushed state and commands. What it does with that session depends on the protocol the session negotiates.

- **Protocol 3.3.** The cloud stays in the loop, as before. Pushes such as the temperature arrive locally. Commands go over the LAN and are repeated through the cloud. The cloud is still polled every 30 or 120 seconds, and motion and sound alerts arrive only through that poll.
- **Protocol 3.4 and newer.** The session reads the monitor's full state when it comes up, and it receives alarms as pushes in DPS 212. Once the monitor has ever reported an alarm in DPS 212, three things change while the session is up:
  - the cloud poll pauses;
  - commands the LAN delivered are not repeated through the cloud;
  - the cloud is only asked for the WiFi signal every 5 minutes and, on SenseIQ monitors, for the nightly summary every 15 minutes.

  The poll comes back within 30 seconds of the session dropping. A monitor that has never reported DPS 212 keeps the poll and the cloud copy of commands. The integration log says `Cloud poll for <camera> paused: its LAN session at protocol 3.5 pushes state and alarms`, and later `Cloud poll for <camera> resumed`.

The 3.4+ behaviour follows Leonard Pitzu's measurements on his SCD973/26 at protocol 3.5. Nobody has tested it here on a 3.4 or 3.5 monitor yet.

During a cloud outage, a LAN session that has proven its key keeps the entities available. On 3.3, a session proves its key with its first decrypted push. The integration logs one warning when the cloud fails, `Cloud for <camera> failed (…); its LAN session keeps it available until the cloud answers again`, and `Cloud for <camera> answers again` when it does. On 3.3, alerts come only from the cloud poll, so Motion Detected, Sound Detected and Crying Detected read unavailable until the cloud answers. They cannot fire in the meantime, and showing them as unavailable makes that visible. The same goes for a 3.4+ monitor that has never reported DPS 212. Without a LAN session, every entity goes unavailable during an outage, as before.

If the monitor is paired again and gets a new local key, the next cloud poll picks it up. The LAN session reconnects with the new key and the bridge config is rewritten, with no Home Assistant restart.

## Camera Data Points

| DPS | Code | Description | Values |
|-----|------|-------------|--------|
| 138 | `bulb_switch` | Night light | on/off |
| 158 | `floodlight_lightness` | Brightness | 1–100 |
| 201 | `play_control` | Lullaby | play/pause/stop/next/prev |
| 207 | `sensor_temperature` | Temperature | °C × 100 |
| 209 | `play_volume` | Volume | 1–100 |
| 134 | `motion_switch` | Motion alert | on/off |
| 139 | `decibel_switch` | Sound alert | on/off |
| 237 | `privacy_switch` | Privacy mode | 0/1 |

Full reference: [examples/DPS_REFERENCE.md](examples/DPS_REFERENCE.md)

## Technical Background

This integration was built through reverse engineering of the Tuya Mobile SDK API. The complete methodology is documented in [WHITEPAPER.md](WHITEPAPER.md), including:

- API signing algorithm (HMAC-SHA256 with composite key)
- Password + MFA login flow
- MQTT credential derivation for WebRTC signaling
- 10 documented failures and dead ends

## For Developers

### Adapting for other Tuya whitelabel cameras

The signing algorithm is generic to all Tuya Thing SDK apps. See [tools/apk-key-extractor/](tools/apk-key-extractor/) for automated key extraction from any APK.

### Running tests

```bash
python3 -m venv .venv && source .venv/bin/activate
pip install pytest pycryptodome aiohttp voluptuous homeassistant
PYTHONPATH=. pytest tests/test_philips_avent/ -v
```

### Running the bridge manually

```bash
cd avent-webrtc-bridge && go build -o avent-webrtc-bridge .
./avent-webrtc-bridge direct --help
```

## Reporting issues

Found a bug or an entity that behaves oddly? See [`docs/reporting-issues.md`](docs/reporting-issues.md). It's a parent-friendly, three-click walkthrough for downloading the diagnostics dump that lets us triage your device without asking for packet captures.

## Acknowledgments

The WebRTC bridge (`avent-webrtc-bridge/`) is forked from [tuya-ipc-terminal](https://github.com/seydx/tuya-ipc-terminal) by seydx, which itself uses WebRTC and codec utilities from [go2rtc](https://github.com/AlexxIT/go2rtc) by AlexxIT. The bridge has since diverged significantly with Philips Avent-specific features including RTP timestamp rebasing, SPS/PPS injection, RTSP backchannel audio, and MQTT-based WebRTC signaling via the Tuya Mobile SDK.

Protocol research in `WHITEPAPER.md` sections 14 to 17, covering the WebRTC signaling frames, the native `libThingP2PSDK.so` analysis and the local-signaling path, was contributed by [@leonardpitzu](https://github.com/leonardpitzu) in [#51](https://github.com/thekoma/aventproxy/issues/51) from work on the sibling `aventlocal` project.

The local streaming path is a port of [Leonard Pitzu's work](https://github.com/leonardpitzu/aventproxy) ([#51](https://github.com/thekoma/aventproxy/issues/51)): he reverse-engineered the LAN protocol and wrote the first implementation. It is documented in `WHITEPAPER.md` section 20 and in his [PROTOCOL.md](https://github.com/leonardpitzu/aventproxy/blob/main/PROTOCOL.md).

## License

MIT — See [LICENSE](LICENSE)

## Disclaimer

This project is not affiliated with, endorsed by, or connected to Koninklijke Philips N.V., Tuya Inc., or any of their subsidiaries. "Philips" and "AVENT" are registered trademarks of Koninklijke Philips N.V. The Philips AVENT logo and branding are used solely for identification purposes to help users recognize which device this integration supports. All API access uses the owner's own credentials and the same protocol as the official app.
