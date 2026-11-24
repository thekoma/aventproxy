"""Drives aventproxy-bridge-addon/run.sh against a fake bridge in a temp dir.

Covers the supervise loop (issues #73, #159): waiting for a config, restarting
the bridge on any exit status, following the newest config file and stopping
promptly on SIGTERM. The timings are shrunk through the environment variables
that run.sh reads for exactly this purpose.
"""

import contextlib
import itertools
import os
import shutil
import signal
import subprocess
import time
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
RUN_SH = ROOT / "aventproxy-bridge-addon" / "run.sh"

pytestmark = pytest.mark.skipif(
    not (shutil.which("bash") and shutil.which("jq") and shutil.which("md5sum")),
    reason="run.sh needs bash, jq and md5sum",
)

# Behaviour is chosen per start from the `mode` file, so a test can change it
# while the supervisor runs. Every start appends "<epoch> <pid> <args>".
FAKE_BRIDGE = """#!/usr/bin/env bash
dir="$(dirname "$0")"
export LC_ALL=C
mode="$(cat "$dir/mode" 2>/dev/null)"
# Armed before the start is announced: a test sends SIGTERM as soon as it sees
# the start, and a TERM that beat the trap would kill this bridge normally.
[ "$mode" = ignoreterm ] && trap '' TERM
echo "$EPOCHREALTIME $$ $*" >> "$dir/starts.log"
echo $$ > "$dir/bridge.pid"
case "$mode" in
    exit1) exit 1 ;;
    exit0) exit 0 ;;
    exit143) exit 143 ;;
    ignoreterm)
        while true; do sleep 0.1; done ;;
    *) exec sleep 300 ;;
esac
"""

FAST_ENV = {
    "POLL_INTERVAL": "1",
    "RESTART_DELAY_MIN": "1",
    "RESTART_DELAY_MAX": "4",
    "STABLE_RUN": "60",
    "STOP_GRACE_TICKS": "8",
}


class Harness:
    def __init__(self, tmp_path):
        self.root = tmp_path
        self.config_dir = tmp_path / "config"
        self.config_dir.mkdir()
        self.options = tmp_path / "options.json"
        self.options.write_text('{"cameras": []}')
        self.bridge = tmp_path / "fake-bridge"
        self.bridge.write_text(FAKE_BRIDGE)
        self.bridge.chmod(0o755)
        self.log_path = tmp_path / "out.log"
        self.proc = None
        self.set_mode("run")

    def set_mode(self, mode):
        (self.root / "mode").write_text(mode)

    def write_config(self, name, body="{}", mtime=None):
        path = self.config_dir / name
        path.write_text(body)
        if mtime is not None:
            os.utime(path, (mtime, mtime))
        return path

    def start(self, wait_for_config="true", extra_env=None):
        env = {
            **os.environ,
            **FAST_ENV,
            "CONFIG_DIR": str(self.config_dir),
            "ADDON_CONFIG": str(self.options),
            "BRIDGE_BIN": str(self.bridge),
            "WAIT_FOR_CONFIG": wait_for_config,
            **(extra_env or {}),
        }
        self.log_file = self.log_path.open("w")
        self.proc = subprocess.Popen(
            ["bash", str(RUN_SH)],
            env=env,
            stdout=self.log_file,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )

    def starts(self):
        path = self.root / "starts.log"
        if not path.exists():
            return []
        return [line.split(" ", 2) for line in path.read_text().splitlines()]

    def log(self):
        return self.log_path.read_text()

    def wait_for(self, predicate, timeout=10.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return True
            time.sleep(0.05)
        return False

    def stop_group(self):
        if self.proc is not None:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(self.proc.pid, signal.SIGKILL)
            self.proc.wait()
            self.log_file.close()


@pytest.fixture
def harness(tmp_path):
    h = Harness(tmp_path)
    yield h
    h.stop_group()


def pid_alive(pid):
    try:
        os.kill(int(pid), 0)
    except ProcessLookupError:
        return False
    return True


def test_waits_for_integration_config_even_though_options_json_exists(harness):
    harness.start()
    time.sleep(1.5)
    assert harness.proc.poll() is None
    assert harness.starts() == []

    harness.write_config("philips_avent_bridge_abc.json")
    assert harness.wait_for(lambda: len(harness.starts()) == 1, timeout=8)
    assert "philips_avent_bridge_abc.json" in harness.starts()[0][2]
    log = harness.log()
    assert log.count("Waiting for bridge config") == 1
    assert "Config found!" in log


def test_addon_options_with_credentials_are_used_when_no_integration_config(harness):
    harness.options.write_text('{"signing_key": "k", "sid": "s"}')
    harness.start()
    assert harness.wait_for(lambda: len(harness.starts()) == 1)
    assert str(harness.options) in harness.starts()[0][2]


def test_fails_fast_without_config_when_not_asked_to_wait(harness):
    harness.start(wait_for_config="false")
    assert harness.wait_for(lambda: harness.proc.poll() is not None, timeout=5)
    assert harness.proc.returncode == 1
    assert "no bridge config found" in harness.log()


@pytest.mark.parametrize("mode", ["exit1", "exit0", "exit143"])
def test_restarts_the_bridge_on_any_exit_status(harness, mode):
    harness.write_config("philips_avent_bridge_abc.json")
    harness.set_mode(mode)
    harness.start()
    assert harness.wait_for(lambda: len(harness.starts()) >= 3, timeout=12)
    assert harness.proc.poll() is None
    assert "restarting in" in harness.log()


def test_restart_delay_grows_for_a_crash_loop(harness):
    harness.write_config("philips_avent_bridge_abc.json")
    harness.set_mode("exit1")
    harness.start()
    assert harness.wait_for(lambda: len(harness.starts()) >= 4, timeout=15)
    times = [float(row[0]) for row in harness.starts()]
    gaps = [b - a for a, b in itertools.pairwise(times)]
    assert gaps[2] > gaps[0] + 0.5


def test_restarts_when_the_config_changes(harness):
    config = harness.write_config("philips_avent_bridge_abc.json", '{"a": 1}')
    harness.start()
    assert harness.wait_for(lambda: len(harness.starts()) == 1)
    first_pid = harness.starts()[0][1]

    config.write_text('{"a": 2}')
    assert harness.wait_for(lambda: len(harness.starts()) == 2, timeout=8)
    assert not pid_alive(first_pid)
    assert harness.proc.poll() is None


def test_a_second_valid_config_does_not_take_over(harness):
    # Two config entries: both files are valid. Following whichever was written
    # last would restart the stream on every rewrite of the other one.
    now = time.time()
    harness.write_config("philips_avent_bridge_old.json", mtime=now - 100)
    harness.start()
    assert harness.wait_for(lambda: len(harness.starts()) == 1)

    harness.write_config("philips_avent_bridge_new.json", mtime=now)
    time.sleep(3)
    assert len(harness.starts()) == 1
    assert harness.proc.poll() is None


def test_switches_when_the_current_config_is_replaced(harness):
    # A re-added integration: the old entry's file goes, the new one appears,
    # and the running bridge moves to it.
    now = time.time()
    old = harness.write_config("philips_avent_bridge_old.json", mtime=now - 100)
    harness.start()
    assert harness.wait_for(lambda: len(harness.starts()) == 1)
    first_pid = harness.starts()[0][1]

    harness.write_config("philips_avent_bridge_new.json", mtime=now)
    old.unlink()
    assert harness.wait_for(lambda: len(harness.starts()) == 2, timeout=8)
    assert "philips_avent_bridge_new.json" in harness.starts()[1][2]
    assert not pid_alive(first_pid)


def test_integration_config_takes_over_from_addon_options(harness):
    harness.options.write_text('{"signing_key": "k", "sid": "s"}')
    harness.start()
    assert harness.wait_for(lambda: len(harness.starts()) == 1)

    harness.write_config("philips_avent_bridge_abc.json")
    assert harness.wait_for(lambda: len(harness.starts()) == 2, timeout=8)
    assert "philips_avent_bridge_abc.json" in harness.starts()[1][2]


def test_switches_when_the_current_config_is_removed_and_the_bridge_exits(harness):
    now = time.time()
    old = harness.write_config("philips_avent_bridge_old.json", mtime=now - 100)
    harness.start()
    assert harness.wait_for(lambda: len(harness.starts()) == 1)

    harness.set_mode("exit1")
    old.unlink()
    harness.write_config("philips_avent_bridge_new.json", mtime=now)
    assert harness.wait_for(lambda: len(harness.starts()) >= 2, timeout=8)
    assert "philips_avent_bridge_new.json" in harness.starts()[1][2]


def test_waits_again_when_the_config_vanishes_and_the_bridge_dies(harness):
    config = harness.write_config("philips_avent_bridge_abc.json")
    harness.start()
    assert harness.wait_for(lambda: len(harness.starts()) == 1)

    harness.set_mode("exit1")
    config.unlink()
    pid = harness.starts()[0][1]
    os.kill(int(pid), signal.SIGKILL)
    time.sleep(3)
    assert harness.proc.poll() is None
    assert len(harness.starts()) == 1

    harness.write_config("philips_avent_bridge_def.json")
    assert harness.wait_for(lambda: len(harness.starts()) == 2, timeout=8)


def test_sigterm_stops_quickly_and_takes_the_bridge_down(harness):
    harness.write_config("philips_avent_bridge_abc.json")
    harness.start()
    assert harness.wait_for(lambda: len(harness.starts()) == 1)
    pid = harness.starts()[0][1]

    began = time.monotonic()
    harness.proc.send_signal(signal.SIGTERM)
    assert harness.wait_for(lambda: harness.proc.poll() is not None, timeout=5)
    assert harness.proc.returncode == 0
    assert time.monotonic() - began < 2
    assert not pid_alive(pid)


def test_sigterm_kills_a_bridge_that_ignores_it(harness):
    harness.write_config("philips_avent_bridge_abc.json")
    harness.set_mode("ignoreterm")
    harness.start()
    assert harness.wait_for(lambda: len(harness.starts()) == 1)
    pid = harness.starts()[0][1]

    harness.proc.send_signal(signal.SIGTERM)
    assert harness.wait_for(lambda: harness.proc.poll() is not None, timeout=6)
    assert harness.proc.returncode == 0
    assert not pid_alive(pid)
    assert "did not stop in time" in harness.log()


def test_sigterm_while_waiting_for_a_config_exits_cleanly(harness):
    harness.start()
    time.sleep(1)
    harness.proc.send_signal(signal.SIGTERM)
    assert harness.wait_for(lambda: harness.proc.poll() is not None, timeout=3)
    assert harness.proc.returncode == 0
