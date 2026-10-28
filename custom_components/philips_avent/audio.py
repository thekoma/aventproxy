"""Lullaby audio kept in the cloud: the camera's catalogue and custom recordings.

A camera's lullabies, the 15 presets and up to two recordings of the parents'
own, are entries under one content code that `tuya.m.audio.by.code.get` lists
with their ids. The ids differ per unit: the SCD973 numbers its presets from
3542154, a unit on firmware 1.1.12 from 12072916 (issue #91), and an SCD951 or
SCD953 (product selj2idknqhjnids) uses 19204241-19204255, counting backwards.
Reading the list is the only way to know them.

A custom recording is a 16 kHz mono 16-bit WAV, wrapped in Tuya's encrypted
audio container, PUT to a signed storage URL and then registered against the
camera; writing "upgrade" to DPS 249 makes the camera fetch it. Reverse
engineered from the panel (React Native bundle 000000kt5n 1.3.0) and the app's
com.thingclips.smart.camera.audiomanager package (app 1.10.0).
"""
from __future__ import annotations

import asyncio
import json
import secrets
import string
import struct
from dataclasses import dataclass
from typing import Any

from Crypto.Cipher import AES
from Crypto.Util.Padding import pad

try:
    from .const import LULLABY_BIZ_CODE, LULLABY_TRACKS
except ImportError:
    from const import LULLABY_BIZ_CODE, LULLABY_TRACKS

# What the app records: the panel calls startRecording(16000, 16, 1).
SAMPLE_RATE = 16000
CHANNELS = 1
SAMPLE_WIDTH = 2
# The panel stops a recording at 90 seconds. Longer files are untested.
DEFAULT_MAX_SECONDS = 90
# For a reply without limitNum: the panel refuses a third recording.
DEFAULT_CUSTOM_LIMIT = 2
# Android's MIME type for the recorder's .wav, which the app sends with the PUT.
CONTENT_TYPE = "audio/x-wav"

# FileEncryptionUtil, container version 1.
_CONTAINER_VERSION = 1
_CHUNK = 1280
_FRAME_TYPE = 5
_IV_ALPHABET = string.ascii_letters + string.digits


class RecordingError(Exception):
    """An upload or delete that cannot go ahead, worded for the user."""


@dataclass(frozen=True)
class Track:
    id: int
    name: str
    preset: bool


def parse_catalogue(result: Any) -> list[Track]:
    entries = result.get("audioInfoVOList") if isinstance(result, dict) else None
    tracks = []
    for entry in entries or []:
        audio_id = entry.get("id") if isinstance(entry, dict) else None
        if isinstance(audio_id, int):
            tracks.append(Track(audio_id, str(entry.get("name") or audio_id), bool(entry.get("isPreset"))))
    return tracks


def custom_slots_left(result: Any) -> int:
    """How many more recordings the camera takes. limitNum counts the presets too."""
    tracks = parse_catalogue(result)
    limit = result.get("limitNum") if isinstance(result, dict) else None
    if isinstance(limit, int):
        return max(0, limit - len(tracks))
    return max(0, DEFAULT_CUSTOM_LIMIT - sum(not track.preset for track in tracks))


def _normalise(name: str) -> str:
    return name.replace("\N{RIGHT SINGLE QUOTATION MARK}", "'").casefold().strip()


def track_labels(catalogue: list[Track]) -> dict[int, str]:
    """The select option for each catalogue id.

    Presets keep the names the select has always offered, so an automation
    picking "Brahms' Lullaby" still works although the cloud spells it with a
    curly quote. A recording keeps its own name, with its id appended when that
    name is already taken.
    """
    known = {_normalise(name): name for name in LULLABY_TRACKS}
    taken = set(LULLABY_TRACKS)
    labels: dict[int, str] = {}
    for track in catalogue:
        label = known.get(_normalise(track.name)) if track.preset else None
        if label is None:
            label = track.name if track.name not in taken else f"{track.name} ({track.id})"
        labels[track.id] = label
        taken.add(label)
    return labels


def track_options(catalogue: list[Track] | None, *, preset: bool = True) -> list[str]:
    """The options of one select: the presets, or the recordings.

    The app lists the two apart, a lullaby tab and a recording tab, though it
    plays both with the same command.
    """
    tracks = catalogue or []
    labels = track_labels(tracks)
    options = list(LULLABY_TRACKS) if preset else []
    options.extend(labels[t.id] for t in tracks if t.preset == preset and labels[t.id] not in options)
    return options


def track_id_for(option: str, catalogue: list[Track] | None) -> int | None:
    for track_id, label in track_labels(catalogue or []).items():
        if label == option:
            return track_id
    return None


def track_id_in(value: Any) -> int | None:
    """The track id in DPS 202 or 248, e.g. {"bizcode":"phi-no-bm","id":19204254}.

    The cloud poll returns that JSON with every quote escaped, {\\"bizcode\\":...},
    which json.loads refuses as it stands.
    """
    if isinstance(value, str):
        try:
            value = json.loads(value.replace('\\"', '"'))
        except json.JSONDecodeError:
            return None
    track_id = value.get("id") if isinstance(value, dict) else None
    return track_id if isinstance(track_id, int) else None


def wav_from_pcm(pcm: bytes) -> bytes:
    """The recorder's file: a 44-byte PCM WAV header (WavHeader) and the samples."""
    block_align = CHANNELS * SAMPLE_WIDTH
    fmt = struct.pack(
        "<IHHIIHH", 16, 1, CHANNELS, SAMPLE_RATE, SAMPLE_RATE * block_align, block_align, SAMPLE_WIDTH * 8
    )
    return (
        b"RIFF" + struct.pack("<I", 36 + len(pcm)) + b"WAVE"
        + b"fmt " + fmt
        + b"data" + struct.pack("<I", len(pcm)) + pcm
    )


def duration_ms(pcm_length: int) -> int:
    return pcm_length * 1000 // (SAMPLE_RATE * CHANNELS * SAMPLE_WIDTH)


def new_iv() -> bytes:
    """16 ASCII letters and digits, the kind of IV FileEncryptionUtil.g() makes."""
    return "".join(secrets.choice(_IV_ALPHABET) for _ in range(16)).encode()


def encrypt_recording(data: bytes, key: str, iv: bytes | None = None) -> bytes:
    """Wrap a file in the container the camera decrypts.

    A 56-byte header: version 1, five zero words, the word 110, three zero
    words and the IV. Then each 1280-byte chunk is encrypted on its own with
    AES-CBC and PKCS#5 padding, key and IV unchanged, and written after a
    24-byte frame header: type 5, ciphertext length, 16 zero bytes. The app
    fills a short last chunk with whatever its buffer held from the previous
    one; zeros play as silence instead.
    """
    iv = iv or new_iv()
    key_bytes = key.encode()
    if len(key_bytes) not in (16, 24, 32) or len(iv) != 16:
        raise ValueError("an AES key of 16, 24 or 32 bytes and a 16-byte IV are needed")
    out = bytearray(struct.pack("<6I", _CONTAINER_VERSION, 0, 0, 0, 0, 0))
    out += struct.pack("<4I", 110, 0, 0, 0) + iv
    for start in range(0, len(data), _CHUNK):
        chunk = data[start:start + _CHUNK].ljust(_CHUNK, b"\0")
        encrypted = AES.new(key_bytes, AES.MODE_CBC, iv).encrypt(pad(chunk, AES.block_size))
        out += struct.pack("<II", _FRAME_TYPE, len(encrypted)) + bytes(16) + encrypted
    return bytes(out)


async def decode_to_pcm(ffmpeg: str, path: str, max_seconds: int) -> bytes:
    """Any audio ffmpeg can read, as the recorder's raw samples, cut at max_seconds."""
    proc = await asyncio.create_subprocess_exec(
        ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
        "-i", path, "-t", str(max_seconds), "-vn",
        "-ac", str(CHANNELS), "-ar", str(SAMPLE_RATE), "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1",
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )
    pcm, err = await proc.communicate()
    if proc.returncode != 0:
        detail = err.decode(errors="replace").strip()[-300:]
        raise RecordingError(f"ffmpeg could not read {path}: {detail}")
    if not pcm:
        raise RecordingError(f"{path} holds no audio")
    return pcm


async def upload_recording(api: Any, dev_id: str, pcm: bytes, name: str, *, iv: bytes | None = None) -> int:
    """Store `pcm` as a custom recording of the camera and return its id.

    The camera does not know about it until DPS 249 is set to "upgrade".
    """
    catalogue = await api.get_audio_catalogue(dev_id, LULLABY_BIZ_CODE)
    if custom_slots_left(catalogue) <= 0:
        held = ", ".join(track.name for track in parse_catalogue(catalogue) if not track.preset)
        raise RecordingError(f"The camera holds as many recordings as it takes ({held}); delete one first")

    slot = await api.get_audio_upload_slot(dev_id, LULLABY_BIZ_CODE) or {}
    url, key = slot.get("url"), slot.get("localKey")
    if not url or not key:
        raise RecordingError("The cloud handed out no upload URL for the recording")

    wav = wav_from_pcm(pcm)
    headers = {k: v for k, v in (slot.get("headers") or {}).items() if k.lower() != "content-type"}
    headers["Content-Type"] = CONTENT_TYPE
    await api.put_file(url, encrypt_recording(wav, key, iv), headers)

    audio_id = await api.register_audio(
        dev_id, LULLABY_BIZ_CODE,
        name=name, duration_ms=duration_ms(len(pcm)), size=len(wav),
        key=key, bucket=slot.get("bucket", ""), path=slot.get("key", ""),
    )
    if not isinstance(audio_id, int):
        raise RecordingError(f"The cloud stored the file but returned no recording id ({audio_id!r})")
    return audio_id


async def delete_recording(api: Any, dev_id: str, recording: str) -> Track:
    """Delete a custom recording given by name, select label or id."""
    tracks = parse_catalogue(await api.get_audio_catalogue(dev_id, LULLABY_BIZ_CODE))
    labels = track_labels(tracks)
    custom = [track for track in tracks if not track.preset]
    matches = [track for track in custom if recording in (track.name, str(track.id), labels[track.id])]
    if not matches:
        held = ", ".join(f"{track.name} ({track.id})" for track in custom) or "none"
        raise RecordingError(f"No custom recording matches {recording!r}; the camera holds: {held}")
    if len(matches) > 1:
        raise RecordingError(f"More than one recording is called {recording!r}; give its id instead")
    await api.delete_audio(dev_id, LULLABY_BIZ_CODE, matches[0].id)
    return matches[0]
