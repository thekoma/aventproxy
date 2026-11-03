# Changelog

This is the dev card: it follows builds off main, made for testing before an rc.
Stay on the stable card unless you are testing, and use the beta card if you only want release candidates.

## Unreleased (dev4)

**Bridge: viewers no longer stall at DESCRIBE ([#89](https://github.com/thekoma/aventproxy/issues/89), [#97](https://github.com/thekoma/aventproxy/issues/97)).**
- Every RTSP viewer gets its answers at once while its camera's stream starts. Only the first one used to: the others waited for the whole cloud attempt, 20 s or more when the camera did not answer, and go2rtc gives up after 5 s and retries, so it never got past DESCRIBE.
- A start that fails closes every viewer that came for it, and the next viewer starts afresh.
- A cloud session that connects but sends no video for 20 s counts as failed, with the warning `Cloud session for <camera> is up but carried no video in 20s`. Its viewers reconnect to a new session instead of waiting on a dead one.
- With two or more cameras, the bridge keeps one connection to Tuya's MQTT broker for all of them. It used to open one per camera under the same client id, and the broker drops the older connection when a newer one arrives, so each camera's start could cut the other's off.
- A start that times out says what it waited for, on the `Failed to start WebRTC bridge` line: `the camera never answered the offer`, `the camera answered, but the connection to it never came up`, or, on HEVC, `connected, but the camera never confirmed the video codec`.
- A connection that never sends a request is closed after 60 s, and the bridge backs off when accepting connections keeps failing instead of filling the log.

## Unreleased (dev3)

Part of this is in the integration and part in this card, so update both to dev3.

**If your monitor speaks Tuya LAN protocol 3.4 or newer.** On 3.3 the bridge log says `Not using LAN signalling for <camera>: protocol 3.3`.
- The integration reads the monitor's state over its own LAN session and gets motion and sound alarms pushed, within seconds.
- Once the monitor has ever reported an alarm in DPS 212, the cloud poll pauses while the session is up, and commands stay on the LAN. The poll is back within 30 s if the session drops.
- Turning Motion Alert on turns Sound Alert off, and the other way round, because the monitor runs one detector at a time.
- This follows Leonard Pitzu's measurements on his 3.5 monitor. None of it has been tested here on a 3.4 or 3.5 monitor.

**Fixes for everyone**
- On 3.3, LAN pushes no longer hold off the cloud poll. Frequent pushes (SenseIQ) could delay it, and with it every motion and sound alert, which arrive only through that poll.
- A pushed update that landed during the LAN keep-alive was thrown away. It is now kept.
- A command that never left over the LAN now goes through the cloud. A lullaby next or previous is no longer sent twice after an unclear error.
- While the integration's LAN session is up, a cloud outage leaves the entities available. On 3.3, Motion, Sound and Crying Detected read unavailable until the cloud is back, because their alerts come only from the cloud.
- A re-paired monitor (new local key) is picked up without restarting Home Assistant.
- New Baby Moving sensor. Sleep stage now knows Out of bed and Settling.
- Bridge, for Frigate, go2rtc and other RTSP viewers:
  - viewers keep the picture when a cloud retry replaces the session;
  - a viewer that stops reading for 5 s is dropped instead of freezing the others;
  - viewers on another machine get UDP media, which used to go to localhost;
  - audio is timed by its samples, which may stop crackle.
- With LAN streaming off, the bridge config no longer holds the local key and P2P password.

**Issue #72 test: a stream pauses a playing lullaby.** Two options on this card only:
- `speaker_off`, on by default here. Once a cloud session connects, the bridge asks the monitor to keep its speaker off. Two-way audio overrides it, and LAN-signalled streams never send it. The stable and beta cards do not send it.
- `webrtc_audio`. `recvonly` (the default) asks the monitor for audio as before. `none` asks for video only, to see whether the monitor needs audio to open its speaker. RTSP viewers then get a silent audio track.

After updating, check that `speaker_off` shows as on in this card's Configuration tab. To tell whether it works, pause Frigate and any other viewer, start a lullaby, then open the camera for a minute. The lullaby should keep playing. Ten seconds after the session connects, the bridge log says one of two things:
- `Audio from <camera> still flows with its speaker kept closed: N packets in 10s`: good.
- A warning, `No audio from <camera> since asking the monitor to keep its speaker closed`: the request muted the stream. Turn `speaker_off` off to get the audio back.

Either way, please report what you saw on #72.

**Log wording.** The bridge names each stream by its signalling: `Streaming <camera> with cloud signalling` or `… with LAN signalling`. The old wording was "over the cloud" and "over the LAN". The video of a cloud-signalled stream already travels over your LAN when the monitor is reachable; only the signalling goes through Tuya. New bridge lines: `Offer to …`, `Answer from …`, `Asked <camera> to keep its speaker off (issue #72)`, the 10 s audio check above, `Cloud session for … carried … packets` when a session ends, and `Cloud sessions: audio …, speaker off …` at start. New integration lines: `Cloud poll for <camera> paused` and `resumed`, `Cloud for <camera> failed …` and `Cloud for <camera> answers again`.

If something broke after this update, open an issue with your monitor model, firmware, the integration diagnostics and the bridge log.

## Unreleased (dev2)

The bridge now uses the LAN only for monitors that speak Tuya LAN protocol 3.5. Monitors on protocol 3.3, such as an SCD973 on firmware 1.4.0, keep streaming over the cloud without a 4-second detour and without probing the LAN every 5 minutes. Update the integration and this card to dev2 together: a dev2 bridge with a dev1 integration never uses the LAN and logs "protocol unknown".

## Unreleased (dev)

Streams now go over your local network when the monitor allows it, and fall back to the cloud when it does not. This is Leonard Pitzu's work (#51), ported and hardened. It has only been tested on an SCD973/26. If your stream stopped working after this update, turn off **LAN streaming** in the integration options and open an issue with your monitor model and the bridge log.
