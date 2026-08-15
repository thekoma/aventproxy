"""Tests for writing the bridge config file to disk.

The file holds a live Tuya session: sid, ecode, partner and device id. It
lands in /config, which Samba, File Editor and Terminal all mount, and which
every backup copies. Path.write_text left it at the default umask — 0644 — so
these tests pin the mode, not just the contents.
"""

import json
import os
import stat

from payload import write_bridge_config_file


def _mode(path):
    return stat.S_IMODE(path.stat().st_mode)


class TestContents:
    def test_writes_the_config_as_indented_json(self, tmp_path):
        path = tmp_path / "philips_avent_bridge_abc.json"
        config = {"sid": "s", "cameras": [{"camera_id": "a"}]}
        write_bridge_config_file(path, config)
        assert json.loads(path.read_text()) == config

    def test_overwrites_rather_than_appends(self, tmp_path):
        path = tmp_path / "bridge.json"
        write_bridge_config_file(path, {"sid": "first", "padding": "x" * 200})
        write_bridge_config_file(path, {"sid": "second"})
        assert json.loads(path.read_text()) == {"sid": "second"}


class TestPermissions:
    def test_new_file_is_owner_only(self, tmp_path):
        path = tmp_path / "bridge.json"
        write_bridge_config_file(path, {"sid": "s"})
        assert _mode(path) == 0o600

    def test_new_file_is_owner_only_under_a_permissive_umask(self, tmp_path):
        # The HA process umask is not ours to choose; 0644 came from exactly this.
        path = tmp_path / "bridge.json"
        previous = os.umask(0o000)
        try:
            write_bridge_config_file(path, {"sid": "s"})
        finally:
            os.umask(previous)
        assert _mode(path) == 0o600

    def test_existing_world_readable_file_is_tightened(self, tmp_path):
        # The upgrade path: everyone already has a 0644 file on disk.
        path = tmp_path / "bridge.json"
        path.write_text("{}")
        path.chmod(0o644)
        write_bridge_config_file(path, {"sid": "s"})
        assert _mode(path) == 0o600

    def test_a_directory_in_the_way_raises_instead_of_silently_skipping(self, tmp_path):
        # A failed write must not leave the caller believing the add-on got a
        # fresh session; __init__ logs success right after this call.
        path = tmp_path / "bridge.json"
        path.mkdir()
        try:
            write_bridge_config_file(path, {"sid": "s"})
        except OSError:
            return
        raise AssertionError("writing over a directory should raise")

    # Note: write_bridge_config_file tightens the mode on the open descriptor
    # before writing, so a pre-existing 0644 file never holds the new session
    # while still world-readable. That ordering is not covered by a test —
    # io.FileIO.write goes straight to the C write(), so it cannot be observed
    # from Python without a thread race. The end state above is what's pinned.
