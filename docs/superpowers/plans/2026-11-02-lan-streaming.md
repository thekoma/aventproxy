# LAN Streaming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve every eligible stream over the local network, fall back to the cloud WebRTC path when the LAN fails, and move back to the LAN once it works again.

**Architecture:** A new `pkg/lan` package (ported from Leonard Pitzu's fork, hardened with deadlines, context cancellation, `sessionid` matching and a close callback) talks to the monitor. A `LANSource` in `pkg/rtsp` feeds the existing `RTPForwarder`, converting audio to the G.711 law the SDP already announces. `CameraStream.startStream` tries the LAN first under a 4 s first-frame deadline, falls back to the cloud, and a per-camera cool-down plus a background promotion bring cloud streams back to the LAN. The integration writes the LAN credentials and the option into the bridge JSON.

**Tech Stack:** Go 1.27 (pion/rtp, pion/stun/v4, new `github.com/xtaci/kcp-go/v5`), Python 3.11+ Home Assistant custom component.

**Spec:** `docs/superpowers/specs/2026-11-02-lan-streaming-design.md`

**Fork reference:** The fork is `leonardpitzu/aventproxy`; clone it read-only into a fresh empty directory (never build or run it). Its bridge lives in `aventproxy-bridge-addon/avent-webrtc-bridge/` (below: `FORK/`). Fork commit `682660f` equals our tag `2026.8.0`.

## Global Constraints

- Work on branch `feat/lan-streaming` off `main`. No push, PR, merge or release without the maintainer's explicit go.
- No AI attribution anywhere: never `Co-Authored-By: Claude` or "Generated with" lines in commits, PRs, issues or files.
- Every commit that ports fork code ends with `Co-Authored-By: Leonard Pitzu <leonardpitzu@users.noreply.github.com>` and names the fork files or commits it ports.
- Go: `cd avent-webrtc-bridge && go test ./... && go vet ./... && test -z "$(gofmt -l .)"` must pass after every task. Static build stays `CGO_ENABLED=0`.
- Python: `PYTHONPATH=. uv run --quiet --no-project --python 3.13 --with pytest --with pycryptodome --with aiohttp --with voluptuous python -m pytest tests/test_philips_avent/ -q` and `uvx ruff check custom_components/ examples/ tools/ --ignore E501` must pass after every Python task. (The repo's `.venv` is empty; this is the command that works on this machine.)
- Values fixed by the spec: first-frame deadline `4 * time.Second`; LAN cool-down `5 * time.Minute`; bridge JSON fields `uid`, `lan_streaming` (absent means `true`), per camera `local_key`, `p2p_password`, `lan_ip`; integration option `CONF_LAN_STREAMING = "lan_streaming"`, default `True`, label "LAN streaming".
- The LAN path is used only for streams that are HD, H.264, with talkback off, `lan_streaming` on, the camera has `local_key` and `p2p_password`, an address is known, and the camera is not in cool-down. Everything else takes the cloud path exactly as today.
- Consumers see the same SDP whichever path delivers the media: audio is G.711 at 8 kHz in the law `generateSDP` announces.
- The bridge JSON stays `0600` (`payload.write_bridge_config_file`) and is byte-identical when nothing changed, so `run.sh`'s md5 supervisor does not restart the bridge (#73).
- LAN secrets live in the bridge only in memory (`RTSPServer.LANTargets`), never in the add-on's camera registry: `findCamera` rereads that registry from disk on every request.
- Test fixture secrets stay under 10 characters (`"lk"`, `"pw"`), so `.gitleaks.toml` rules do not fire on them.
- Do not port: fork packaging/CI/versioning, `framing_probe.go`, `access_unit.go`, `Stats`/sampling/`OnRawMedia` instrumentation, the L16 audio format, `cmd/lan`.

## Review Focus

1. **Stale or wrong P2P password:** dial, answer and ICE succeed, the login is sent, no video ever arrives. Expected: the attempt fails within 4 s and the stream is on the cloud, never an "active" black stream. Test: Task 6 `TestLANSourceFailsWithoutFirstFrame`.
2. **The phone app watching at the same time:** the monitor sends cmd-32 answers to every LAN client. Expected: the bridge ignores answers whose `sessionid` is not its own. Test: Task 3 `TestReadSignalSkipsOtherSessions`.
3. **Home Assistant restarting with nothing changed:** expected byte-identical bridge JSON, so no bridge restart. Test: Task 10 `test_unchanged_inputs_give_identical_json`.
4. **A bridge JSON written by an older integration** (no `uid`, no LAN fields): expected the bridge starts, every stream takes the cloud path, no LAN attempt. Test: Task 8 `TestLoadConfigWithoutLANFields`.
5. **Viewer leaves, or the add-on stops, during a LAN attempt:** expected the attempt is cancelled at once, its sockets closed, no goroutine left blocked. Test: Task 7 `TestStopCancelsLANAttempt`.

---

### Task 1: Issue #97 (stream reuse race, unbounded answer wait, server-wide lock)

Prerequisite for everything else: the LAN selection lives in the same `startStream` path. If the reporter's PR for #97 has landed on `main` by now, keep only the parts below it does not cover.

**Files:**
- Modify: `avent-webrtc-bridge/pkg/rtsp/server.go` (`CameraStream` struct, `getOrCreateStream`, `AddClient`, `removeClient`)
- Modify: `avent-webrtc-bridge/pkg/utils/waiter.go`
- Modify: `avent-webrtc-bridge/pkg/rtsp/bridge.go:252`
- Test: `avent-webrtc-bridge/pkg/rtsp/stream_error_test.go`, create `avent-webrtc-bridge/pkg/utils/waiter_test.go`

**Interfaces:**
- Produces: `CameraStream.pending bool`; `func (w *Waiter) WaitTimeout(d time.Duration) error`; `var ErrWaitTimeout = errors.New("timed out waiting")` in `pkg/utils`; constant `answerTimeout = 20 * time.Second` in `pkg/rtsp/bridge.go`.

- [ ] **Step 1: Write the failing tests** (use the assertion style the package's existing tests use)
  - `TestGetOrCreateStreamReusesPendingStream`: `NewRTSPServer(0, nil)`, camera `{DeviceID: "d", DeviceName: "n"}`; two `getOrCreateStream(cam, "hd", nil)` calls before any `AddClient` return the same pointer.
  - `TestRemoveClientDoesNotHoldServerLockOnStream`: with the stream's `cs.mutex` held by the test, `server.removeClient(id)` runs in a goroutine; `server.mutex.TryLock()` succeeds within 100 ms.
  - `TestWaitTimeoutExpires`: a fresh `Waiter`; `WaitTimeout(50 * time.Millisecond)` returns `ErrWaitTimeout` in under 1 s.
  - `TestWaitTimeoutReturnsDoneError`: `Done(errors.New("boom"))` 10 ms later; `WaitTimeout(time.Second)` returns that error.

- [ ] **Step 2: Run them, expect FAIL** — `cd avent-webrtc-bridge && go test ./pkg/rtsp/ ./pkg/utils/ -run 'Pending|RemoveClientDoesNot|WaitTimeout' -v` → the pending test gets two different pointers, `WaitTimeout` is undefined.

- [ ] **Step 3: Implement**
  - `CameraStream.pending`: set `true` when `getOrCreateStream` creates a stream, reuse when `active || connecting || pending`, cleared in `AddClient` (the reporter's diff in #97).
  - `removeClient`: under `s.mutex` only look up and delete the client; release `s.mutex`, then call `client.stream.RemoveClient` and close the connection.
  - `WaitTimeout`: run `Wait` in a goroutine writing to a buffered channel; `select` on it and a timer.
  - `bridge.go`: `wb.waiter.Wait()` becomes `wb.waiter.WaitTimeout(answerTimeout)`.

- [ ] **Step 4: Run the Go gate (Global Constraints), expect PASS.**

- [ ] **Step 5: Commit** with message `fix(bridge): reuse a stream until its first client attaches, bound the answer wait (#97)` and the trailer `Co-Authored-By: AdamVyborny <27994036+AdamVyborny@users.noreply.github.com>` (the pending fix is his diff).

---

### Task 2: `pkg/lan` frames and session

Port `FORK/pkg/lan/frame.go` and `FORK/pkg/lan/session.go` (fork commits `05b9372`, `4f30e93`). Change: `Dial` takes a context whose deadline bounds dial and negotiation (the fork's negotiation read has no deadline).

**Files:**
- Create: `avent-webrtc-bridge/pkg/lan/frame.go`, `avent-webrtc-bridge/pkg/lan/session.go`
- Test: `avent-webrtc-bridge/pkg/lan/frame_test.go`, `avent-webrtc-bridge/pkg/lan/session_test.go`

**Interfaces:**
- Produces: `const Port = 6668`; `type Header` (fork fields); `func Pack6699(key, iv []byte, seq, cmd uint32, payload []byte) ([]byte, error)`; `func Unpack6699(key, frame []byte) (Header, []byte, error)`; `func StripRetcode(payload []byte) []byte`; `type Session struct`; `func Dial(ctx context.Context, ip, deviceID, localKey string) (*Session, error)`; `func (s *Session) Send(cmd uint32, iv, payload []byte) error`; `func (s *Session) Recv(timeout time.Duration) (Header, []byte, error)`; `func (s *Session) Heartbeat() error`; `func (s *Session) Close() error`; unexported `sessionKey(localKey, localNonce, remoteNonce []byte) ([]byte, error)` and `dialPort` (a variable, so tests can point `Dial` at a local listener).

- [ ] **Step 1: Write the failing tests**
  - `TestPack6699RoundTrip`: key `[]byte("0123456789abcdef")`, a 12-byte IV, seq 7, cmd 32, payload `{"a":1}`; `Unpack6699` returns the same seq, cmd and payload.
  - `TestPack6699LengthExcludesSuffix`: the header's length field covers IV, ciphertext and tag but not the 4-byte `0x9966` suffix (the bug fork commit `4f30e93` fixed).
  - `TestUnpack6699RejectsWrongKey`: unpacking with another key returns an error.
  - `TestSessionKeyMatchesSDK`: `sessionKey` of fixed inputs equals `AES-GCM(localKey, nonce=localNonce[:12]).Seal(localNonce xor remoteNonce)[12:28]`, computed in the test with `crypto/cipher`.
  - `TestDialHonoursContextDeadline`: a `net.Listen("tcp", "127.0.0.1:0")` that accepts and never writes; `Dial` with a 200 ms context returns an error within 1 s.

- [ ] **Step 2: Run, expect FAIL** — `go test ./pkg/lan/ -v` → package does not exist.

- [ ] **Step 3: Implement** by porting the fork files; set `conn.SetDeadline` from `ctx` before negotiation and clear it after; close the connection when `ctx` ends during `Dial`.

- [ ] **Step 4: Run the Go gate, expect PASS.**

- [ ] **Step 5: Commit** `feat(lan): Tuya 3.5 frames and session key negotiation` with the Leonard Pitzu trailer, naming `FORK/pkg/lan/frame.go`, `session.go`, `05b9372`, `4f30e93`.

---

### Task 3: `pkg/lan` signalling

Port `FORK/pkg/lan/signal.go`. Change: `ReadSignal` takes the offer's session id and skips frames for other sessions (the fork ignores `sessionid`, and the monitor sends cmd-32 frames to every LAN client).

**Files:**
- Create: `avent-webrtc-bridge/pkg/lan/signal.go`
- Test: `avent-webrtc-bridge/pkg/lan/signal_test.go`

**Interfaces:**
- Consumes: `Session.Send`, `Session.Recv` (Task 2).
- Produces: `TypeOffer`, `TypeAnswer`, `TypeCandidate`, `TypeDisconnect` (fork values); `SignalHeader`, `SignalMessage`, `SignalFrame`, `Offer` (fork fields); `func NewOffer(uid, deviceID, iceUfrag, icePwd string) (*Offer, error)`; `func (o *Offer) SessionID() string`; `func (o *Offer) SDP() string`; `func (o *Offer) SendOffer(s *Session) error`; `func (o *Offer) SendCandidate(s *Session, candidate string) error`; `func IVFromTraceID(traceID string) []byte`; `func ReadSignal(s *Session, sessionID string, timeout time.Duration) (*SignalFrame, error)`.

- [ ] **Step 1: Write the failing tests**
  - `TestOfferSDPDeclaresLANImm`: `SDP()` contains `m=application`, `imm`, `a=ice-ufrag:uf`, `a=ice-pwd:pw` and an `a=aes-key:` line of 32 hex characters.
  - `TestOfferHeaderUsesLANPath`: the offer frame's JSON has `"path":"lan"`, no `auth`, and exactly one ICE server, `stun:192.0.2.1`.
  - `TestIVFromTraceIDIsASCII`: 12 bytes, all printable ASCII, taken from the trace id.
  - `TestReadSignalSkipsOtherSessions`: over a `net.Pipe` session pair sharing one key, write an answer with `sessionid` "other" then one with the offer's id; `ReadSignal` returns the second.
  - `TestReadSignalTimesOut`: nothing written; error after the timeout.

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement** by porting `signal.go`; `ReadSignal` loops until the deadline, dropping frames whose `sessionid` differs.
- [ ] **Step 4: Run the Go gate, expect PASS.**
- [ ] **Step 5: Commit** `feat(lan): offer, answer and candidates over the local channel` with the trailer, naming `FORK/pkg/lan/signal.go`.

---

### Task 4: `pkg/lan` ICE, media and client

Port `FORK/pkg/lan/ice.go`, `media.go`, `client.go`, `media_test.go` (fork commits `05b9372`, `211151c`, `6755080`, `bdeb206`). Changes: `pion/stun/v3` becomes our `pion/stun/v4`; no `Stats`, sampling or `OnRawMedia`; the client takes a config struct, its `Start` is bounded by the caller's context, and a close callback reports a stream that dies (the fork's read loop and keep-alive exit silently).

**Files:**
- Create: `avent-webrtc-bridge/pkg/lan/ice.go`, `media.go`, `client.go`
- Modify: `avent-webrtc-bridge/go.mod`, `go.sum` (add `github.com/xtaci/kcp-go/v5` at the version the fork's `go.mod` pins)
- Test: `avent-webrtc-bridge/pkg/lan/media_test.go` (port), `avent-webrtc-bridge/pkg/lan/client_test.go`

**Interfaces:**
- Consumes: `Dial`, `Session` (Task 2), `NewOffer`, `ReadSignal` (Task 3).
- Produces:
  - `type VideoFrame struct { Width, Height, FPS int; Timestamp uint64; NAL []byte }`; `type AudioFrame struct { Timestamp uint64; Codec, SampleRate, Channels int; Samples []byte }` (Samples: signed 16-bit little-endian PCM).
  - `func LoginToken(devicePassword, localKey string) string`.
  - `type ClientConfig struct { DeviceID, LocalKey, Password, UID, IP string; OnVideo func(*VideoFrame); OnAudio func(*AudioFrame); OnClosed func(error) }`.
  - `func NewClient(cfg ClientConfig) *Client`; `func (c *Client) Start(ctx context.Context) error`; `func (c *Client) Close() error`.
  - `OnClosed` is called once, with a non-nil error, when a media read loop or the keep-alive fails while the client is open; a media read idle for `mediaIdleTimeout = 3 * time.Second` counts as failed. Never called after `Close` or after `ctx` is cancelled.
  - Unexported, for tests: `func (c *Client) consume(ctx context.Context, r mediaReader, aesKey []byte) error`, `type mediaReader interface { Read([]byte) (int, error); SetReadDeadline(time.Time) error }`.

- [ ] **Step 1: Write the failing tests**
  - Port `FORK/pkg/lan/media_test.go` (both sub-header forms parse to the right payload).
  - `TestSealOpenRoundTrip`: `open(key, seal(key, msg))` returns `msg`; flipping one byte of the HMAC trailer makes `open` fail.
  - `TestLoginToken`: `LoginToken("pw", "lk")` is the hex md5 of `"pw||lk"`.
  - `TestConsumeReportsDeadStream`: a fake `mediaReader` yields one sealed video message then `io.EOF`; `OnVideo` fires once and `OnClosed` once with a non-nil error.
  - `TestConsumeSilentAfterCancel`: same fake, context cancelled before the error; `OnClosed` is not called.

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement** by porting; `Start` uses `ctx` for dial, answer (`ReadSignal` with the time left on `ctx`), ICE and login, and closes everything it opened before returning an error.
- [ ] **Step 4: Run the Go gate, expect PASS.**
- [ ] **Step 5: Commit** `feat(lan): ICE, KCP media and the LAN client` with the trailer, naming the fork files and commits above.

---

### Task 5: H.264 and G.711 helpers for the LAN source

Port `FORK/pkg/rtsp/h264_level.go` and the `endsAccessUnit`/`split` logic of `FORK/pkg/rtsp/lan_bridge.go` (fork commits `e21b12e`, `9c8caf6`). New: the 16 kHz to 8 kHz downsampler and the G.711 encoders, and one helper that both the SDP and the LAN source use to pick the law.

**Files:**
- Create: `avent-webrtc-bridge/pkg/rtsp/lan_h264.go`, `avent-webrtc-bridge/pkg/rtsp/g711.go`
- Modify: `avent-webrtc-bridge/pkg/rtsp/protocol.go:572-595` (audio lines from `audioLawFor`)
- Test: `avent-webrtc-bridge/pkg/rtsp/lan_h264_test.go`, `avent-webrtc-bridge/pkg/rtsp/g711_test.go`

**Interfaces:**
- Produces:
  - `func minLevel(width, height, fps int) byte`; `func correctSPSLevel(nal []byte, level byte) []byte`; `func endsAccessUnit(payload []byte) bool`; `const lanRTPMTU = 1200`; `func splitNAL(payload []byte, p *codecs.H264Payloader) [][]byte`.
  - `type g711Law int` with `lawMu`, `lawA`; `func audioLawFor(skill *tuya.Skill) g711Law` (codec 106 is A-law, everything else μ-law, which is today's `generateSDP` rule); `func (l g711Law) payloadType() uint8` (0 or 8); `func (l g711Law) rtpmap() string` (`"PCMU/8000"` or `"PCMA/8000"`).
  - `func pcm16LE(b []byte) []int16`; `type halfRateDownsampler struct`; `func (d *halfRateDownsampler) process(in []int16) []int16` (returns `len(in)/2` samples after a low-pass; the filter is the builder's choice); `func encodeG711(l g711Law, samples []int16) []byte`.

- [ ] **Step 1: Write the failing tests**
  - `TestEncodeG711KnownValues`: μ-law of `0, 32767, -32768` is `0xFF, 0x80, 0x00`; A-law of the same is `0xD5, 0xAA, 0x2A`.
  - `TestDownsamplerHalvesAndPassesLowTone`: 320 samples of a 1 kHz sine at 16 kHz give 160 samples whose RMS is within 10 % of the input's; a 7 kHz sine comes out at least 20 dB lower.
  - `TestAudioLawMatchesSDP`: for skills with audio codec 101, 105, 106, no audio, and a nil skill, the audio `rtpmap` in `generateSDP`'s output equals `audioLawFor(skill).rtpmap()`.
  - `TestEndsAccessUnit`: FU-A with the end bit is true, without it false; NAL types 1 and 5 true; 7 (SPS) false.
  - `TestSplitNALKeepsFragmentsWhole`: a 2000-byte FU-A payload (type 28) comes back as one piece; a 2000-byte type-5 NAL comes back as FU-A pieces of at most 1200 bytes.
  - `TestCorrectSPSLevelRaisesTooLowLevel`: an SPS with `level_idc` 30 and the level from `minLevel(1920, 1080, 15)` comes back with byte 3 equal to that level.

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement**; `generateSDP` builds its audio lines from `audioLawFor`, so the SDP and the LAN audio cannot disagree.
- [ ] **Step 4: Run the Go gate, expect PASS.**
- [ ] **Step 5: Commit** `feat(rtsp): H.264 and G.711 helpers for local media` with the trailer, naming `FORK/pkg/rtsp/h264_level.go`, `lan_bridge.go`, `e21b12e`, `9c8caf6`.

---

### Task 6: `LANSource`

Port `FORK/pkg/rtsp/lan_bridge.go` as `LANSource`, minus the framing probe. Changes: `Start` returns only after the first video frame reached the forwarder, audio goes out as G.711, and a dead LAN session is reported through the error callback.

**Files:**
- Create: `avent-webrtc-bridge/pkg/rtsp/lan_source.go`
- Test: `avent-webrtc-bridge/pkg/rtsp/lan_source_test.go`

**Interfaces:**
- Consumes: `lan.ClientConfig`, `lan.NewClient` (Task 4); helpers of Task 5; `RTPForwarder.ForwardVideoPacket`, `ForwardAudioPacket`.
- Produces:
  - `type LANTarget struct { LocalKey, Password, IP string }`
  - `type lanStreamer interface { Start(ctx context.Context) error; Stop(); SetOnError(func(error)) }`
  - `type LANSource struct` implementing `lanStreamer`; `func NewLANSource(deviceID, deviceName, uid string, target LANTarget, law g711Law, forwarder *RTPForwarder) *LANSource`.
  - `var errNoFirstFrame = errors.New("lan: no video within the first-frame deadline")`.
  - Test seam: `LANSource.newClient func(lan.ClientConfig) lanClient`, `type lanClient interface { Start(ctx context.Context) error; Close() error }`.

- [ ] **Step 1: Write the failing tests** (fake `lanClient` through the seam)
  - `TestLANSourceStartsOnFirstFrame`: the fake's `Start` returns nil and delivers one IDR `VideoFrame` 50 ms later through `OnVideo`; `Start` with a 1 s context returns nil, and the forwarder got one video packet with payload type 96 and the marker set.
  - `TestLANSourceFailsWithoutFirstFrame`: the fake starts but never delivers; `Start` with a 200 ms context returns `errNoFirstFrame` within 400 ms, and the fake was closed.
  - `TestLANSourceForwardsG711`: one 16 kHz mono `AudioFrame` of 320 samples gives one audio packet of 160 bytes, payload type 0 for μ-law and 8 for A-law.
  - `TestLANSourceDropsUnexpectedAudio`: a 48 kHz or a stereo frame forwards nothing.
  - `TestLANSourceReportsDeath`: after a successful start the fake calls `OnClosed(io.EOF)`; the error callback gets an error wrapping it, once.
  - `TestLANSourceStopIsQuiet`: `Stop`, then the fake calls `OnClosed`; the error callback is not called.

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement** by porting `lan_bridge.go` without `framing`; a random SSRC per session as in the fork; audio goes through `pcm16LE`, the downsampler and `encodeG711`.
- [ ] **Step 4: Run the Go gate, expect PASS.**
- [ ] **Step 5: Commit** `feat(rtsp): LAN media source with a first-frame check` with the trailer, naming `FORK/pkg/rtsp/lan_bridge.go`, `05b9372`, `00d7e2f`.

---

### Task 7: LAN-first selection and cool-down

Rebuild the fork's `tryLAN`/`connectCloud` (`FORK/pkg/rtsp/server.go:584-700`) on our `startStream`. Also move the MQTT client lookup out of `getOrCreateStream` into the cloud attempt, so a stream can exist without the cloud (needed by Task 8) and the #97 window shrinks.

**Files:**
- Create: `avent-webrtc-bridge/pkg/rtsp/lan_select.go` (eligibility, cool-down)
- Modify: `avent-webrtc-bridge/pkg/rtsp/server.go` (`RTSPServer`, `CameraStream`, `getOrCreateStream`, `startStream`, `replaceBridge`, `handleBridgeError`, `stopStreamInternal`, `Stop`)
- Test: `avent-webrtc-bridge/pkg/rtsp/lan_select_test.go`

**Interfaces:**
- Consumes: `lanStreamer`, `NewLANSource`, `LANTarget`, `audioLawFor`, `errNoFirstFrame` (Tasks 5–6).
- Produces:
  - On `RTSPServer`: `LanStreaming bool`, `UID string`, `LANTargets map[string]LANTarget` (by device id; set before `Start`, read-only after), `func (s *RTSPServer) lanAddress(deviceID string) string` (target IP, else Task 9's discovered address, else ""), `cooldown *lanCooldown`.
  - `type lanCooldown struct` with `func (c *lanCooldown) active(deviceID string) bool`, `func (c *lanCooldown) start(deviceID string)`, `func (c *lanCooldown) remaining(deviceID string) time.Duration`, and a `now func() time.Time` field for tests.
  - `const lanFirstFrameDeadline = 4 * time.Second`; `const lanCooldownPeriod = 5 * time.Minute`.
  - On `CameraStream`: `lan lanStreamer` (the running LAN source; nil on the cloud), `lanFactory func() lanStreamer` (defaults to `NewLANSource(...)` on the stream's forwarder), `cloudStartOverride func() error` (tests only), `attemptCancel context.CancelFunc`.
  - `func (cs *CameraStream) lanEligible() (bool, string)`; reasons: `"disabled"`, `"talkback on"`, `"sd stream"`, `"HEVC"`, `"no LAN credentials"`, `"no address"`, `"cooling down"`.

- [ ] **Step 1: Write the failing tests** (fake `lanStreamer` through `lanFactory`, fake cloud through `cloudStartOverride`)
  - `TestLANSuccessSkipsCloud`: eligible stream, the fake LAN starts; the stream ends `active` with `cs.lan` set, and the cloud was never called.
  - `TestLANFailureFallsBackToCloud`: the fake LAN returns `errNoFirstFrame`; the cloud is called once, the stream is `active`, `cs.lan` is nil, and `server.cooldown.active(deviceID)` is true.
  - `TestCooldownSkipsLAN`: cool-down active; the LAN factory is never called and the cloud is.
  - `TestIneligibleStreamsSkipLAN`: talkback on, resolution `sd`, `LanStreaming` false, a target without password, an empty address: each goes straight to the cloud, and `lanEligible` returns the matching reason.
  - `TestLANDeathRestartsOnCloud`: after a LAN start, the fake calls its error callback; the clients are closed (existing `handleBridgeError` behaviour), the cool-down is active, and a new stream for the same camera takes the cloud.
  - `TestStopCancelsLANAttempt`: the fake's `Start` blocks until its context ends; `cs.Stop()` from another goroutine makes it return within 100 ms, and the stream is not `active`.
  - `TestStreamCreatedWithoutCloud`: `getOrCreateStream` succeeds on a server with no `mqttManager` and no `MobileClient`.

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement**
  - `startStream`, first attempt: when `lanEligible`, mark `connecting`, derive a context with `lanFirstFrameDeadline` and keep its cancel in `attemptCancel`, release `cs.mutex` while `lan.Start` runs, take it back to commit. On success: set `cs.lan` and `active`, log `Streaming <camera> over the LAN`. On failure: start the cool-down, log `LAN failed for <camera>: <err>, using the cloud`, and continue with the existing cloud attempt on the same `webrtcBridge`. Do not set `bridgeStarted` for the LAN attempt, so RTSP clients already set up on its forwarder stay attached.
  - The cloud attempt fetches the MQTT client from `mqttManager` (what `getOrCreateStream` does today) right before `webrtcBridge.Start()`.
  - `stopStreamInternal` and `handleBridgeError` also stop `cs.lan`; `Stop` calls `attemptCancel`.
  - The LAN source's error callback starts the cool-down, then calls `handleBridgeError`.
- [ ] **Step 4: Run the Go gate, expect PASS.**
- [ ] **Step 5: Commit** `feat(rtsp): try the LAN first, fall back to the cloud` with the trailer, naming `FORK/pkg/rtsp/server.go` (`tryLAN`, `connectCloud`) and `05b9372`.

---

### Task 8: Add-on config, LAN targets and a cloud-optional start

Port the config side of fork commits `05b9372` and `e6e51a2` onto our `cmd/addon/addon.go`.

**Files:**
- Modify: `avent-webrtc-bridge/cmd/addon/addon.go`
- Modify: `avent-webrtc-bridge/pkg/rtsp/server.go` (lazy cloud readiness)
- Test: `avent-webrtc-bridge/cmd/addon/addon_test.go`, `avent-webrtc-bridge/pkg/rtsp/lan_select_test.go`

**Interfaces:**
- Consumes: `RTSPServer.LanStreaming`, `UID`, `LANTargets`, `LANTarget` (Tasks 6–7).
- Produces:
  - `BridgeConfig.UID string` (`json:"uid"`), `BridgeConfig.LanStreaming *bool` (`json:"lan_streaming"`); `Camera.LocalKey` (`json:"local_key"`), `Camera.P2PPassword` (`json:"p2p_password"`), `Camera.LanIP` (`json:"lan_ip"`).
  - `func lanStreamingEnabled(cfg BridgeConfig) bool` (nil means true).
  - `func lanTargets(cams []Camera) map[string]rtsp.LANTarget` (only cameras with both key and password).
  - `RTSPServer.CloudConnect func() error` (set by `runAddon`; does today's API verification and user lookup) and `func (s *RTSPServer) ensureCloud() error`, which calls `CloudConnect` at most once per 30 s until it succeeds; Task 7's cloud attempt calls it first.

- [ ] **Step 1: Write the failing tests**
  - `TestLoadConfigWithLANFields`: JSON with `uid`, `"lan_streaming": false` and a camera with the three fields parses into them; `lanStreamingEnabled` is false; `lanTargets` contains the camera.
  - `TestLoadConfigWithoutLANFields`: today's JSON, without the new fields, gives `lanStreamingEnabled` true and empty `lanTargets`, so every stream is ineligible with reason `"no LAN credentials"`.
  - `TestLanTargetsNeedKeyAndPassword`: a camera with only `local_key` is left out.
  - `TestEnsureCloudRetriesAfterInterval`: `CloudConnect` fails once; a second `ensureCloud` within 30 s returns the cached error without calling it; after moving the server's clock past 30 s it is called again and succeeds.

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement**
  - `runAddon`: when the API verification fails, `lanTargets` is non-empty and a stored add-on user exists in `storageManager.ListUsers()`, log `Cloud unreachable, serving LAN streams until it returns` at WARN and continue with that user and the stored camera registry; otherwise return the error as today.
  - Hand the server `cfg.UID` (or the cloud user id when the cloud answered), `lanStreamingEnabled(cfg)` and `lanTargets(cfg.Cameras)`; log at INFO how many cameras have LAN targets, never the values.
- [ ] **Step 4: Run the Go gate, expect PASS.**
- [ ] **Step 5: Commit** `feat(addon): LAN targets in the bridge config, start without the cloud` with the trailer, naming `FORK/cmd/addon/addon.go`, `05b9372`, `e6e51a2`.

---

### Task 9: Back to the LAN, and bridge-side discovery

The spec's "Back to the LAN after a fallback", plus the fork's discovery (`FORK/pkg/lan/discover.go`, fork commit `61fa5c3`) run in the background, so it never spends the 4 s budget.

**Files:**
- Create: `avent-webrtc-bridge/pkg/lan/discover.go`, `avent-webrtc-bridge/pkg/rtsp/lan_promote.go`
- Modify: `avent-webrtc-bridge/pkg/rtsp/server.go` (`Start` launches discovery; the stream lifecycle cancels promotion)
- Test: `avent-webrtc-bridge/pkg/lan/discover_test.go`, `avent-webrtc-bridge/pkg/rtsp/lan_promote_test.go`

**Interfaces:**
- Consumes: `lanCooldown`, `lanStreamer`, `lanEligible`, `handleBridgeError` (Task 7).
- Produces:
  - `type Device struct { ID, IP string; Protocol float64 }`; `func Discover(ctx context.Context, deviceID string) (*Device, error)` (UDP 6667 with SO_REUSEPORT through `golang.org/x/sys/unix`; protocol 3.5 or later required).
  - `func (s *RTSPServer) discoverMissingAddresses(ctx context.Context)`: for each LAN target with an empty IP, retry `Discover` every 60 s until found; the result is what `lanAddress` returns.
  - `var errLANAvailable = errors.New("LAN available again")`; `func (cs *CameraStream) schedulePromotion()`; `func (cs *CameraStream) cancelPromotion()`.

- [ ] **Step 1: Write the failing tests**
  - `TestParseAnnouncement`: an announcement built in the test, encrypted with the fork's UDP key seed (`yGAdlopoPVldABfn`, as `FORK/pkg/lan/discover.go` decrypts it), parses to the device id, IP and protocol 3.5.
  - `TestPromotionMovesCloudStreamToLAN`: a cloud stream whose cool-down ends; the promotion's fake LAN source starts; the probe is stopped, `handleBridgeError` got `errLANAvailable`, and the clients were closed.
  - `TestPromotionFailureRestartsCooldown`: the probe fails; the stream stays on the cloud, the clients are untouched, the cool-down is active again, and a new promotion is scheduled.
  - `TestPromotionCancelledWhenStreamStops`: the stream stops before the timer fires; the factory is never called.
  - `TestPromotionIsNotLoggedAsAnError`: `handleBridgeError(errLANAvailable)` does not log the "WebRTC error" ERROR line (check through the logger the package's tests already capture, or have `handleBridgeError` pick the level in a small function and test that).

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement**: after a cloud start of a stream whose only reason not to use the LAN is the cool-down, `schedulePromotion` arms `time.AfterFunc(cooldown.remaining(id))`. The callback runs a LAN source against a throwaway forwarder (`NewRTPForwarder()`, no clients) with the 4 s deadline and stops it; on success it calls `handleBridgeError(errLANAvailable)`, on failure it restarts the cool-down and reschedules. `stopStreamInternal` calls `cancelPromotion`.
- [ ] **Step 4: Run the Go gate, expect PASS.**
- [ ] **Step 5: Commit** `feat(rtsp): move cloud streams back to the LAN, discover missing addresses` with the trailer, naming `FORK/pkg/lan/discover.go` and `61fa5c3`.

---

### Task 10: Integration: bridge JSON fields, option and redaction

The pure-function side of the integration (fork commits `05b9372`, `bdeb206`, `6fc2509`), testable without Home Assistant.

**Files:**
- Modify: `custom_components/philips_avent/payload.py:81-141`, `custom_components/philips_avent/const.py`, `custom_components/philips_avent/redact.py:21-31`, `.gitleaks.toml:23`
- Test: `tests/test_philips_avent/test_bridge_config.py`, and the test file that already covers `redact_secrets`

**Interfaces:**
- Produces:
  - `const.py`: `CONF_LAN_STREAMING = "lan_streaming"`, `DEFAULT_LAN_STREAMING = True`.
  - `build_bridge_config(..., talkback: bool = False, uid: str = "", lan_streaming: bool = True, lan: dict[str, dict] | None = None) -> dict`; `lan` maps camera id to `{"local_key": str, "p2p_password": str, "lan_ip": str}`.
  - `build_cameras_payload(cameras: list, lan: dict[str, dict] | None = None) -> list`: each camera gains `local_key`, `p2p_password`, `lan_ip` (empty strings when unknown).
  - `redact.SECRET_KEYS` gains `"p2p_password"` and `"lan_ip"`.

- [ ] **Step 1: Write the failing tests**
  - Update `test_keys_match_the_go_struct` to today's keys plus `"uid"` and `"lan_streaming"`.
  - Update `test_cameras_use_the_shared_payload_builder` to expect the three empty fields.
  - `test_cameras_carry_lan_fields`: with `uid="u1"` and `lan={"abc123": {"local_key": "lk", "p2p_password": "pw", "lan_ip": "192.168.1.9"}}` the camera carries those values; a camera missing from `lan` carries three empty strings.
  - `test_lan_streaming_defaults_on` and `test_lan_streaming_written_when_off`.
  - `test_unchanged_inputs_give_identical_json`: two calls with equal inputs give equal `json.dumps(..., indent=2)` strings.
  - `test_redacts_lan_fields`: `redact_secrets({"cameras": [{"p2p_password": "pw", "lan_ip": "192.168.1.9", "local_key": "lk"}]})` redacts all three values.

- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement**; add `p2p_password|p2ppassword|lan_ip` to the key list of the `aventproxy-credential-assignment` rule in `.gitleaks.toml`.
- [ ] **Step 4: Run the Python gate, expect PASS**; if `gitleaks` is installed, `gitleaks detect --no-git --source . --config .gitleaks.toml` reports nothing new.
- [ ] **Step 5: Commit** `feat(integration): LAN fields and option in the bridge config` with the trailer.

---

### Task 11: Integration wiring

**Files:**
- Modify: `custom_components/philips_avent/__init__.py:56-88,223-247`, `custom_components/philips_avent/lan.py`, `custom_components/philips_avent/coordinator.py:85-96`, `custom_components/philips_avent/config_flow.py:410-430`, `custom_components/philips_avent/strings.json`, `custom_components/philips_avent/translations/en.json`
- Test: `tests/test_philips_avent/test_lan.py`, `tests/test_philips_avent/test_config_flow.py`, `tests/test_philips_avent/test_bridge_config.py`

**Interfaces:**
- Consumes: `build_bridge_config(..., uid, lan_streaming, lan)` (Task 10).
- Produces:
  - `TuyaLANClient.ip -> str | None` (property over `_ip`) and an `on_address_change: Callable[[str | None], None] | None` constructor argument, called whenever `_ip` changes value.
  - `PhilipsAventCoordinator.lan_ip -> str | None`; `start_lan(on_address_change=...)`.
  - `__init__.py`: `async def _lan_credentials(api, coordinators: dict) -> dict[str, dict]`: `local_key` from the coordinator's latest `device_info["localKey"]` (the fresh cloud value wins over a stored one), `p2p_password` from `(await api.get_rtc_config(cam_id))["password"]` (a failed call logs a warning and leaves it empty, so that camera uses the cloud), `lan_ip` from `coordinator.lan_ip`.
  - `hass.data[DOMAIN][entry_id]["cameras"]` keeps the camera list, so an address change can rewrite the bridge JSON.

- [ ] **Step 1: Check the P2P password is stable before writing code.** With the maintainer's credentials (`tools/_credentials.py`), call `smartlife.m.rtc.config.get` twice for one camera and compare `password` without printing it. Record "stable" or "changes per call" in the commit message. If it changes, stop and switch `_lan_credentials` to keep the stored value and refresh it only when `local_key` changes, persisted in `entry.data["cameras"]` (spec, Integration section), and adjust the tests below.

- [ ] **Step 2: Write the failing tests**
  - `test_lan_client_reports_address_changes` (`test_lan.py`): discovery returning `"192.168.1.9"` calls `on_address_change("192.168.1.9")` once; reconnecting at the same address does not call it again; losing the address calls it with `None`.
  - Options flow (`test_config_flow.py`): the schema contains `lan_streaming` defaulting to `True`, and saving `False` stores it.
  - `test_lan_credentials_prefer_fresh_key` (`test_bridge_config.py`, stub api and coordinator objects): a stored `localKey` "old" and a device_info `localKey` "new" give `local_key == "new"`; `get_rtc_config` raising gives `p2p_password == ""`.

- [ ] **Step 3: Run, expect FAIL.**
- [ ] **Step 4: Implement**; `_write_bridge_config` passes `uid=entry.data.get(CONF_UID, "")`, `lan_streaming=entry.options.get(CONF_LAN_STREAMING, DEFAULT_LAN_STREAMING)` and `lan=await _lan_credentials(...)`; the coordinator's address callback schedules `_write_bridge_config` on the event loop. Option label "LAN streaming", description "Stream over the local network when the monitor allows it. Turn it off if your stream stopped working after updating."
- [ ] **Step 5: Run the Python gate, expect PASS.**
- [ ] **Step 6: Commit** `feat(integration): send LAN credentials and address to the bridge` with the trailer.

---

### Task 12: Docs, changelog, credit

**Files:**
- Modify: `WHITEPAPER.md` (new section "Local streaming (LAN)"), `README.md`, the dev add-on card's changelog under `aventproxy-bridge-addon-dev/`, `docs/superpowers/specs/2026-11-02-lan-streaming-design.md` (Status: Implemented)

- [ ] **Step 1: WHITEPAPER**: summarise the local path (inputs, 6699 signalling, the ASCII IV and placeholder STUN quirks, hand-rolled ICE, KCP media, framing, the boot-time internet requirement), credit Leonard Pitzu by name, and link `https://github.com/leonardpitzu/aventproxy/blob/main/PROTOCOL.md` (sections 14–16, 20–21).
- [ ] **Step 2: README**: LAN-first streaming; what still uses the cloud (talkback, `/sd`, HEVC, a monitor that has not reached the internet since it booted); the "LAN streaming" option; credit.
- [ ] **Step 3: Dev changelog entry**, to reuse in the rc and release notes:

  > Streams now go over your local network when the monitor allows it, and fall back to the cloud when it does not. This is Leonard Pitzu's work (#51), ported and hardened. It has only been tested on an SCD973/26. If your stream stopped working after this update, turn off **LAN streaming** in the integration options and open an issue with your monitor model and the bridge log.

- [ ] **Step 4: Run both gates, expect PASS.**
- [ ] **Step 5: Commit** `docs: LAN streaming, credit to Leonard Pitzu (#51)` with the trailer.

---

### Task 13: Hand-off to the maintainer (no code)

- [ ] **Step 1:** Run the whole-branch review and both gates.
- [ ] **Step 2:** Ask the maintainer for the go to push `feat/lan-streaming` and open a PR linked to #51; the PR body follows the maintainer's external-communication style (humble, no AI attribution).
- [ ] **Step 3:** After the merge, and only on the maintainer's go, run the release workflow with type `dev`.
- [ ] **Step 4:** Give the maintainer the hardware checklist from the spec's Testing section (seven checks on the SCD973 with the dev card), and wait for the results before proposing the `rc`.
- [ ] **Step 5:** Draft the #51 comment thanking Leonard Pitzu and saying what was ported; post it only on the maintainer's go.
