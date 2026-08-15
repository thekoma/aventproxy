"""Tests for the credential loader used by the development scripts.

These scripts used to carry a real account's sid/ecode/localKey as module
constants, which put live credentials in the public history (see SECURITY.md).
The loader exists so a script can never again hold a working credential; these
tests pin the behaviour that makes that true — no defaults, no silent empties.
"""

import json

import pytest
from _credentials import MissingCredentials, load_credentials, mask


class TestMask:
    """These scripts print connection details to the console; a derived MQTT
    password or a local key must not be among them."""

    def test_keeps_a_short_recognisable_prefix(self):
        assert mask("abcdefghij") == "abcd******"

    def test_hides_everything_after_the_prefix(self):
        masked = mask("supersecretvalue")
        assert "secret" not in masked
        assert len(masked) == len("supersecretvalue")

    def test_short_value_is_fully_hidden(self):
        assert mask("abc") == "***"

    def test_value_exactly_the_prefix_length_is_fully_hidden(self):
        assert mask("abcd") == "****"

    def test_prefix_length_is_adjustable(self):
        assert mask("abcdefghij", keep=2) == "ab********"

    def test_empty_value_stays_empty(self):
        assert mask("") == ""


class TestEnvironment:
    def test_reads_values_from_the_environment(self):
        env = {"AVENT_SID": "sid-value", "AVENT_ECODE": "ecode-value"}
        assert load_credentials("sid", "ecode", env=env) == {
            "sid": "sid-value",
            "ecode": "ecode-value",
        }

    def test_multiword_name_maps_to_upper_snake_env_var(self):
        env = {"AVENT_LOCAL_KEY": "key-value"}
        assert load_credentials("local_key", env=env) == {"local_key": "key-value"}

    def test_unrequested_names_are_not_returned(self):
        env = {"AVENT_SID": "sid-value", "AVENT_ECODE": "ecode-value"}
        assert load_credentials("sid", env=env) == {"sid": "sid-value"}


class TestFileFallback:
    def test_falls_back_to_the_credentials_file(self, tmp_path):
        path = tmp_path / "credentials.json"
        path.write_text(json.dumps({"sid": "from-file", "ecode": "also-from-file"}))
        assert load_credentials("sid", "ecode", env={}, path=path) == {
            "sid": "from-file",
            "ecode": "also-from-file",
        }

    def test_environment_wins_over_the_file(self, tmp_path):
        path = tmp_path / "credentials.json"
        path.write_text(json.dumps({"sid": "from-file"}))
        env = {"AVENT_SID": "from-env"}
        assert load_credentials("sid", env=env, path=path) == {"sid": "from-env"}

    def test_absent_file_is_fine_when_the_environment_is_complete(self, tmp_path):
        env = {"AVENT_SID": "from-env"}
        missing = tmp_path / "nope.json"
        assert load_credentials("sid", env=env, path=missing) == {"sid": "from-env"}

    def test_unreadable_file_reports_the_path_instead_of_a_json_traceback(self, tmp_path):
        path = tmp_path / "credentials.json"
        path.write_text("{not json")
        with pytest.raises(MissingCredentials) as excinfo:
            load_credentials("sid", env={}, path=path)
        assert str(path) in str(excinfo.value)


class TestMissingValues:
    def test_missing_value_raises_naming_the_environment_variable(self):
        with pytest.raises(MissingCredentials) as excinfo:
            load_credentials("sid", "ecode", env={"AVENT_SID": "only-this-one"})
        message = str(excinfo.value)
        assert "AVENT_ECODE" in message
        assert "AVENT_SID" not in message

    def test_blank_value_counts_as_missing(self):
        # An empty string would otherwise sail through and fail later as an
        # opaque MQTT auth error.
        with pytest.raises(MissingCredentials):
            load_credentials("sid", env={"AVENT_SID": "   "})

    def test_message_explains_both_ways_to_supply_credentials(self):
        with pytest.raises(MissingCredentials) as excinfo:
            load_credentials("sid", env={})
        message = str(excinfo.value)
        assert "AVENT_SID" in message
        assert "credentials.json" in message

    def test_no_default_is_ever_substituted(self):
        # Guards against someone reintroducing a convenience fallback constant.
        with pytest.raises(MissingCredentials):
            load_credentials("sid", "ecode", "partner", "uid", env={})
