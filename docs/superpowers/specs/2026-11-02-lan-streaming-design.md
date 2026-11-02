# LAN Streaming Design

**Date:** 2026-11-02
**Status:** Approved
**Context:** Baby monitor: a stream that silently shows nothing is worse than one that is slow
**Issue:** #51
**Origin:** port of Leonard Pitzu's work in [leonardpitzu/aventproxy](https://github.com/leonardpitzu/aventproxy), 82 commits on top of our `2026.8.0`

## Intent

Stream video and audio from the monitor over the local network whenever that
works, and use the cloud WebRTC path only when it does not. The maintainer's
preference is plain: LAN always, if possible. The video stays inside the house,
a slow or flaky Tuya cloud stops mattering, and a stream keeps working through a
WAN outage once the monitor has booted.

Leonard Pitzu reverse engineered the local signalling and media path and got it
working on his SCD973/26. His fork has since moved far from ours (own 2.x line,
own CI and packaging, no security work from mid-August), so it cannot be merged.
We port the LAN streaming work onto our code, harden it, and credit him.

## Goals

1. A stream opened through the bridge goes over the LAN when the monitor is
   reachable locally, with no cloud call per session.
2. When the LAN path fails to start, the same stream falls back to the cloud
   path. The client sees a slower start and, at worst, one reconnect.
3. When the LAN path dies mid-stream, the stream restarts, and the restart
   goes to the cloud.
4. RTSP consumers (Home Assistant, go2rtc, Frigate, Scrypted, VLC) see the same
   SDP whichever path delivers the media. Nothing downstream changes.
5. The bridge starts and serves LAN streams even when the cloud is unreachable
   at bridge start, and cloud fallback becomes available once the cloud is back,
   without a restart.
6. A stream that fell back to the cloud moves back to the LAN on its own once
   the LAN works again, even when its viewers never disconnect (Frigate,
   Scrypted, an always-on go2rtc).
7. LAN streaming is on by default for every model, and a user can turn it off
   in the integration options.
8. Leonard Pitzu is credited in every ported commit, in the docs and in the
   release notes.

## Non-goals

- Talkback over the LAN. The fork does not implement it; a stream with talkback
  enabled keeps using the cloud path.
- SD (`/sd`) or HEVC streams over the LAN. The fork ignores the resolution and
  assumes H.264; those streams keep using the cloud path.
- Running the monitor with no internet at all. Its firmware validates TLS
  against a built-in trust store and ignores local offers until it has reached
  Tuya once since its last boot (fork `PROTOCOL.md` §20–21). Not fixable here.
- Making the Home Assistant integration start without the cloud. Its first
  refresh still needs the cloud; only the bridge gains a cloud-free start.
- A Home Assistant entity showing which path a stream uses. The bridge log says
  it; an entity can come later if people ask.
- The other fixes found in the fork (see "Follow-ups"). They are separate work.

## Anti-goals

- **A black stream reported as healthy.** The fork declares the LAN stream
  started once the login is sent, so a wrong password yields an "active" stream
  with no video and no fallback. Success must mean media arrived.
- **A LAN attempt that can freeze the server.** The fork's negotiation read has
  no deadline and runs under `cs.mutex`; issue #97 showed what one stuck start
  does to every client.
- **Changing what consumers receive.** The fork announces L16/16000 audio to
  everyone. We do not.
- **Weakening the secrets handling.** The bridge JSON gains the P2P password and
  the local key; they get the same treatment as the existing credentials.

## How the LAN path works

Summary of the fork's implementation, verified by reading its code
(`aventproxy-bridge-addon/avent-webrtc-bridge/pkg/lan/`). Its `PROTOCOL.md`
§14–16 has the full research.

- **Inputs**, all fetched once by the integration and written into the bridge
  JSON: the account `uid`; per camera the `localKey`, the P2P `password` from
  `smartlife.m.rtc.config.get`, and the monitor's LAN IP.
- **Signalling** over TCP 6668 in Tuya 3.5 "6699" frames: AES-GCM with the
  header as AAD; the session key comes from a cmd 3/4/5 nonce exchange
  (`AES-GCM(localKey, localNonce xor remoteNonce)[:16]`). Cmd 32 carries a
  302 JSON offer with `path:"lan"` and an `m=application imm` SDP. The GCM IV
  must be ASCII taken from the trace UUID (a random IV gets an ACK and nothing
  else), and the ICE server list must be a non-empty IP literal, hence a
  placeholder `stun:192.0.2.1`.
- **ICE** is hand-rolled because the monitor always takes the controlling role
  and sends no MESSAGE-INTEGRITY, which pion's agent rejects.
- **Media**: KCP on the same UDP socket, conversations demultiplexed by id
  (conv 0 control, the monitor picks the media conv); each datagram is a KCP
  segment plus an HMAC-SHA1 trailer, payload 16-byte IV plus AES-128-CBC keyed
  by `a=aes-key` from the answer. Login on conv 0 is `admin` plus
  `md5(password + "||" + localKey)`, followed by six startup messages replayed
  verbatim.
- **Framing**: 32-byte header, then a sub-header in a 16-byte or an 8-byte form
  (told apart by the length field). One message is one RTP payload: SPS/PPS
  arrive whole, slices already FU-A. The bridge sets the marker from the FU end
  bit, re-fragments whole NALs over 1200 bytes, and lowers the SPS `level_idc`.
- **Audio**: 16 kHz signed 16-bit little-endian mono PCM.

## Decisions

### Made by the maintainer

- LAN first, always, whenever it works; the cloud is the fallback.
- On by default for every model, although only an SCD973/26 has been tested.
  The release notes say so and ask anyone whose stream breaks to open an issue
  with their model.
- Release path: a `dev` build first, tested on the maintainer's SCD973; if it
  works, an `rc` for one week; then a `release`.
- Port the features ourselves rather than ask the author for a PR, and credit
  him.

### Made by me (review these)

- **Audio stays as announced today.** In add-on mode `CameraInfo.Skill` is
  empty, so `generateSDP` announces `PCMU/8000` (`pkg/rtsp/protocol.go:573-595`).
  The LAN path downsamples its 16 kHz PCM to 8 kHz and encodes it with G.711 in
  whatever law the SDP announced (μ-law today, A-law if the skill says codec
  106). Narrowband is what the cloud path delivers anyway.
- **Which streams try the LAN**: HD, H.264, talkback off, and the camera has a
  `local_key`, `password` and either a `lan_ip` or a discovery result.
  Everything else goes straight to the cloud, as today.
- **Success means the first video frame arrived** within 4 s of the start of
  the attempt (dial, negotiation, ICE and login included). No frame in time is
  a failed attempt and the cloud is tried next, in the same stream start.
  The bound comes from the clients: `handleDescribe` answers with a fixed SDP
  straight away (`pkg/rtsp/protocol.go:288`), so during the attempt a client is
  simply receiving nothing, and go2rtc drops a source after 5 s without data.
  A client that gives up reconnects onto the same stream, which by then is on
  the cloud (this relies on the #97 `pending` fix).
- **Mid-stream LAN failure** (read loop or keep-alive error, or no media for a
  few seconds) goes through the existing `handleBridgeError`, which closes the
  clients and clears the stream; the reconnecting client gets a fresh stream.
- **LAN cool-down per camera.** Any LAN failure, at start or mid-stream, makes
  that camera skip the LAN for 5 minutes. A camera whose LAN never works (stale
  password, an untested model) then pays the 4 s once, not on every open.
- **Every blocking step has a deadline**, the LAN attempt honours a context
  cancelled by `Stop()`, and no server-wide lock is held while it waits.
- **Back to the LAN after a fallback.** Without this, a consumer that never
  disconnects stays on the cloud for good after one LAN failure, and that
  includes every monitor power cut: after a boot the monitor ignores local
  offers until it has reached Tuya, so the first attempt always falls back.
  When the cool-down ends, a stream running on the cloud makes a background LAN
  attempt that leaves the cloud session alone. If that attempt delivers a video
  frame, it is closed and the stream is ended through the error path; the
  clients reconnect, and the fresh stream takes the LAN. If it fails, the
  cool-down starts again. The price is one reconnect of a couple of seconds at
  each switch, which is accepted. Swapping the source under the running
  forwarder would avoid it, at the cost of RTP identity and timestamp work that
  is not worth it now.
- **A switch to turn the LAN off**, because the first-frame check cannot see a
  stream that flows but does not decode (the fork's `PROTOCOL.md` describes
  that failure on the one tested model), and without a switch an affected user
  can only downgrade. It is an integration option, "LAN streaming", on by
  default, carried to the bridge as a top-level `lan_streaming` boolean, the
  same way `talkback` is today. A bridge config without the field means on;
  such a config also carries no LAN credentials, so it ends up on the cloud
  anyway.
- **Answers are matched by `sessionid`**: the monitor sends cmd-32 frames to
  every LAN client, so the phone app's or another stream's answer must not be
  taken as ours.
- **Prerequisite: issue #97 lands first.** Its `pending` fix and a deadline on
  `waiter.Wait()` touch the same `startStream` path. Either the reporter's PR or
  ours.
- **`local_key` freshness**: the key from the latest cloud poll wins over the
  stored one, so a re-paired monitor does not keep a stale key. The fork does
  the opposite.
- **`lan_ip` follows the integration's tinytuya session**: whenever the address
  the LAN client connected to changes, the bridge JSON is rewritten. The fork
  publishes it once, 60 s after setup.
- **Bridge discovery stays as a fallback** for an empty `lan_ip` (UDP 6667,
  SO_REUSEPORT so it coexists with Home Assistant's own scan).

## What changes

### Bridge (`avent-webrtc-bridge/`)

- `pkg/lan/`: port of the fork's `client`, `discover`, `frame`, `ice`, `media`,
  `session`, `signal`, without its measurement code (`Stats`, sampling,
  `OnRawMedia`, `framing_probe.go`, `access_unit.go`), adapted from
  `pion/stun/v3` to the `v4` we already use, with deadlines and context
  cancellation added.
- `pkg/rtsp/`: a LAN source that feeds the existing `RTPForwarder` (marker from
  the FU end bit, NAL re-fragmentation, SPS level fix, PCM to G.711), and the
  LAN-first selection inside `startStream`, keeping our retry, `replaceBridge`
  and error-handler logic. Our forwarder already does one timestamp per access
  unit (#88), which the fork's `00d7e2f` duplicates.
- `cmd/addon` and `pkg/storage`: the new config fields; a cloud login that is
  allowed to fail at start and is retried lazily when a stream needs the cloud.
- New dependency: `github.com/xtaci/kcp-go/v5` (and its transitive
  `klauspost/reedsolomon`, `klauspost/cpuid`, `pkg/errors`, `tjfoc/gmsm`);
  `golang.org/x/sys` becomes direct for SO_REUSEPORT.
- Logging: one INFO line per stream saying which path it took, and why a LAN
  attempt was skipped or failed.

### Bridge config contract (`philips_avent_bridge.json`)

- Top level: `uid`, and `lan_streaming` (boolean, absent means `true`).
- Per camera: `local_key`, `p2p_password`, `lan_ip` (empty when unknown).
- The file keeps its `0600` write (`payload.write_bridge_config_file`) and is
  rewritten only when a value actually changes, so the md5 supervisor in
  `run.sh` does not restart the bridge for nothing (#73).

### Integration (`custom_components/philips_avent/`)

- `payload.build_bridge_config` and `build_cameras_payload`: the new fields.
- `const.py`, `config_flow.py` options step, `strings.json` and
  `translations/en.json`: the "LAN streaming" option (`CONF_LAN_STREAMING`,
  default on), next to two-way audio.
- `__init__.py`: fetch the P2P password from `api.get_rtc_config` for each
  camera at every setup (Home Assistant needs the cloud at that point anyway),
  and write it with the local key and LAN IP into the bridge JSON, which is
  rewritten only when a value changes. `get_rtc_config` has no caller today, so
  the first implementation step calls it twice on the maintainer's account to
  confirm the password is stable. If it turns out to change per call, the
  integration keeps the stored value and refreshes it only when `local_key`
  changes, so the bridge is not restarted on every Home Assistant start (#73).
- `lan.py`: expose the address the tinytuya session uses; the coordinator
  notifies when it changes.
- `redact.py`: redact `p2p_password`, `local_key` (already covered as
  `localKey`; check the snake_case spelling) and `lan_ip` in both the debug log
  and diagnostics.
- `.gitleaks.toml`: a rule for the P2P password, like the existing `ECODE` and
  `LOCAL_KEY` rules.

### Docs and release

- `WHITEPAPER.md`: a LAN streaming section summarising the protocol, crediting
  Leonard Pitzu and linking his `PROTOCOL.md`.
- `README.md`: LAN-first streaming, the boot-time internet requirement, the
  cases that still use the cloud, the option to turn it off, and credit.
- Release notes for each channel (dev, rc, release): tested on SCD973/26 only;
  if your stream stopped working, turn off "LAN streaming" in the integration
  options and open an issue with your model and the bridge log.
- A comment on #51 telling the author what was ported and thanking him.

## Credit

- Every commit that ports his code carries
  `Co-Authored-By: Leonard Pitzu <leonardpitzu@users.noreply.github.com>`, the
  form our history already uses (e343db9, e4860c3).
- Commit messages name the fork commits they port.
- His fork keeps our MIT licence (`Copyright (c) 2026 thekoma`); the bridge
  directory keeps its own MIT licence from seydx. No new files in the fork carry
  their own headers.

## Testing

- Unit tests, written against captured bytes where the fork has them:
  6699 frame encode/decode, session key derivation, cmd-32 offer/answer and
  `sessionid` matching, both media sub-header forms, KCP payload decryption,
  H.264 marker and re-fragmentation, SPS level rewrite, PCM to G.711.
- Selection logic with a fake LAN source behind a seam like the existing
  `startStreamOverride`: LAN success, LAN timeout falling back to cloud, LAN
  dying mid-stream then cloud on reconnect, cool-down after a failed start,
  the background attempt moving a cloud stream back to the LAN (and leaving it
  alone when the attempt fails), `lan_streaming` off, talkback and `/sd`
  skipping the LAN, `Stop()` during a LAN attempt.
- Python: the new bridge JSON fields, the option and its default, persistence
  only on change, the fresh key winning, redaction of the new fields.
- Hardware, on the maintainer's SCD973 with the `dev` card:
  1. Open the camera in Home Assistant: the bridge log shows the LAN path,
     video and audio play.
  2. Block TCP 6668 to the monitor: the stream falls back to the cloud.
  3. Cut the WAN after the monitor booted: an open stream keeps playing and a
     new one starts.
  4. Restart the bridge with the WAN cut: LAN streams start.
  5. Go2rtc and a snapshot together right after a bridge restart (#97 shape):
     one stream, no hang.
  6. With a stream kept open on the cloud (TCP 6668 blocked), unblock it: the
     stream moves to the LAN within the cool-down, with one short reconnect.
     Also power-cycle the monitor with a viewer attached and check it ends up
     back on the LAN.
  7. Turn "LAN streaming" off: new streams use the cloud.

## Rollout

1. #97 fix merged.
2. The port, in reviewable pieces: `pkg/lan` with its tests; the LAN source and
   selection in `pkg/rtsp`; the config contract and the integration; docs.
3. `dev` release (dev card only, no GitHub release), hardware test above.
4. `rc` for one week with the release-note request for issues.
5. `release`.

## Follow-ups (not in this project)

Found in the fork while reading it, each worth its own small change:

- `lan.py` heartbeat with `nowait=False` can swallow a pushed alert in place of
  the ack (fork `8c4fc03`); our code is unchanged from 2026.8.0.
- SenseIQ stage codes `o` and `i` are not mapped, so the stage reads `None`.
- Motion and sound switches should turn each other off; the monitor runs one
  detector at a time.
- `select.py` mutates `coordinator.data` in place.
- `manifest.json` does not declare the `ffmpeg` dependency `camera.py` uses.
- Bridge: media sent before PLAY, UDP sent to localhost instead of the client
  address, no lock on the MQTT camera map, missing locks in `Stop` and stream
  cleanup, `ECONNREFUSED` not dropping the client.

## Left to the builder

- The exact PR split and the order of the follow-ups.
- Deadlines and the cool-down length, within the bounds above.
- The downsampling filter for 16 kHz to 8 kHz.
- Whether `cmd/lan` (the fork's standalone LAN test command) is worth keeping as
  a developer tool.
