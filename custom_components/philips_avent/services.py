"""Actions: upload and delete custom lullaby recordings.

The phone app records up to two lullabies of the parents' own. upload_lullaby
fills one of those slots from an audio file instead; see audio.py for the
protocol. The recording then shows up in the lullaby recording select.
"""
from __future__ import annotations

import shutil
from pathlib import Path

import aiohttp
import voluptuous as vol
from homeassistant.core import HomeAssistant, ServiceCall, ServiceResponse, SupportsResponse
from homeassistant.exceptions import HomeAssistantError, ServiceValidationError
from homeassistant.helpers import config_validation as cv
from homeassistant.helpers import device_registry as dr

from . import audio
from .api import TuyaAPIError
from .const import DOMAIN, DPS_VOICE_UPGRADE, VOICE_UPGRADE_SIGNAL
from .coordinator import PhilipsAventCoordinator

SERVICE_UPLOAD_LULLABY = "upload_lullaby"
SERVICE_DELETE_LULLABY = "delete_lullaby"
ATTR_DEVICE_ID = "device_id"
ATTR_FILE = "file"
ATTR_NAME = "name"
ATTR_MAX_DURATION = "max_duration"
ATTR_RECORDING = "recording"

UPLOAD_SCHEMA = vol.Schema({
    vol.Required(ATTR_DEVICE_ID): cv.string,
    vol.Required(ATTR_FILE): cv.string,
    vol.Optional(ATTR_NAME): vol.All(cv.string, vol.Length(min=1, max=64)),
    vol.Optional(ATTR_MAX_DURATION, default=audio.DEFAULT_MAX_SECONDS): vol.All(
        vol.Coerce(int), vol.Range(min=1, max=600)
    ),
})
DELETE_SCHEMA = vol.Schema({
    vol.Required(ATTR_DEVICE_ID): cv.string,
    vol.Required(ATTR_RECORDING): cv.string,
})


def _coordinator_for_device(hass: HomeAssistant, device_id: str) -> PhilipsAventCoordinator:
    device = dr.async_get(hass).async_get(device_id)
    cam_ids = {ident for domain, ident in device.identifiers if domain == DOMAIN} if device else set()
    for entry_data in hass.data.get(DOMAIN, {}).values():
        for cam_id, coordinator in entry_data["coordinators"].items():
            if cam_id in cam_ids:
                return coordinator
    raise ServiceValidationError(f"Device {device_id} is not a Philips Avent camera that is set up")


async def _tell_camera(coordinator: PhilipsAventCoordinator) -> None:
    """Make the camera fetch its recordings again, then refresh the select."""
    await coordinator.set_dps({DPS_VOICE_UPGRADE: VOICE_UPGRADE_SIGNAL})
    await coordinator.refresh_catalogue(force=True)


def async_register_services(hass: HomeAssistant) -> None:
    async def upload_lullaby(call: ServiceCall) -> ServiceResponse:
        coordinator = _coordinator_for_device(hass, call.data[ATTR_DEVICE_ID])
        path = Path(call.data[ATTR_FILE])
        if not path.is_absolute() or not await hass.async_add_executor_job(
            hass.config.is_allowed_path, str(path)
        ):
            raise ServiceValidationError(
                f"{path} is outside the folders Home Assistant may read. Put it under /media, "
                "or add its folder to allowlist_external_dirs"
            )
        if not await hass.async_add_executor_job(path.is_file):
            raise ServiceValidationError(f"{path} does not exist")
        ffmpeg = shutil.which("ffmpeg")
        if not ffmpeg:
            raise HomeAssistantError("ffmpeg is needed to convert the file and was not found")

        name = call.data.get(ATTR_NAME) or path.stem
        try:
            pcm = await audio.decode_to_pcm(ffmpeg, str(path), call.data[ATTR_MAX_DURATION])
            audio_id = await audio.upload_recording(coordinator.api, coordinator.camera_id, pcm, name)
        except audio.RecordingError as err:
            raise HomeAssistantError(str(err)) from err
        except (TuyaAPIError, aiohttp.ClientError, TimeoutError) as err:
            raise HomeAssistantError(f"Uploading the lullaby failed: {err}") from err
        try:
            await _tell_camera(coordinator)
        except (TuyaAPIError, aiohttp.ClientError, TimeoutError) as err:
            raise HomeAssistantError(
                f"{name} is stored as recording {audio_id}, but the camera could not be told to "
                f"fetch it: {err}"
            ) from err
        return {"id": audio_id, "name": name, "duration": audio.duration_ms(len(pcm)) / 1000}

    async def delete_lullaby(call: ServiceCall) -> ServiceResponse:
        coordinator = _coordinator_for_device(hass, call.data[ATTR_DEVICE_ID])
        try:
            track = await audio.delete_recording(coordinator.api, coordinator.camera_id, call.data[ATTR_RECORDING])
            await _tell_camera(coordinator)
        except audio.RecordingError as err:
            raise ServiceValidationError(str(err)) from err
        except (TuyaAPIError, aiohttp.ClientError, TimeoutError) as err:
            raise HomeAssistantError(f"Deleting the lullaby failed: {err}") from err
        return {"id": track.id, "name": track.name}

    hass.services.async_register(
        DOMAIN, SERVICE_UPLOAD_LULLABY, upload_lullaby,
        schema=UPLOAD_SCHEMA, supports_response=SupportsResponse.OPTIONAL,
    )
    hass.services.async_register(
        DOMAIN, SERVICE_DELETE_LULLABY, delete_lullaby,
        schema=DELETE_SCHEMA, supports_response=SupportsResponse.OPTIONAL,
    )
