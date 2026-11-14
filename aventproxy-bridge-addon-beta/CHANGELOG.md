# Changelog

This is the beta card: it follows release candidates of the integration.
Stay on the stable card unless you are testing an rc.

## 2026.11.0-rc2

Update the integration and this card together, as with rc1.

**Talk from Home Assistant 2026.11 (experimental).** With **Two-way audio (talkback)** on in the integration, the camera tells Home Assistant 2026.11 that it takes audio, and the camera dialog shows a microphone once the two-way audio preview is on under Settings → System → Labs. It needs HTTPS and a WebRTC stream; an older Home Assistant ignores it.
- New option `talkback_mode`, `on_demand` by default. The stream stays one-way, so imm and the LAN stay in use and a playing lullaby keeps playing. While audio arrives on the RTSP backchannel (Home Assistant sends it only while its microphone is on), the bridge opens a second, two-way session and closes it 3 seconds after the audio stops. The lullaby pauses while that session is open, and the first second or two of a talk are lost while it opens.
- `session` is the rc1 behaviour, where every stream asks the monitor for two-way audio.
- The talk audio now reaches the monitor as 16 kHz PCM, the format it plays. G.711 came out as hissing on an SCD923/26 and an SCD951/26 (Luuk Visser, [#111](https://github.com/thekoma/aventproxy/pull/111)). Not yet tried on an SCD973.
- The log says `Talking to <camera>: the two-way session took …` and `Stopped talking to <camera> …`. Please post those lines on [#72](https://github.com/thekoma/aventproxy/issues/72), and say whether your voice came through and whether the lullaby carried on afterwards.

**Fixes**
- Video packets that Wi-Fi delivers out of order are put back in order before they reach the viewers, which removes a smeared, green lower half of the picture until the next keyframe (Wessel Blokzijl, [#112](https://github.com/thekoma/aventproxy/pull/112)).
- SCD953P on firmware 2.4.1: picking a lullaby plays it, and the track select shows the track that plays (czaja, [#116](https://github.com/thekoma/aventproxy/pull/116)).
- SCD971/26: the SenseIQ nightly summary shows light, deep and awake time instead of zeros (Kenny, [#108](https://github.com/thekoma/aventproxy/pull/108)).
- On a monitor on protocol 3.4, Motion Alert and Sound Alert are independent again. rc1 paired them from 3.4 up, but only a 3.5 monitor runs one detector at a time ([#110](https://github.com/thekoma/aventproxy/issues/110)).

**New action `philips_avent.get_sleep_history`** (Kenny, [#109](https://github.com/thekoma/aventproxy/pull/109)). It returns the SenseIQ daily summary for a range of days, each session with its stage timeline. The cloud keeps about 30 days, so an older start date is moved up to 29 days ago.

`cloud_transport: imm` has now kept the lullaby playing on an SCD953P too ([#72](https://github.com/thekoma/aventproxy/issues/72)). It stays opt-in for this rc.

## 2026.11.0-rc1

Update the integration and this card together. Part of what follows is in each, and the bridge streams over the LAN only with the protocol version that this integration writes into its config.

**Local streaming ([#51](https://github.com/thekoma/aventproxy/issues/51)).** Ported from Leonard Pitzu's fork, with his measurements. On monitors that speak Tuya LAN protocol 3.5, the bridge negotiates the stream with the monitor over your network and uses cloud signalling only when that fails. Each failure in a row doubles how long it waits before trying the LAN again, up to an hour. Monitors on 3.3, such as an SCD973 on firmware 1.4.0, stay on cloud signalling, and the video of such a stream usually travels over your LAN already. The integration option **LAN streaming**, on by default, turns it off. The bridge log says `Streaming <camera> with LAN signalling` or `Not using LAN signalling for <camera>: protocol 3.3`.

**New option `cloud_transport` ([#72](https://github.com/thekoma/aventproxy/issues/72), [#59](https://github.com/thekoma/aventproxy/issues/59)), experimental.** `webrtc`, the default, changes nothing. With `imm`, a stream that does not take the LAN first tries the Philips app's own transport over cloud signalling, and falls back to webrtc when that fails. On an SCD973 on firmware 1.4.0 (protocol 3.3) it came up in under a second and a playing lullaby kept playing. With webrtc, the same monitor flags the session as the app talking and stops the lullaby until the stream closes. It needs the monitor on your local network. The log says `Streaming <camera> with imm over cloud signalling`, or one warning names the step that failed and that camera skips imm for 5 minutes. A start can take up to 16 seconds before the fallback, 20 when a LAN attempt runs first. Please report on #72, with your monitor model and firmware.

**Alerts on protocol 3.4 and newer.** The integration keeps a LAN session with the monitor and gets motion and sound alarms pushed within seconds, and commands stay local. Turning Motion Alert on turns Sound Alert off, because the monitor runs one detector at a time. None of this has been tested here on a 3.4 or 3.5 monitor.

**Fixes**
- RTSP viewers no longer stall at DESCRIBE while their camera's stream starts ([#89](https://github.com/thekoma/aventproxy/issues/89), [#97](https://github.com/thekoma/aventproxy/issues/97)).
- With two or more cameras, one MQTT connection serves all of them. Each camera's start used to cut the other's off.
- A cloud session that carries no media for 20 s is replaced, with the warning `Cloud session for <camera> is up but carried no media in 20s`. A session that carries only audio, as in privacy mode, is kept.
- Viewers keep the picture when a cloud retry replaces the session, a viewer that stops reading for 5 s is dropped instead of freezing the others, RTSP over UDP works from another machine, and audio is timed by its samples, which may stop crackle.
- A start that times out says what it waited for, on the `Failed to start WebRTC bridge` line.
- On 3.3, LAN pushes no longer delay the cloud poll, and with it the motion and sound alerts.
- A lullaby next or previous no longer skips tracks when the monitor answers slowly.
- While the integration's LAN session is up, a cloud outage leaves the entities available, and a re-paired monitor is picked up without restarting Home Assistant.
- New Baby Moving sensor. Sleep stage knows Out of bed and Settling.
- A temporary bridge config that a crash left in `/config` is removed.

If something broke, turn off **LAN streaming** and set `cloud_transport` back to `webrtc`, then open an issue with your monitor model, firmware, the integration diagnostics and the bridge log.

## 2026.10.1-rc3

The bridge is unchanged from rc1. In the integration, Baby Detected no longer
stays on after the baby leaves the crib on monitors that push SenseIQ status
over the LAN. Please report on #74.

## 2026.10.1-rc2

The bridge is unchanged from rc1. In the integration, SenseIQ entities now
appear when their data first arrives, not only at startup, so data that shows
up after Home Assistant starts still gets its entities. Please report on #74.

## 2026.10.1-rc1

The bridge is the same as in 2026.10.0. This rc exists to test SenseIQ
sleep and breathing in the integration (#87); please report on #74.

### ⚠️ BREAKING CHANGE: read this before updating

**The RTSP stream is now only reachable from this host.** The bridge's RTSP
server has no password, so it now listens on `localhost` (127.0.0.1 and ::1)
instead of every network interface.

**Nothing to do** if Home Assistant opens the stream at the default
`rtsp://localhost:38554/...`.

**You must change a setting** if you open the stream from another machine
(VLC, Frigate, Scrypted or HomeKit through Scrypted, a separate go2rtc), or if
you set the integration's **Bridge host** option to this machine's IP address.
Set the add-on option **RTSP bind address** to `0.0.0.0` or to a specific
address, save, restart the add-on, and restrict port 38554 on your network.

### Fixed

- HomeKit through Scrypted no longer freezes on the first frame: every packet
  of a video frame carries the same RTP timestamp (#88).
- An SD H.264 stream no longer leaves the camera locked for later clients
  (#81).

### Changed

- Built with Go 1.27, pion/webrtc 4.2.23 and the matching pion libraries.
