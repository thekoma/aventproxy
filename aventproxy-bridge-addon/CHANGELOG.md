# Changelog

## 2026.11.1

Update the integration and this card together.

The integration now counts a LAN heartbeat that the monitor leaves unanswered as a failure, and reconnects, so a monitor that stops answering no longer keeps looking connected with its last values (Luuk Visser, [#144](https://github.com/thekoma/aventproxy/pull/144)). It was measured on protocol 3.4 and, through 2026.11.1-rc1, checked on an SCD973 on protocol 3.3: one LAN session, no unanswered heartbeat. If a monitor on 3.3 reconnects every 10 to 20 seconds after this update, with `LAN heartbeat not answered` in the integration's debug log, please report it on [#110](https://github.com/thekoma/aventproxy/issues/110).

**Add-on**
- The add-on no longer stops when the bridge exits with an error, for example when the network is not up yet after a reboot. The bridge is restarted on any exit, with a delay that grows from 5 seconds to 5 minutes while it keeps failing. The add-on also waits for the integration's config instead of exiting without a message, and picks up an integration that was removed and added again ([#159](https://github.com/thekoma/aventproxy/issues/159)).
- `/config` is mapped read-only, and the bridge keeps its saved session data in `/data`, so it survives a rebuilt container ([#160](https://github.com/thekoma/aventproxy/issues/160)).
- Stopping or restarting the add-on no longer takes minutes when the Tuya MQTT broker is unreachable: a stream start still waiting for the cloud is cancelled. An MQTT connect already under way can still hold the stop for up to 10 seconds ([#162](https://github.com/thekoma/aventproxy/issues/162)).
- Smaller bridge fixes from the same report: an RTSP client whose first request is `TEARDOWN` is disconnected at once, an `rtsp_bind_address` of only spaces means localhost instead of every interface, a refused login is reported as an error, and a failed HEVC data channel no longer crashes the bridge ([#162](https://github.com/thekoma/aventproxy/issues/162)).
- A quieter log: a client that closes its RTSP connection after a reply is logged at debug level, not as an error ([#129](https://github.com/thekoma/aventproxy/issues/129)), and so are MQTT frames for a session that already ended (Luuk Visser, [#150](https://github.com/thekoma/aventproxy/pull/150)).

**Integration**
- A camera that another account shared to yours is found. Discovery only looked in the account's own homes ([#127](https://github.com/thekoma/aventproxy/issues/127)). Not yet tried on an account with a shared camera.
- If the cloud refuses one monitor, the others still start (Luuk Visser, [#152](https://github.com/thekoma/aventproxy/pull/152)). A setup whose camera discovery fails on a network error is retried instead of ending with no cameras (Luuk Visser, [#148](https://github.com/thekoma/aventproxy/pull/148)).
- Snapshots work for a monitor whose name contains an apostrophe, such as "Baby's room" (Luuk Visser, [#141](https://github.com/thekoma/aventproxy/pull/141)).
- On the LAN (Luuk Visser):
  - A monitor that is not on the LAN is warned about once, and the retries back off to once a minute ([#143](https://github.com/thekoma/aventproxy/pull/143)).
  - Once a monitor has had a keyed LAN session (3.4 or 3.5), the integration no longer falls back to 3.3 for it. A monitor that later moves back to 3.3 needs the integration added again ([#146](https://github.com/thekoma/aventproxy/pull/146)).
  - Alarms that the monitor pushes over the LAN fire even when the monitor's clock is off ([#147](https://github.com/thekoma/aventproxy/pull/147)).
- Several commands in quick succession no longer postpone the cloud poll, and with it the alerts that only the poll brings (Luuk Visser, [#138](https://github.com/thekoma/aventproxy/pull/138)).
- Diagnostics and debug logs hide the monitor's MAC address (Luuk Visser, [#137](https://github.com/thekoma/aventproxy/pull/137)).
- The "no cameras" error says to reload the integration, which runs the discovery again, instead of pointing to a reconfigure step that does not exist ([#132](https://github.com/thekoma/aventproxy/pull/132)).

## 2026.11.0

Update the integration and this card together. The bridge streams over the LAN only with the protocol version that the integration writes into its config.

### Fixed

- The RTSP server could stop answering every client until the add-on was restarted, while the add-on still showed as started. A viewer that stopped reading its stream held the stream up, and the next client to leave then held up the whole server. A viewer that stops reading for 5 s is now dropped, and a client that leaves no longer holds up the others. This is likely the hang an NVR ran into after its TEARDOWN ([#129](https://github.com/thekoma/aventproxy/issues/129)).
- RTSP clients that keep their session alive with `GET_PARAMETER`, such as Ajax NVRs, get `200 OK` instead of `501 Not Implemented`, and a request that carries a body no longer drops the connection ([#129](https://github.com/thekoma/aventproxy/issues/129)).
- RTSP viewers no longer stall at DESCRIBE while their camera's stream starts ([#89](https://github.com/thekoma/aventproxy/issues/89), [#97](https://github.com/thekoma/aventproxy/issues/97)).
- With two or more cameras, one MQTT connection serves all of them. Each camera's start used to cut the other's off.
- A cloud session that carries no media for 20 s is replaced, with the warning `Cloud session for <camera> is up but carried no media in 20s`. A session that carries only audio, as in privacy mode, is kept.
- Video packets that Wi-Fi delivers out of order are put back in order, which removes a smeared, green lower half of the picture until the next keyframe (Wessel Blokzijl, [#112](https://github.com/thekoma/aventproxy/pull/112)).
- Viewers keep the picture when a cloud retry replaces the session, RTSP over UDP works from another machine, and audio is timed by its samples, which may stop crackle.
- A start that times out says what it waited for, on the `Failed to start WebRTC bridge` line.
- A temporary bridge config that a crash left in `/config` is removed.
- In the integration:
  - SCD953P on firmware 2.4.1: picking a lullaby plays it, and the track select shows the track that plays (czaja, [#116](https://github.com/thekoma/aventproxy/pull/116)).
  - SCD971/26: the SenseIQ nightly summary shows light, deep and awake time instead of zeros (Kenny, [#108](https://github.com/thekoma/aventproxy/pull/108)).
  - On protocol 3.3, LAN pushes no longer delay the cloud poll, and with it the motion and sound alerts.
  - A lullaby next or previous no longer skips tracks when the monitor answers slowly.
  - While the integration's LAN session is up, a cloud outage leaves the entities available, and a re-paired monitor is picked up without restarting Home Assistant.

### New

- **Local streaming ([#51](https://github.com/thekoma/aventproxy/issues/51)).** Ported from Leonard Pitzu's fork, with his measurements. On monitors that speak Tuya LAN protocol 3.5, the bridge negotiates the stream with the monitor over your network and uses cloud signalling only when that fails. Each failure in a row doubles how long it waits before trying the LAN again, up to an hour. Monitors on 3.3, such as an SCD973 on firmware 1.4.0, stay on cloud signalling, and the video of such a stream usually travels over your LAN already. The integration option **LAN streaming**, on by default, turns it off. The bridge log says `Streaming <camera> with LAN signalling` or `Not using LAN signalling for <camera>: protocol 3.3`.
- **Alerts on protocol 3.4 and newer.** The integration keeps a LAN session with the monitor, gets motion and sound alarms pushed within seconds, and sends commands locally. On a 3.5 monitor, which runs one detector at a time, turning Motion Alert on turns Sound Alert off ([#110](https://github.com/thekoma/aventproxy/issues/110)). Not tested here on a 3.4 or 3.5 monitor.
- **Talk from Home Assistant 2026.11 (experimental).** With **Two-way audio (talkback)** on in the integration, the camera tells Home Assistant 2026.11 that it takes audio, and the camera dialog shows a microphone once the two-way audio preview is on under Settings → System → Labs. It needs HTTPS and a WebRTC stream; an older Home Assistant ignores it.
  - New option `talkback_mode`, `on_demand` by default. The stream stays one-way, and while audio arrives on the RTSP backchannel the bridge opens a second, two-way session, which it closes 3 seconds after the audio stops. A playing lullaby pauses only while that session is open, and the first second or two of a talk are lost while it opens. `session` keeps the 2026.10.0 behaviour, where every stream with talkback on asks the monitor for two-way audio.
  - The talk audio reaches the monitor as 16 kHz PCM, the format it plays. G.711 came out as hissing on an SCD923/26 and an SCD951/26 (Luuk Visser, [#111](https://github.com/thekoma/aventproxy/pull/111)).
  - The log says `Talking to <camera>: the two-way session took …` and `Stopped talking to <camera> …`.
- **New option `cloud_transport` (experimental, [#72](https://github.com/thekoma/aventproxy/issues/72), [#59](https://github.com/thekoma/aventproxy/issues/59)).** `webrtc`, the default, changes nothing. With `imm`, a stream that does not take the LAN first tries the Philips app's own transport over cloud signalling, and falls back to webrtc when that fails. On an SCD973 on firmware 1.4.0 (protocol 3.3) and on an SCD953P, a playing lullaby kept playing, where the webrtc session stops it until the stream closes. It needs the monitor on your local network. The log says `Streaming <camera> with imm over cloud signalling`, or one warning names the step that failed and that camera skips imm for 5 minutes. Please report on #72 with your monitor model and firmware.
- **SenseIQ (Steeven85, [#87](https://github.com/thekoma/aventproxy/pull/87)).** Monitors with SenseIQ get sleep and presence entities: Baby Detected, Baby Moving, Breathing rate, Sleep stage (awake, light, deep, out of bed, settling), the sleep duration and a nightly summary. They also get switches for SenseIQ, the awake alert and the cry alert, and the delay before the awake alert. Each entity appears when its data first arrives. On a monitor that pushes its status over the LAN, Baby Detected turns off a minute after the pushes stop, and Breathing rate reads unknown ([#74](https://github.com/thekoma/aventproxy/issues/74)).
- New action `philips_avent.get_sleep_history` (Kenny, [#109](https://github.com/thekoma/aventproxy/pull/109)). It returns the SenseIQ daily summary for a range of days, each session with its stage timeline. The cloud keeps about 30 days.

### Changed

- The image runs on Debian 13 (trixie) instead of Debian 12 (bookworm), which security scanners flagged for packages the bridge never uses (Daniel Jansen, [#124](https://github.com/thekoma/aventproxy/pull/124)).

If something broke, turn off **LAN streaming** and set `cloud_transport` back to `webrtc`, then open an issue with your monitor model, firmware, the integration diagnostics and the bridge log.

## 2026.10.0

### ⚠️ BREAKING CHANGE: read this before updating

**The RTSP stream is now only reachable from this host.** The bridge's RTSP
server has no password, so it now listens on `localhost` (127.0.0.1 and ::1)
instead of every network interface. Until now, anyone on your network could
watch and hear the camera at `rtsp://<your-HA-address>:38554/<camera>`.

**Nothing to do** if Home Assistant opens the stream at the default
`rtsp://localhost:38554/...`, which is the standard setup.

**You must change a setting** if any of these applies to you:

- you open the stream from another machine: VLC, Frigate, Scrypted or
  HomeKit through Scrypted, a separate go2rtc;
- you set the integration's **Bridge host** option to this machine's IP
  address instead of `localhost`.

To restore access, set the add-on option **RTSP bind address** to `0.0.0.0`
(every interface) or to a specific address, save, and restart the add-on.
Then restrict port 38554 on your network, because the stream has no password.

### Fixed

- HomeKit through Scrypted froze on the first frame of the live view, and
  Scrypted lost the pre-roll of HomeKit Secure Video clips. Every packet of a
  video frame now carries the same RTP timestamp, and the parameter sets
  injected before each keyframe get consecutive sequence numbers (#88).
- An SD H.264 stream never reported that it had started, which left the
  camera locked and stalled every later client before it got an answer (#81).

### Changed

- Built with Go 1.27, pion/webrtc 4.2.23 and the matching pion libraries.
