#!/usr/bin/env bash
set -e

# Try HA integration config first (auto-configured), then add-on options
BRIDGE_CONFIG="/config/philips_avent_bridge.json"
ADDON_CONFIG="/data/options.json"

if [ "${WAIT_FOR_CONFIG:-false}" = "true" ]; then
    echo "Waiting for bridge config from HA integration..."
    while [ ! -f "$BRIDGE_CONFIG" ] && [ ! -f "$ADDON_CONFIG" ]; do
        sleep 5
    done
    echo "Config found!"
fi

if [ -f "$BRIDGE_CONFIG" ]; then
    echo "Using bridge config from HA integration"
    CONFIG_PATH="$BRIDGE_CONFIG"
else
    echo "Using add-on options config"
    CONFIG_PATH="$ADDON_CONFIG"
fi

SIGNING_KEY=$(jq -r '.signing_key' "$CONFIG_PATH")
SID=$(jq -r '.sid' "$CONFIG_PATH")
ECODE=$(jq -r '.ecode' "$CONFIG_PATH")
PARTNER=$(jq -r '.partner' "$CONFIG_PATH")
APP_KEY=$(jq -r '.app_key' "$CONFIG_PATH")
DEVICE_ID=$(jq -r '.device_id' "$CONFIG_PATH")
PACKAGE_NAME=$(jq -r '.package_name' "$CONFIG_PATH")

NUM_CAMERAS=$(jq '.cameras | length' "$CONFIG_PATH")

if [ -z "$SIGNING_KEY" ] || [ "$SIGNING_KEY" = "null" ]; then
    echo "ERROR: signing_key not configured"
    echo "Configure the Philips Avent integration first, or set add-on options manually."
    exit 1
fi

if [ "$NUM_CAMERAS" = "0" ] || [ "$NUM_CAMERAS" = "null" ]; then
    echo "ERROR: no cameras configured"
    exit 1
fi

# For now, use the first camera. Multi-camera: would need multiple bridge instances or
# modify avent-webrtc-bridge to accept multiple cameras.
CAMERA_ID=$(jq -r '.cameras[0].camera_id' "$CONFIG_PATH")
CAMERA_NAME=$(jq -r '.cameras[0].camera_name // "camera"' "$CONFIG_PATH")

echo "=============================="
echo "Philips Avent WebRTC Bridge"
echo "=============================="
PORT=$(jq -r '.bridge_port // 18554' "$CONFIG_PATH")

echo "Camera: $CAMERA_NAME ($CAMERA_ID)"
echo "RTSP:   rtsp://localhost:$PORT/$CAMERA_NAME"
echo "=============================="

CONFIG_HASH=$(md5sum "$CONFIG_PATH" | cut -d' ' -f1)
(
    while true; do
        sleep 10
        NEW_HASH=$(md5sum "$CONFIG_PATH" 2>/dev/null | cut -d' ' -f1)
        if [ -n "$NEW_HASH" ] && [ "$NEW_HASH" != "$CONFIG_HASH" ]; then
            echo "Config changed, restarting bridge..."
            kill $$
            exit 0
        fi
    done
) &

exec avent-webrtc-bridge direct \
    --signing-key "$SIGNING_KEY" \
    --sid "$SID" \
    --ecode "$ECODE" \
    --partner "$PARTNER" \
    --app-key "$APP_KEY" \
    --device-id "$DEVICE_ID" \
    --package "$PACKAGE_NAME" \
    --camera-id "$CAMERA_ID" \
    --camera-name "$CAMERA_NAME" \
    --port "$PORT"
