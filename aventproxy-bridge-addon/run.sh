#!/usr/bin/env bash
set -e

# Try HA integration config first (auto-configured), then add-on options
BRIDGE_CONFIG="/config/philips_avent_bridge.json"
ADDON_CONFIG="/data/options.json"

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
# modify tuya-ipc-terminal to accept multiple cameras.
CAMERA_ID=$(jq -r '.cameras[0].camera_id' "$CONFIG_PATH")
CAMERA_NAME=$(jq -r '.cameras[0].camera_name // "camera"' "$CONFIG_PATH")

echo "=============================="
echo "Philips Avent WebRTC Bridge"
echo "=============================="
echo "Camera: $CAMERA_NAME ($CAMERA_ID)"
echo "RTSP:   rtsp://localhost:8554/$CAMERA_NAME"
echo "=============================="

exec tuya-ipc-terminal direct \
    --signing-key "$SIGNING_KEY" \
    --sid "$SID" \
    --ecode "$ECODE" \
    --partner "$PARTNER" \
    --app-key "$APP_KEY" \
    --device-id "$DEVICE_ID" \
    --package "$PACKAGE_NAME" \
    --camera-id "$CAMERA_ID" \
    --camera-name "$CAMERA_NAME" \
    --port 8554
