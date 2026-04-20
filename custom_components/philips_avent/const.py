"""Constants for the Philips Avent Baby Monitor integration."""

DOMAIN = "philips_avent"

# Tuya Mobile SDK credentials (static per APK version)
# These are functional identifiers, equivalent to OAuth client_id/secret
# in other HA integrations. Same for all users of the Philips Avent app.
TUYA_SIGNING_KEY = (
    "com.philips.ph.babymonitorplus"
    "_D2:D6:95:A1:1D:1B:84:F9:25:A9:45:6E:27:F4:45:E9:FD:87:C3:74"
    ":63:AA:8A:34:32:A6:6A:23:3B:0F:D5:0F"
    "_8n459nxk9g98gqgcwrpk3csv97uuwajm"
    "_a3nfht4ufwfw9cmkspaftv4x89cx58qx"
)
TUYA_APP_KEY = "wx3at9qprkhskvkcsyhm"
TUYA_PACKAGE_NAME = "com.philips.ph.babymonitorplus"
TUYA_CH_KEY = "071d81fa"
TUYA_API_URL = "https://a1.tuyaeu.com/api.json"
TUYA_MQTT_HOST = "m1.tuyaeu.com"
TUYA_MQTT_PORT = 8883

# DPS codes
DPS_NIGHT_LIGHT = "138"
DPS_BRIGHTNESS = "158"
DPS_LIGHT_COLOR = "204"
DPS_LIGHT_TIMER = "240"
DPS_LIGHT_TIMER_SWITCH = "241"
DPS_TEMPERATURE = "207"
DPS_TEMPERATURE_F = "208"
DPS_MOTION_SWITCH = "134"
DPS_MOTION_SENSITIVITY = "106"
DPS_SOUND_SWITCH = "139"
DPS_SOUND_SENSITIVITY = "140"
DPS_LULLABY_CONTROL = "201"
DPS_LULLABY_VOLUME = "209"
DPS_LULLABY_MODE = "203"
DPS_LULLABY_STATE = "246"
DPS_LULLABY_TIMER_SWITCH = "243"
DPS_LULLABY_TIMER = "244"
DPS_PRIVACY_MODE = "237"
DPS_POWER_STATUS = "205"
DPS_FLIP = "102"
DPS_APP_TALKING = "253"

CONF_SID = "sid"
CONF_ECODE = "ecode"
CONF_PARTNER = "partner_identity"
CONF_UID = "uid"
CONF_CAMERA_ID = "camera_id"
CONF_CAMERA_NAME = "camera_name"
