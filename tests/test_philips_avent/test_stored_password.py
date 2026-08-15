"""Tests for purging the account password from the config entry.

The config flow used to persist the plaintext password and nothing ever read
it back — reauth asks for it again. It sat in .storage/core.config_entries,
and in every backup, for no functional benefit. Not writing it any more only
helps new installs; existing entries need it actively removed, which is what
this helper drives.
"""

from payload import strip_stored_password


class TestEntriesCarryingAPassword:
    def test_returns_the_data_without_the_password(self):
        data = {"email": "a@b.c", "password": "hunter2", "sid": "s"}
        assert strip_stored_password(data) == {"email": "a@b.c", "sid": "s"}

    def test_every_other_field_survives(self):
        data = {"password": "hunter2", "cameras": [{"id": "a"}], "country": "IT"}
        assert strip_stored_password(data) == {"cameras": [{"id": "a"}], "country": "IT"}

    def test_the_original_is_not_mutated(self):
        # A half-applied update must not leave the in-memory entry inconsistent
        # with what was persisted.
        data = {"password": "hunter2", "sid": "s"}
        strip_stored_password(data)
        assert data == {"password": "hunter2", "sid": "s"}

    def test_an_empty_password_is_still_removed(self):
        # An empty string is a leftover key, not a credential worth keeping.
        assert strip_stored_password({"password": "", "sid": "s"}) == {"sid": "s"}


class TestEntriesWithoutAPassword:
    def test_returns_none_so_the_caller_can_skip_the_write(self):
        # Rewriting the entry on every startup would churn .storage for nothing.
        assert strip_stored_password({"email": "a@b.c", "sid": "s"}) is None

    def test_empty_data_returns_none(self):
        assert strip_stored_password({}) is None
