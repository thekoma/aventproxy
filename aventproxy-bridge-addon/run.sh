#!/usr/bin/env bash
# No `set -e` on purpose: this script supervises a child that is allowed to fail,
# and a non-zero status from `wait` or `kill` must never end the add-on (issues
# #73 and #159). Every command whose failure matters is checked by hand.

# The defaults are the add-on's paths and timings. The environment overrides
# them only so that tests/test_addon_run_sh.py can drive the script in a temp dir.
CONFIG_DIR="${CONFIG_DIR:-/config}"
BRIDGE_CONFIG_GLOB="$CONFIG_DIR/philips_avent_bridge_*.json"
BRIDGE_CONFIG_LEGACY="$CONFIG_DIR/philips_avent_bridge.json"
ADDON_CONFIG="${ADDON_CONFIG:-/data/options.json}"
BRIDGE_BIN="${BRIDGE_BIN:-avent-webrtc-bridge}"
POLL_INTERVAL="${POLL_INTERVAL:-10}"       # seconds between config checks
RESTART_DELAY_MIN="${RESTART_DELAY_MIN:-5}"
RESTART_DELAY_MAX="${RESTART_DELAY_MAX:-300}"
STABLE_RUN="${STABLE_RUN:-60}"             # a run this long resets the back-off
STOP_GRACE_TICKS="${STOP_GRACE_TICKS:-32}" # times 0.25 s before the child gets SIGKILL

# Picks the most recently written config. The integration writes one file per
# config entry, and a leftover from a deleted entry used to win just by sorting
# first, which had the bridge streaming an old session and camera id (issue #52).
find_bridge_config() {
    newest=$(ls -1t $BRIDGE_CONFIG_GLOB 2>/dev/null | head -1)
    if [ -n "$newest" ] && [ -f "$newest" ]; then
        echo "$newest"
        return 0
    fi
    [ -f "$BRIDGE_CONFIG_LEGACY" ] && echo "$BRIDGE_CONFIG_LEGACY" && return 0
    return 1
}

warn_on_extra_configs() {
    count=$(ls -1 $BRIDGE_CONFIG_GLOB 2>/dev/null | wc -l | tr -d ' ')
    if [ "${count:-0}" -gt 1 ]; then
        echo "WARNING: $count bridge config files found in $CONFIG_DIR, using the newest one:"
        ls -1t $BRIDGE_CONFIG_GLOB 2>/dev/null | sed 's/^/  /'
        echo "The integration deletes the others on its next start. If they stay, remove the ones"
        echo "that do not belong to your current integration entry by hand."
    fi
}

# Where the RTSP server listens. Without the integration's "Password on the
# RTSP streams" option it has no authentication, so it stays on
# loopback unless the user opens it on purpose: Home Assistant shares the host
# network with the add-on and reaches it at rtsp://localhost:<port>. The add-on
# option rtsp_bind_address wins over the default; RTSP_BIND_ADDRESS (env) wins
# over both, for docker-compose setups that have no /data/options.json.
resolve_rtsp_bind() {
    if [ -n "${RTSP_BIND_ADDRESS:-}" ]; then
        echo "$RTSP_BIND_ADDRESS"
        return 0
    fi
    if [ -f "$ADDON_CONFIG" ]; then
        value=$(jq -r '.rtsp_bind_address // empty' "$ADDON_CONFIG" 2>/dev/null || true)
        if [ -n "$value" ]; then
            echo "$value"
            return 0
        fi
    fi
    echo "localhost"
}

# True when the add-on options carry the credentials themselves, i.e. the user
# filled them in by hand instead of using the integration. /data/options.json
# always exists under Supervisor, so its mere presence says nothing.
has_addon_credentials() {
    [ -f "$ADDON_CONFIG" ] && jq -e '(.signing_key // "") != "" and (.sid // "") != ""' "$ADDON_CONFIG" >/dev/null 2>&1
}

# Sets CONFIG_PATH to the newest integration config, else to the add-on options
# when they carry credentials. Returns 1 when there is neither.
resolve_config() {
    found=$(find_bridge_config 2>/dev/null || true)
    if [ -n "$found" ]; then
        CONFIG_PATH="$found"
        return 0
    fi
    if has_addon_credentials; then
        CONFIG_PATH="$ADDON_CONFIG"
        return 0
    fi
    CONFIG_PATH=""
    return 1
}

# A sleep that SIGTERM and SIGINT interrupt at once: bash runs a trap only after
# the foreground command ends, but `wait` returns as soon as a trapped signal
# arrives.
nap() {
    sleep "$1" &
    wait $! 2>/dev/null || true
}

BRIDGE_PID=""

# Ask the bridge to stop, and kill it if it ignores the request: a child that
# does not stop must not hold up a restart or Supervisor's stop.
stop_bridge() {
    kill "$BRIDGE_PID" 2>/dev/null || true
    ticks=0
    while kill -0 "$BRIDGE_PID" 2>/dev/null && [ "$ticks" -lt "$STOP_GRACE_TICKS" ]; do
        sleep 0.25
        ticks=$((ticks + 1))
    done
    if kill -0 "$BRIDGE_PID" 2>/dev/null; then
        echo "Bridge did not stop in time, killing it"
        kill -9 "$BRIDGE_PID" 2>/dev/null || true
    fi
}

# Supervisor stops the add-on with SIGTERM: pass it on, give the bridge a moment
# to close its sessions, then exit cleanly.
shutdown() {
    echo "Stopping bridge..."
    if [ -n "$BRIDGE_PID" ]; then
        stop_bridge
        wait "$BRIDGE_PID" 2>/dev/null || true
    fi
    exit 0
}
trap shutdown TERM INT

# Wait until there is something to run. It logs once, so a long wait does not
# flood the log. With WAIT_FOR_CONFIG off, the very first start fails fast.
wait_for_config() {
    announced=0
    while ! resolve_config; do
        if [ "${WAIT_FOR_CONFIG:-false}" != "true" ] && [ -z "$BRIDGE_EVER_STARTED" ]; then
            echo "ERROR: no bridge config found"
            exit 1
        fi
        if [ "$announced" -eq 0 ]; then
            echo "Waiting for bridge config from HA integration..."
            announced=1
        fi
        nap 5
    done
    if [ "$announced" -eq 1 ]; then
        echo "Config found!"
    fi
}

# Issue #72 experiment knobs. Only the Dev card defines these options (with
# speaker_off on): on the other cards they are absent from options.json, no
# flag is passed, and the bridge keeps its defaults (no speaker-off request,
# recvonly audio). A false must still reach the bridge, hence the null test
# rather than jq's // operator.
addon_option() {
    [ -f "$ADDON_CONFIG" ] || return 0
    jq -r --arg key "$1" 'if .[$key] == null then empty else .[$key] end' "$ADDON_CONFIG" 2>/dev/null || true
}

build_args() {
    BRIDGE_ARGS=(addon --config "$CONFIG_PATH" --bind "$RTSP_BIND")
    SPEAKER_OFF=$(addon_option speaker_off)
    if [ -n "$SPEAKER_OFF" ]; then
        BRIDGE_ARGS+=("--speaker-off=$SPEAKER_OFF")
    fi
    WEBRTC_AUDIO=$(addon_option webrtc_audio)
    if [ -n "$WEBRTC_AUDIO" ]; then
        BRIDGE_ARGS+=(--webrtc-audio "$WEBRTC_AUDIO")
    fi
    # Dev card only, like the two above: imm tries the app's own transport over
    # cloud signalling before the webrtc session. Absent elsewhere, so the bridge
    # keeps webrtc.
    CLOUD_TRANSPORT=$(addon_option cloud_transport)
    if [ -n "$CLOUD_TRANSPORT" ]; then
        BRIDGE_ARGS+=(--cloud-transport "$CLOUD_TRANSPORT")
    fi
    # Dev card only too: on_demand opens a two-way session only while a client
    # talks. Absent elsewhere, so talkback keeps the session-wide mode.
    TALKBACK_MODE=$(addon_option talkback_mode)
    if [ -n "$TALKBACK_MODE" ]; then
        BRIDGE_ARGS+=(--talkback-mode "$TALKBACK_MODE")
    fi
}

BRIDGE_EVER_STARTED=""
CONFIG_PATH=""
wait_for_config
warn_on_extra_configs

RTSP_BIND=$(resolve_rtsp_bind)
echo "=============================="
echo "Philips Avent WebRTC Bridge"
echo "RTSP bind: $RTSP_BIND"
echo "=============================="

case "$RTSP_BIND" in
    localhost|127.0.0.1|::1|"127.0.0.1,::1") ;;
    *)
        echo "WARNING: the RTSP server listens on '$RTSP_BIND'. Unless 'Password on the RTSP"
        echo "streams' is on in the integration, anyone who can reach this port can watch"
        echo "and hear the camera."
        ;;
esac

# Supervise the bridge here rather than exiting the container when the config
# changes or the bridge dies. The old version killed this script and relied on
# the supervisor to bring the add-on back; after two Home Assistant restarts in
# quick succession it did not, and the add-on sat stopped with no video and
# nothing in the integration log to explain it (issue #73). The bridge is
# restarted on any exit status, with a growing delay so that a permanent failure
# does not hit the Tuya API every few seconds. The config is resolved again on
# every start and watched while the bridge runs. Another integration file takes
# over only once the current one is gone, or when the bridge was running on the
# add-on options: with two config entries both files are valid, and following
# whichever was written last would restart the stream each time the other one
# is rewritten. A re-added integration (new entry id) removes the old file, so
# it is still picked up (issues #52 and #159).
restart_delay="$RESTART_DELAY_MIN"

while true; do
    wait_for_config
    BRIDGE_EVER_STARTED=1
    build_args
    echo "Using bridge config: $CONFIG_PATH"
    CONFIG_HASH=$(md5sum "$CONFIG_PATH" 2>/dev/null | cut -d' ' -f1)

    started_at=$SECONDS
    "$BRIDGE_BIN" "${BRIDGE_ARGS[@]}" &
    BRIDGE_PID=$!
    echo "Bridge started (pid $BRIDGE_PID)"

    RESTART_REASON=""
    waited=0
    while kill -0 "$BRIDGE_PID" 2>/dev/null; do
        nap 1
        waited=$((waited + 1))
        if [ "$waited" -lt "$POLL_INTERVAL" ]; then
            continue
        fi
        waited=0
        LATEST=$(find_bridge_config 2>/dev/null || true)
        if [ -n "$LATEST" ] && [ "$LATEST" != "$CONFIG_PATH" ] &&
            { [ ! -f "$CONFIG_PATH" ] || [ "$CONFIG_PATH" = "$ADDON_CONFIG" ]; }; then
            echo "Bridge config is now $LATEST, restarting bridge..."
            RESTART_REASON="config"
        else
            NEW_HASH=$(md5sum "$CONFIG_PATH" 2>/dev/null | cut -d' ' -f1)
            if [ -n "$NEW_HASH" ] && [ "$NEW_HASH" != "$CONFIG_HASH" ]; then
                echo "Config changed, restarting bridge..."
                RESTART_REASON="config"
            fi
        fi
        if [ "$RESTART_REASON" = "config" ]; then
            stop_bridge
            break
        fi
    done

    BRIDGE_EXIT=0
    wait "$BRIDGE_PID" 2>/dev/null || BRIDGE_EXIT=$?
    BRIDGE_PID=""

    if [ "$RESTART_REASON" = "config" ]; then
        restart_delay="$RESTART_DELAY_MIN"
        continue
    fi

    if [ $((SECONDS - started_at)) -ge "$STABLE_RUN" ]; then
        restart_delay="$RESTART_DELAY_MIN"
    fi
    echo "Bridge exited with status $BRIDGE_EXIT, restarting in ${restart_delay}s..."
    nap "$restart_delay"
    restart_delay=$((restart_delay * 2))
    if [ "$restart_delay" -gt "$RESTART_DELAY_MAX" ]; then
        restart_delay="$RESTART_DELAY_MAX"
    fi
done
