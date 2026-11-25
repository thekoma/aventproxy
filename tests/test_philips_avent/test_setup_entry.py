"""Setting up an entry when the cloud refuses one of its monitors.

Loads __init__.py against the stand-in in ha_stand_in.py, with the coordinator,
the bridge config writer and the services replaced by fakes, and runs
async_setup_entry for an account with two monitors.
"""
from __future__ import annotations

import asyncio
import importlib.util
import sys

import aiohttp
import pytest

from .ha_stand_in import PKG, PKG_DIR, _Names, home_assistant_installed, load

pytestmark = pytest.mark.skipif(home_assistant_installed(), reason="uses a stand-in for Home Assistant")

CAMERAS = [
    {"id": "cam1", "name": "Monitor A", "product_id": "pid1"},
    {"id": "cam2", "name": "Monitor B", "product_id": "pid2"},
]


class FakeConfig:
    def path(self, *parts):
        return "/".join(("/nonexistent", *parts))


class FakeConfigEntries:
    def __init__(self):
        self.forwarded = []
        self.reloaded = []

    def async_update_entry(self, entry, data=None, **_):
        entry.data = data

    async def async_reload(self, entry_id):
        self.reloaded.append(entry_id)

    async def async_forward_entry_setups(self, entry, platforms):
        self.forwarded.append(entry.entry_id)


class FakeHass:
    def __init__(self):
        self.config = FakeConfig()
        self.config_entries = FakeConfigEntries()
        self.data = {}

    async def async_add_executor_job(self, func, *args):
        return func(*args)


class FakeEntry:
    entry_id = "entry1"

    def __init__(self, cameras=CAMERAS):
        self.data = {"sid": "sid", "device_id": "dev", "cameras": cameras}
        self.options = {"lan_streaming": False}

    def async_on_unload(self, func):
        pass

    def add_update_listener(self, listener):
        return lambda: None

    def async_create_background_task(self, hass, coro, name):
        coro.close()


@pytest.fixture
def setup(monkeypatch):
    load(monkeypatch, "coordinator")
    api = sys.modules[f"{PKG}.api"]
    exceptions = sys.modules["homeassistant.exceptions"]
    update_failed = sys.modules["homeassistant.helpers.update_coordinator"].UpdateFailed
    monkeypatch.setattr(sys.modules["homeassistant.const"], "Platform", _Names(), raising=False)
    services = type(sys)(f"{PKG}.services")

    async def _no_services(hass):
        return None

    services.async_setup_services = _no_services
    monkeypatch.setitem(sys.modules, f"{PKG}.services", services)
    # __init__.py runs as the stand-in package itself, so its relative imports
    # find the modules loaded above and the fake services.
    spec = importlib.util.spec_from_file_location(PKG, PKG_DIR / "__init__.py", submodule_search_locations=[str(PKG_DIR)])
    module = importlib.util.module_from_spec(spec)
    monkeypatch.setitem(sys.modules, PKG, module)
    spec.loader.exec_module(module)

    async def _no_write(hass, entry, api, cameras, lan):
        return None

    monkeypatch.setattr(module, "_write_bridge_config", _no_write)

    def tuya(code, message):
        # As the coordinator's poll raises it: UpdateFailed from the TuyaAPIError.
        try:
            raise update_failed(f"Error fetching data: {code}: {message}") from api.TuyaAPIError(code, message)
        except update_failed as err:
            return err

    def run(failures, cameras=CAMERAS):
        """failures: monitor id -> the coordinator's failure in its first refresh."""

        class FakeCoordinator:
            started = []

            def __init__(self, hass, api, cam_id, cam_name, **_):
                self.camera_id = cam_id
                # What async_setup_entry sets from the bridge's paths.
                self.camera_name, self.display_name, self.rtsp_path = cam_name, cam_name, None
                # What the bridge config reads.
                self.device_info, self.local_key, self.lan_ip, self.lan_protocol = {}, None, None, None

            async def async_config_entry_first_refresh(self):
                cause = failures.get(self.camera_id)
                if isinstance(cause, exceptions.ConfigEntryAuthFailed):
                    raise cause
                if cause is not None:
                    # As Home Assistant 2026.11 raises it: the failure is the cause.
                    err = exceptions.ConfigEntryNotReady()
                    err.__cause__ = cause
                    raise err

            async def start_lan(self, *args):
                FakeCoordinator.started.append(self.camera_id)

            async def stop_lan(self):
                pass

        monkeypatch.setattr(module, "PhilipsAventCoordinator", FakeCoordinator)
        hass, entry = FakeHass(), FakeEntry(cameras)

        async def scenario():
            try:
                return await module.async_setup_entry(hass, entry)
            finally:
                # An unload would close it; without one the run can warn about an unclosed session.
                if data := hass.data.get("philips_avent", {}).get(entry.entry_id):
                    await data["session"].close()

        return asyncio.run(scenario()), hass, FakeCoordinator.started

    run.tuya = tuya
    run.module = module
    run.exceptions = exceptions
    return run


def _warnings(caplog):
    return [r.getMessage() for r in caplog.records if r.levelname == "WARNING" and "did not start" in r.getMessage()]


def test_the_cloud_session_gives_up_after_30_s(setup):
    # aiohttp's default is five minutes, which held a command to a silent cloud that long.
    ok, hass, _ = setup({})
    assert ok is True
    assert hass.data["philips_avent"]["entry1"]["session"].timeout.total == 30


class TestMonitorWithoutAccess:
    def test_a_monitor_the_account_cannot_reach_is_skipped(self, setup, caplog):
        caplog.set_level("WARNING")
        ok, hass, started = setup({"cam2": setup.tuya("PERMISSION_DENIED", "No access")})
        assert ok is True
        assert started == ["cam1"]
        assert list(hass.data["philips_avent"]["entry1"]["coordinators"]) == ["cam1"]
        assert hass.config_entries.forwarded == ["entry1"]
        [warning] = _warnings(caplog)
        assert warning.startswith("Monitor B did not start")
        assert "PERMISSION_DENIED: No access" in warning
        assert "reloading the integration or restarting Home Assistant tries it again" in warning

    @pytest.mark.parametrize(
        "failure",
        [
            pytest.param(lambda s: s.tuya("DEVICE_NOT_EXIST", "No such device"), id="another-tuya-code"),
            pytest.param(lambda s: aiohttp.ClientConnectionError("connection reset"), id="network"),
            pytest.param(lambda s: TimeoutError(), id="timeout"),
        ],
    )
    def test_any_other_error_still_retries_the_entry(self, setup, caplog, failure):
        caplog.set_level("WARNING")
        cause = failure(setup)
        with pytest.raises(setup.exceptions.ConfigEntryNotReady) as raised:
            setup({"cam2": cause})
        assert raised.value.__cause__ is cause
        assert not _warnings(caplog)

    def test_every_monitor_without_access_still_retries_the_entry(self, setup, caplog):
        caplog.set_level("WARNING")
        first = setup.tuya("PERMISSION_DENIED", "No access")
        with pytest.raises(setup.exceptions.ConfigEntryNotReady) as raised:
            setup({"cam1": first, "cam2": setup.tuya("PERMISSION_DENIED", "No access")})
        # The first monitor's own error, as before.
        assert raised.value.__cause__ is first
        assert not _warnings(caplog)

    def test_a_skipped_monitor_keeps_its_path_for_a_same_named_one(self, setup):
        # The bridge assigns paths over every monitor in its config, the skipped one
        # too: it keeps the plain path and the running one is served under _<id prefix>.
        name = "Philips Avent Baby Monitor"
        cameras = [
            {"id": "a1b2c3d4e5", "name": name, "product_id": "pid1"},
            {"id": "f6e5d4c3b2", "name": name, "product_id": "pid1"},
        ]
        ok, hass, started = setup({"a1b2c3d4e5": setup.tuya("PERMISSION_DENIED", "No access")}, cameras)
        assert ok is True
        assert started == ["f6e5d4c3b2"]
        coordinator = hass.data["philips_avent"]["entry1"]["coordinators"]["f6e5d4c3b2"]
        assert coordinator.rtsp_path == "Philips_Avent_Baby_Monitor_f6e5d4"
        assert coordinator.display_name == "Philips Avent Baby Monitor f6e5d4"

    def test_a_refused_session_still_asks_for_a_new_login(self, setup):
        refused = setup.exceptions.ConfigEntryAuthFailed("Authentication failed: USER_SESSION_LOSS")
        with pytest.raises(setup.exceptions.ConfigEntryAuthFailed):
            setup({"cam2": refused})


class TestUpdateListener:
    """Home Assistant calls the update listener on every update of the entry, its data too."""

    @staticmethod
    def _updated(setup, options):
        ok, hass, _ = setup({})
        assert ok is True
        entry = type("Entry", (), {"entry_id": "entry1", "options": options})()
        asyncio.run(setup.module._async_options_updated(hass, entry))
        return hass.config_entries.reloaded

    def test_an_update_of_the_data_alone_does_not_reload(self, setup):
        # A reauth stores the new session and reloads the entry itself.
        assert self._updated(setup, {"lan_streaming": False}) == []

    def test_an_options_change_reloads(self, setup):
        assert self._updated(setup, {"lan_streaming": True}) == ["entry1"]

    def test_an_entry_that_is_not_loaded_reloads_as_before(self, setup):
        hass = FakeHass()
        entry = type("Entry", (), {"entry_id": "entry1", "options": {}})()
        asyncio.run(setup.module._async_options_updated(hass, entry))
        assert hass.config_entries.reloaded == ["entry1"]
