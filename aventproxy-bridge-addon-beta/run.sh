#!/usr/bin/env bash
set -e

BRIDGE_CONFIG_GLOB="/config/philips_avent_bridge_*.json"
BRIDGE_CONFIG_LEGACY="/config/philips_avent_bridge.json"
ADDON_CONFIG="/data/options.json"

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
        echo "WARNING: $count bridge config files found in /config, using the newest one:"
        ls -1t $BRIDGE_CONFIG_GLOB 2>/dev/null | sed 's/^/  /'
        echo "The integration deletes the others on its next start. If they stay, remove the ones"
        echo "that do not belong to your current integration entry by hand."
    fi
}

if [ "${WAIT_FOR_CONFIG:-false}" = "true" ]; then
    echo "Waiting for bridge config from HA integration..."
    while ! find_bridge_config >/dev/null 2>&1 && [ ! -f "$ADDON_CONFIG" ]; do
        sleep 5
    done
    echo "Config found!"
fi

warn_on_extra_configs

FOUND_CONFIG=$(find_bridge_config 2>/dev/null)
if [ -n "$FOUND_CONFIG" ]; then
    echo "Using bridge config from HA integration: $FOUND_CONFIG"
    CONFIG_PATH="$FOUND_CONFIG"
elif [ -f "$ADDON_CONFIG" ]; then
    echo "Using add-on options config"
    CONFIG_PATH="$ADDON_CONFIG"
else
    echo "ERROR: no bridge config found"
    exit 1
fi

echo "=============================="
echo "Philips Avent WebRTC Bridge"
echo "Config: $CONFIG_PATH"
echo "=============================="

# Supervise the bridge here rather than exiting the container when the config
# changes. The old version killed this script and relied on the supervisor to
# bring the add-on back; after two Home Assistant restarts in quick succession it
# did not, and the add-on sat stopped with no video and nothing in the
# integration log to explain it (issue #73).
BRIDGE_PID=""

shutdown() {
    echo "Stopping bridge..."
    if [ -n "$BRIDGE_PID" ]; then
        kill "$BRIDGE_PID" 2>/dev/null
        wait "$BRIDGE_PID" 2>/dev/null
    fi
    exit 0
}
trap shutdown TERM INT

while true; do
    CONFIG_HASH=$(md5sum "$CONFIG_PATH" 2>/dev/null | cut -d' ' -f1)

    avent-webrtc-bridge addon --config "$CONFIG_PATH" &
    BRIDGE_PID=$!
    echo "Bridge started (pid $BRIDGE_PID)"

    RESTART_REASON=""
    while kill -0 "$BRIDGE_PID" 2>/dev/null; do
        sleep 10
        NEW_HASH=$(md5sum "$CONFIG_PATH" 2>/dev/null | cut -d' ' -f1)
        if [ -n "$NEW_HASH" ] && [ "$NEW_HASH" != "$CONFIG_HASH" ]; then
            echo "Config changed, restarting bridge..."
            RESTART_REASON="config"
            kill "$BRIDGE_PID" 2>/dev/null
            break
        fi
    done

    wait "$BRIDGE_PID" 2>/dev/null
    BRIDGE_EXIT=$?
    BRIDGE_PID=""

    if [ "$RESTART_REASON" != "config" ]; then
        echo "Bridge exited with status $BRIDGE_EXIT, restarting in 5s..."
        sleep 5
    fi
done
