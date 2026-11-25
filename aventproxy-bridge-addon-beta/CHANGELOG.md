# Changelog

This is the beta card: it follows release candidates of the integration.
Stay on the stable card unless you are testing an rc.

## 2026.11.2-rc1

Update the integration and this card together. This rc builds on 2026.11.1. The bridge has one small change; the rest is in the integration.

**Please check on protocol 3.3.** While the cloud is failing, a command that reached the monitor over the LAN is no longer copied to the cloud, and a copy that fails no longer turns that command into an error (Luuk Visser, [#173](https://github.com/thekoma/aventproxy/pull/173)). A command made just after the cloud comes back, before the next poll, is not copied either. If the monitor does not report a LAN change to the cloud on its own, the switch could flip back on that poll.

If you can cut Home Assistant off from the Tuya cloud (a firewall or DNS block on your region's API host, a1.tuyaeu.com in Europe), this is the check:
1. Block it and wait for the warning `Cloud for <camera> failed (…); its LAN session keeps it available…`. If the entities go unavailable instead, the monitor has no LAN session and the check does not apply.
2. Turn the night light on. It should change at once, with no error.
3. Remove the block and wait two minutes. Is the switch still on?
4. Block again until the warning comes back, then remove the block and, within a few seconds, turn the night light off. Wait two minutes. Is the switch still off?

Please post what you see on [#110](https://github.com/thekoma/aventproxy/issues/110), with your monitor model and firmware.

**Integration**
- Two monitors with the same name each show their own stream. Before, both camera entities pulled the first monitor's stream. The second monitor's device name gets the suffix the bridge already gives its stream path, such as "Philips Avent Baby Monitor f6e5d4"; entity ids do not change (Luuk Visser, [#140](https://github.com/thekoma/aventproxy/pull/140)).
- Snapshots read the bridge over TCP, so they also work when Home Assistant reaches the bridge through a published Docker port (Luuk Visser, [#172](https://github.com/thekoma/aventproxy/pull/172)).
- The integration's polls and commands to the Tuya cloud give up after 30 seconds instead of 5 minutes, and the outage warning names the error type when the error has no message (Luuk Visser, [#173](https://github.com/thekoma/aventproxy/pull/173)).
- A login the cloud no longer accepts, while a LAN session keeps the monitor available, gets its own warning, `Tuya session for <camera> is no longer valid`, instead of the generic outage line (Luuk Visser, [#175](https://github.com/thekoma/aventproxy/pull/175)).
- A reauthentication reloads the integration once instead of twice, and saving the options without a change no longer reloads it (Luuk Visser, [#174](https://github.com/thekoma/aventproxy/pull/174)).

**Add-on**
- A failed bridge start logs its error without the bridge's full list of options after it ([#170](https://github.com/thekoma/aventproxy/pull/170)).

## 2026.11.1-rc1

Update the integration and this card together. This rc builds on 2026.11.0, which went out after rc4 with one more bridge fix: RTSP clients that keep their session alive with `GET_PARAMETER`, such as Ajax NVRs, get `200 OK` instead of `501 Not Implemented` ([#129](https://github.com/thekoma/aventproxy/issues/129)).

**Please check on protocol 3.3.** The integration now counts a LAN heartbeat that the monitor leaves unanswered as a failure, and reconnects (Luuk Visser, [#144](https://github.com/thekoma/aventproxy/pull/144)). Before, a monitor that stopped answering kept looking connected, with its last values. This was measured on protocol 3.4. If a monitor on 3.3 never answers heartbeats, it would now reconnect every 10 to 20 seconds. With debug logging on for the integration, a few minutes of a 3.3 monitor with no `LAN heartbeat not answered` line is the check. Please post what you see on [#110](https://github.com/thekoma/aventproxy/issues/110), with your monitor model and firmware.

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

## 2026.11.0-rc4

The bridge image now runs on Debian 13 (trixie) instead of Debian 12 (bookworm), which security scanners flagged for packages the bridge never uses (Daniel Jansen, [#124](https://github.com/thekoma/aventproxy/pull/124)). Nothing else changed since rc3, in the bridge or in the integration.

## 2026.11.0-rc3

Only the bridge changed since rc2.

**Fix:** with **Two-way audio (talkback)** on, each still image of the camera opened a two-way session for 3 seconds, and a playing lullaby paused. Home Assistant takes a still image with ffmpeg, which sends an empty packet to every track of the stream, the return audio included, and the bridge took it for a voice. Empty packets are now ignored. rc2 and 2026.11.0-dev5 have the same problem. The bridge log showed it as a talk that sent nothing: `Stopped talking to <camera> (the audio stopped) after 3s: 0 packets sent, 1 dropped`.

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
