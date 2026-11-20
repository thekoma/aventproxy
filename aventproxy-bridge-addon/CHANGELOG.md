# Changelog

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
- New Baby Moving sensor. Sleep stage knows Out of bed and Settling.
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
