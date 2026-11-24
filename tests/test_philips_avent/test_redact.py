"""Tests for the shared secret-redaction helper.

Two things depend on this being right. The diagnostics dump promises users it
strips their secrets, and docs/reporting-issues.md makes the same promise about
the debug log before asking them to attach it to a public issue. Discovery
responses carry ``localKey`` — a direct LAN control credential — inside lists
of device dicts, which is exactly the shape a non-recursive redactor misses.
"""

from redact import REDACTED, SECRET_KEYS, redact_secrets


class TestFlatDicts:
    def test_local_key_is_redacted(self):
        assert redact_secrets({"localKey": "abc"}) == {"localKey": "**REDACTED**"}

    def test_snake_case_spelling_is_redacted_too(self):
        assert redact_secrets({"local_key": "abc"}) == {"local_key": "**REDACTED**"}

    def test_key_matching_ignores_case(self):
        assert redact_secrets({"LocalKey": "abc"}) == {"LocalKey": "**REDACTED**"}

    def test_harmless_keys_survive_untouched(self):
        payload = {"name": "Baby", "devId": "abc123", "category": "sp"}
        assert redact_secrets(payload) == payload

    def test_session_credentials_are_redacted(self):
        out = redact_secrets({"sid": "s", "ecode": "e", "partnerIdentity": "p", "uid": "u"})
        assert set(out.values()) == {"**REDACTED**"}

    def test_household_coordinates_are_redacted(self):
        # Tuya home objects carry the household's position; a debug log
        # attached to a public issue should not hand it out.
        out = redact_secrets({"gid": 123, "lat": "45.4", "lon": "9.1"})
        assert out == {"gid": 123, "lat": "**REDACTED**", "lon": "**REDACTED**"}

    def test_public_ip_is_redacted(self):
        # The cloud's device record carries the address it sees the camera
        # from, which is the household's public IP (issue #91).
        out = redact_secrets({"devId": "bf1", "ip": "203.0.113.7", "verSw": "1.1.12"})
        assert out == {"devId": "bf1", "ip": "**REDACTED**", "verSw": "1.1.12"}

    def test_mac_address_is_redacted(self):
        out = redact_secrets({"devId": "bf1", "mac": "00:00:5e:00:53:01", "verSw": "1.1.12"})
        assert out == {"devId": "bf1", "mac": "**REDACTED**", "verSw": "1.1.12"}


class TestNesting:
    def test_redacts_lan_fields(self):
        out = redact_secrets({"cameras": [{"p2p_password": "pw", "lan_ip": "192.168.1.9", "local_key": "lk"}]})
        assert out["cameras"][0] == {"p2p_password": REDACTED, "lan_ip": REDACTED, "local_key": REDACTED}

    def test_recurses_into_nested_dicts(self):
        out = redact_secrets({"device": {"inner": {"localKey": "abc"}}})
        assert out["device"]["inner"]["localKey"] == "**REDACTED**"

    def test_recurses_into_lists_of_dicts(self):
        # The gap that made discovery logging unsafe: cameras arrive as a list.
        payload = {"deviceList": [{"devId": "a", "localKey": "secret"}]}
        out = redact_secrets(payload)
        assert out["deviceList"][0] == {"devId": "a", "localKey": "**REDACTED**"}

    def test_redacts_a_bare_list_at_the_top_level(self):
        # discover_cameras logs the raw API result, which is a list, not a dict.
        out = redact_secrets([{"localKey": "secret"}, {"localKey": "other"}])
        assert out == [{"localKey": "**REDACTED**"}, {"localKey": "**REDACTED**"}]

    def test_recurses_through_lists_inside_lists(self):
        out = redact_secrets([[{"localKey": "secret"}]])
        assert out == [[{"localKey": "**REDACTED**"}]]

    def test_no_secret_value_survives_anywhere_in_the_output(self):
        payload = {"homes": [{"lat": "45.4", "rooms": [{"deviceList": [{"localKey": "leaked"}]}]}]}
        assert "leaked" not in repr(redact_secrets(payload))
        assert "45.4" not in repr(redact_secrets(payload))


class TestNonMappings:
    def test_scalars_pass_through(self):
        assert redact_secrets("hello") == "hello"
        assert redact_secrets(7) == 7
        assert redact_secrets(None) is None

    def test_tuples_are_traversed_and_returned_as_lists(self):
        assert redact_secrets(({"localKey": "abc"},)) == [{"localKey": "**REDACTED**"}]

    def test_input_is_not_mutated(self):
        payload = {"localKey": "abc"}
        redact_secrets(payload)
        assert payload == {"localKey": "abc"}


class TestSecretKeys:
    def test_secret_keys_are_declared_lower_case(self):
        # Matching lower-cases the incoming key, so an upper-case entry here
        # would silently never match.
        assert all(key == key.lower() for key in SECRET_KEYS)


class TestDiagnosticsShape:
    """What diagnostics.py hands the redactor: the config entry and the cloud device record.

    The fork found its diagnostics leaking `local_key` and `password` from the
    camera list, because its redactor did not walk lists (fork e0a67d6).
    """

    def test_no_lan_credential_survives_the_dump(self):
        entry = {
            "email": "SECRET-email",
            "password": "SECRET-account-password",
            "sid": "SECRET-sid",
            "cameras": [{
                "id": "bf1camera", "name": "Nursery",
                "localKey": "SECRET-lk1", "local_key": "SECRET-lk2",
                "password": "SECRET-pw1", "p2p_password": "SECRET-pw2", "lan_ip": "SECRET-ip",
            }],
        }
        device_info = {"devId": "bf1camera", "localKey": "SECRET-lk3", "ip": "SECRET-public-ip"}
        dump = repr({"config_entry": redact_secrets(entry), "device_info": redact_secrets(device_info)})
        assert "SECRET" not in dump
        assert "Nursery" in dump
        assert "bf1camera" in dump
