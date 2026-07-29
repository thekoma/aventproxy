"""Tuya data-center routing for the login flow.

A Tuya account lives in exactly one data center, chosen when the account is
created. The session id (SID) issued by one data center is not valid in any
other: logging in against the wrong host is what produced the
`USER_SESSION_INVALID` failures reported in issues #44 and #58 by every user
outside the Central Europe region, while EU accounts worked fine.

Nothing here imports Home Assistant, so the routing logic is unit-testable
without an HA runtime (see `tests/test_philips_avent/test_region.py`).
"""
from __future__ import annotations

# Tuya data-center API hosts. Keys are the short region names Tuya uses for
# its endpoints (a1.tuya<region>.com) and are also what we persist in the
# config entry, so they must stay stable.
DATA_CENTER_HOSTS: dict[str, str] = {
    "eu": "a1.tuyaeu.com",
    "us": "a1.tuyaus.com",
    "in": "a1.tuyain.com",
    "cn": "a1.tuyacn.com",
}

DEFAULT_DATA_CENTER = "eu"

# Order in which unknown accounts are probed: EU first (where these monitors
# are mostly sold), then the Americas, then India and China.
PROBE_ORDER: tuple[str, ...] = ("eu", "us", "in", "cn")

# Phone calling code sent as `countryCode` when we cannot derive one from the
# Home Assistant country. One per data center so a probe always carries a
# plausible value.
FALLBACK_CALLING_CODE: dict[str, str] = {
    "eu": "39",
    "us": "1",
    "in": "91",
    "cn": "86",
}

# ISO 3166-1 alpha-2 -> (phone calling code, data center).
#
# The calling code is what Tuya expects in `countryCode`; the data center is
# where the account lives. Tuya keeps Europe, Africa and the Middle East in
# Central Europe, the Americas in Western America, and runs separate data
# centers for India and China. Asia-Pacific assignments are the least certain
# part of the table; when one is wrong the login probe falls back to the other
# data centers, so only the calling code has to be exact.
#
# Generated from the public country dataset at
# https://github.com/mledoze/countries (cca2 + idd fields), restricted to the
# country codes Home Assistant's country selector accepts.
COUNTRY_ROUTING: dict[str, tuple[str, str]] = {
    "AD": ("376", "eu"), "AE": ("971", "eu"), "AF": ("93", "in"), "AG": ("1268", "us"),
    "AI": ("1264", "us"), "AL": ("355", "eu"), "AM": ("374", "eu"), "AO": ("244", "eu"),
    "AR": ("54", "us"), "AS": ("1684", "us"), "AT": ("43", "eu"), "AU": ("61", "us"),
    "AW": ("297", "us"), "AX": ("358", "eu"), "AZ": ("994", "eu"), "BA": ("387", "eu"),
    "BB": ("1246", "us"), "BD": ("880", "in"), "BE": ("32", "eu"), "BF": ("226", "eu"),
    "BG": ("359", "eu"), "BH": ("973", "eu"), "BI": ("257", "eu"), "BJ": ("229", "eu"),
    "BL": ("590", "us"), "BM": ("1441", "us"), "BN": ("673", "us"), "BO": ("591", "us"),
    "BQ": ("599", "us"), "BR": ("55", "us"), "BS": ("1242", "us"), "BT": ("975", "in"),
    "BV": ("47", "eu"), "BW": ("267", "eu"), "BY": ("375", "eu"), "BZ": ("501", "us"),
    "CA": ("1", "us"), "CC": ("61", "us"), "CD": ("243", "eu"), "CF": ("236", "eu"),
    "CG": ("242", "eu"), "CH": ("41", "eu"), "CI": ("225", "eu"), "CK": ("682", "us"),
    "CL": ("56", "us"), "CM": ("237", "eu"), "CN": ("86", "cn"), "CO": ("57", "us"),
    "CR": ("506", "us"), "CU": ("53", "us"), "CV": ("238", "eu"), "CW": ("599", "us"),
    "CX": ("61", "us"), "CY": ("357", "eu"), "CZ": ("420", "eu"), "DE": ("49", "eu"),
    "DJ": ("253", "eu"), "DK": ("45", "eu"), "DM": ("1767", "us"), "DO": ("1", "us"),
    "DZ": ("213", "eu"), "EC": ("593", "us"), "EE": ("372", "eu"), "EG": ("20", "eu"),
    "EH": ("212", "eu"), "ER": ("291", "eu"), "ES": ("34", "eu"), "ET": ("251", "eu"),
    "FI": ("358", "eu"), "FJ": ("679", "us"), "FK": ("500", "us"), "FM": ("691", "us"),
    "FO": ("298", "eu"), "FR": ("33", "eu"), "GA": ("241", "eu"), "GB": ("44", "eu"),
    "GD": ("1473", "us"), "GE": ("995", "eu"), "GF": ("594", "us"), "GG": ("44", "eu"),
    "GH": ("233", "eu"), "GI": ("350", "eu"), "GL": ("299", "us"), "GM": ("220", "eu"),
    "GN": ("224", "eu"), "GP": ("590", "us"), "GQ": ("240", "eu"), "GR": ("30", "eu"),
    "GS": ("500", "eu"), "GT": ("502", "us"), "GU": ("1671", "us"), "GW": ("245", "eu"),
    "GY": ("592", "us"), "HK": ("852", "cn"), "HN": ("504", "us"), "HR": ("385", "eu"),
    "HT": ("509", "us"), "HU": ("36", "eu"), "ID": ("62", "us"), "IE": ("353", "eu"),
    "IL": ("972", "eu"), "IM": ("44", "eu"), "IN": ("91", "in"), "IO": ("246", "eu"),
    "IQ": ("964", "eu"), "IR": ("98", "eu"), "IS": ("354", "eu"), "IT": ("39", "eu"),
    "JE": ("44", "eu"), "JM": ("1876", "us"), "JO": ("962", "eu"), "JP": ("81", "us"),
    "KE": ("254", "eu"), "KG": ("996", "eu"), "KH": ("855", "us"), "KI": ("686", "us"),
    "KM": ("269", "eu"), "KN": ("1869", "us"), "KP": ("850", "us"), "KR": ("82", "us"),
    "KW": ("965", "eu"), "KY": ("1345", "us"), "KZ": ("7", "eu"), "LA": ("856", "us"),
    "LB": ("961", "eu"), "LC": ("1758", "us"), "LI": ("423", "eu"), "LK": ("94", "in"),
    "LR": ("231", "eu"), "LS": ("266", "eu"), "LT": ("370", "eu"), "LU": ("352", "eu"),
    "LV": ("371", "eu"), "LY": ("218", "eu"), "MA": ("212", "eu"), "MC": ("377", "eu"),
    "MD": ("373", "eu"), "ME": ("382", "eu"), "MF": ("590", "us"), "MG": ("261", "eu"),
    "MH": ("692", "us"), "MK": ("389", "eu"), "ML": ("223", "eu"), "MM": ("95", "us"),
    "MN": ("976", "us"), "MO": ("853", "cn"), "MP": ("1670", "us"), "MQ": ("596", "us"),
    "MR": ("222", "eu"), "MS": ("1664", "us"), "MT": ("356", "eu"), "MU": ("230", "eu"),
    "MV": ("960", "in"), "MW": ("265", "eu"), "MX": ("52", "us"), "MY": ("60", "us"),
    "MZ": ("258", "eu"), "NA": ("264", "eu"), "NC": ("687", "us"), "NE": ("227", "eu"),
    "NF": ("672", "us"), "NG": ("234", "eu"), "NI": ("505", "us"), "NL": ("31", "eu"),
    "NO": ("47", "eu"), "NP": ("977", "in"), "NR": ("674", "us"), "NU": ("683", "us"),
    "NZ": ("64", "us"), "OM": ("968", "eu"), "PA": ("507", "us"), "PE": ("51", "us"),
    "PF": ("689", "us"), "PG": ("675", "us"), "PH": ("63", "us"), "PK": ("92", "in"),
    "PL": ("48", "eu"), "PM": ("508", "us"), "PN": ("64", "us"), "PR": ("1", "us"),
    "PS": ("970", "eu"), "PT": ("351", "eu"), "PW": ("680", "us"), "PY": ("595", "us"),
    "QA": ("974", "eu"), "RE": ("262", "eu"), "RO": ("40", "eu"), "RS": ("381", "eu"),
    "RU": ("7", "eu"), "RW": ("250", "eu"), "SA": ("966", "eu"), "SB": ("677", "us"),
    "SC": ("248", "eu"), "SD": ("249", "eu"), "SE": ("46", "eu"), "SG": ("65", "us"),
    "SH": ("290", "eu"), "SI": ("386", "eu"), "SJ": ("47", "eu"), "SK": ("421", "eu"),
    "SL": ("232", "eu"), "SM": ("378", "eu"), "SN": ("221", "eu"), "SO": ("252", "eu"),
    "SR": ("597", "us"), "SS": ("211", "eu"), "ST": ("239", "eu"), "SV": ("503", "us"),
    "SX": ("1721", "us"), "SY": ("963", "eu"), "SZ": ("268", "eu"), "TC": ("1649", "us"),
    "TD": ("235", "eu"), "TF": ("262", "eu"), "TG": ("228", "eu"), "TH": ("66", "us"),
    "TJ": ("992", "eu"), "TK": ("690", "us"), "TL": ("670", "us"), "TM": ("993", "eu"),
    "TN": ("216", "eu"), "TO": ("676", "us"), "TR": ("90", "eu"), "TT": ("1868", "us"),
    "TV": ("688", "us"), "TW": ("886", "cn"), "TZ": ("255", "eu"), "UA": ("380", "eu"),
    "UG": ("256", "eu"), "UM": ("1", "us"), "US": ("1", "us"), "UY": ("598", "us"),
    "UZ": ("998", "eu"), "VA": ("39", "eu"), "VC": ("1784", "us"), "VE": ("58", "us"),
    "VG": ("1284", "us"), "VI": ("1340", "us"), "VN": ("84", "us"), "VU": ("678", "us"),
    "WF": ("681", "us"), "WS": ("685", "us"), "YE": ("967", "eu"), "YT": ("262", "eu"),
    "ZA": ("27", "eu"), "ZM": ("260", "eu"), "ZW": ("263", "eu"),
}


def is_wrong_data_center(code: str) -> bool:
    """True when a Tuya login error suggests the account lives elsewhere.

    Password and verification-code failures are never treated this way: they
    are answers from the data center that owns the account, and retrying them
    against the other hosts would only burn failed-login attempts.
    """
    upper = code.upper()
    if "PASSWD" in upper or "MFA" in upper or "CODE" in upper:
        return False
    return "NOT_EXIST" in upper or "SESSION" in upper or "REGION" in upper


def api_url(data_center: str) -> str:
    """Return the mobile SDK API URL for a data center name."""
    host = DATA_CENTER_HOSTS.get(data_center, DATA_CENTER_HOSTS[DEFAULT_DATA_CENTER])
    return f"https://{host}/api.json"


def api_host(data_center: str) -> str:
    """Return the bare API host for a data center name (used by the bridge)."""
    return DATA_CENTER_HOSTS.get(data_center, DATA_CENTER_HOSTS[DEFAULT_DATA_CENTER])


def api_url_for_host(host: str | None) -> str:
    """Build the mobile SDK API URL for a bare API host."""
    if not host:
        return api_url(DEFAULT_DATA_CENTER)
    return f"https://{host}/api.json"


def normalize_api_host(value: str | None) -> str | None:
    """Reduce a `domain.mobileApiUrl` value to a bare host.

    Tuya returns these either as a bare host (`a1.tuyaeu.com`) or as a full
    URL, so both forms are accepted. Returns None when there is nothing usable.
    """
    if not value:
        return None
    host = value.strip().removeprefix("https://").removeprefix("http://")
    host = host.split("/", 1)[0].strip()
    return host or None


def hosts_from_domain(domain: object) -> dict[str, str]:
    """Extract the per-account hosts from a login/user-info `domain` block.

    The login response carries the authoritative endpoints for the account's
    region (`mobileApiUrl`, `mobileMqttsUrl`, `regionCode`). Using them means
    the integration self-corrects even when the country table guessed the wrong
    data center, and the bridge inherits the same hosts.
    """
    if not isinstance(domain, dict):
        return {}

    hosts: dict[str, str] = {}
    api = normalize_api_host(domain.get("mobileApiUrl"))
    if api:
        hosts["api_host"] = api
    mqtt = normalize_api_host(domain.get("mobileMqttsUrl"))
    if mqtt:
        hosts["mqtt_host"] = mqtt
    region_code = (domain.get("regionCode") or "").strip()
    if region_code:
        hosts["region_code"] = region_code
    return hosts


def data_center_from_host(host: str) -> str:
    """Map an API host back to its data center name, defaulting to EU."""
    for name, known in DATA_CENTER_HOSTS.items():
        if known == host:
            return name
    return DEFAULT_DATA_CENTER


def login_candidates(country: str | None) -> list[tuple[str, str]]:
    """Return the (data_center, calling_code) pairs to try, best guess first.

    `country` is the Home Assistant country (ISO 3166-1 alpha-2) and may be
    None or unknown. The account's own calling code is kept across every
    candidate when we know it, because only the data center is in doubt.
    """
    routed = COUNTRY_ROUTING.get((country or "").upper())
    candidates: list[tuple[str, str]] = []

    if routed:
        calling_code, data_center = routed
        candidates.append((data_center, calling_code))
        for name in PROBE_ORDER:
            if name != data_center:
                candidates.append((name, calling_code))
    else:
        for name in PROBE_ORDER:
            candidates.append((name, FALLBACK_CALLING_CODE[name]))

    return candidates
