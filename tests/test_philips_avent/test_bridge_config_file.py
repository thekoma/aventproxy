"""Tests for writing the bridge config file to disk.

The file holds a live Tuya session: sid, ecode, partner and device id. It
lands in /config, which Samba, File Editor and Terminal all mount, and which
every backup copies. Path.write_text left it at the default umask — 0644 — so
these tests pin the mode, not just the contents.
"""

import json
import os
import stat
import time

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

    def test_a_failed_write_leaves_no_temporary_file(self, tmp_path):
        path = tmp_path / "bridge.json"
        path.mkdir()
        try:
            write_bridge_config_file(path, {"sid": "s"})
        except OSError:
            pass
        assert [p.name for p in tmp_path.iterdir()] == ["bridge.json"]

    # Note: write_bridge_config_file tightens the mode on the open descriptor
    # before writing, so the new session is never in a world-readable file.
    # That ordering is not covered by a test — io.FileIO.write goes straight to
    # the C write(), so it cannot be observed from Python without a thread race.
    # The end state above is what's pinned.


class TestAtomicReplace:
    """run.sh polls the md5 every 10 s and restarts the bridge on any change (#73)."""

    def test_a_reader_never_sees_a_truncated_file(self, tmp_path):
        # A reader that opened the file before the write keeps reading the whole
        # old content: the new file replaces it rather than being written into it.
        path = tmp_path / "philips_avent_bridge_abc.json"
        write_bridge_config_file(path, {"sid": "old", "padding": "x" * 200})
        with path.open() as reader:
            write_bridge_config_file(path, {"sid": "new"})
            assert json.loads(reader.read())["sid"] == "old"
        assert json.loads(path.read_text()) == {"sid": "new"}

    def test_only_the_config_is_left_in_the_directory(self, tmp_path):
        path = tmp_path / "philips_avent_bridge_abc.json"
        write_bridge_config_file(path, {"sid": "s"})
        write_bridge_config_file(path, {"sid": "s"})
        assert [p.name for p in tmp_path.iterdir()] == ["philips_avent_bridge_abc.json"]

    def test_the_same_config_gives_the_same_bytes(self, tmp_path):
        path = tmp_path / "bridge.json"
        write_bridge_config_file(path, {"sid": "s", "cameras": [{"camera_id": "a"}]})
        first = path.read_bytes()
        write_bridge_config_file(path, {"sid": "s", "cameras": [{"camera_id": "a"}]})
        assert path.read_bytes() == first


class TestLeftovers:
    """A hard crash between creating the temporary file and the replace leaves it behind.

    It holds the live session, so the next write removes it, but only once it is a
    minute old: another write may be in flight and its temporary file must survive.
    """

    @staticmethod
    def _leftover(tmp_path, name, age_seconds):
        leftover = tmp_path / name
        leftover.write_text('{"sid": "old"}')
        then = time.time() - age_seconds
        os.utime(leftover, (then, then))
        return leftover

    def test_an_old_temporary_file_is_removed(self, tmp_path):
        leftover = self._leftover(tmp_path, ".philips_avent_bridge_crashed.tmp", 3600)
        write_bridge_config_file(tmp_path / "philips_avent_bridge_abc.json", {"sid": "s"})
        assert not leftover.exists()

    def test_a_fresh_temporary_file_is_left_alone(self, tmp_path):
        in_flight = self._leftover(tmp_path, ".philips_avent_bridge_busy.tmp", 0)
        write_bridge_config_file(tmp_path / "philips_avent_bridge_abc.json", {"sid": "s"})
        assert in_flight.exists()

    def test_other_files_are_never_touched(self, tmp_path):
        others = [
            self._leftover(tmp_path, "philips_avent_bridge_other.json", 3600),
            self._leftover(tmp_path, ".philips_avent_bridge_note.txt", 3600),
            self._leftover(tmp_path, "unrelated.tmp", 3600),
        ]
        write_bridge_config_file(tmp_path / "philips_avent_bridge_abc.json", {"sid": "s"})
        assert all(p.exists() for p in others)
