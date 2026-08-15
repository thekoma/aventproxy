"""Secret redaction shared by the debug log and the diagnostics dump.

docs/reporting-issues.md tells users their password, session token, email and
device keys are stripped before anything leaves their machine, and the issue
template repeats it while asking for the debug log. That promise has to hold
for both outputs, so both go through this one function.

Tuya discovery responses are the hard case: ``localKey`` — which is enough to
control the camera over the LAN on its own — sits inside lists of device dicts,
and household coordinates ride along in the home objects. Redaction therefore
walks lists as well as dicts.

No Home Assistant or aiohttp imports here, so the unit tests can load it
directly.
"""
from __future__ import annotations

REDACTED = "**REDACTED**"

# Lower-case; incoming keys are lower-cased before comparison.
SECRET_KEYS = frozenset({
    # Session and account
    "sid", "ecode", "uid", "partner", "partneridentity", "partner_identity",
    "password", "passwd", "email", "username", "mobile", "phone",
    "token", "access_token", "refresh_token", "sessionid", "session_id",
    # Device
    "localkey", "local_key", "psk", "secret", "devkey", "dev_key",
    # Location
    "lat", "lon", "lng", "latitude", "longitude",
})


def redact_secrets(value):
    """Return ``value`` with every secret-looking key replaced by a marker.

    Walks dicts, lists and tuples; anything else is returned as-is. The input
    is never mutated. Sequences come back as lists.
    """
    if isinstance(value, dict):
        return {
            key: REDACTED if str(key).lower() in SECRET_KEYS else redact_secrets(item)
            for key, item in value.items()
        }
    if isinstance(value, (list, tuple)):
        return [redact_secrets(item) for item in value]
    return value
