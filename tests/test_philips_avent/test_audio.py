"""Tests for custom lullaby recordings and the camera's audio catalogue."""

import asyncio
import io
import shutil
import struct
import subprocess
import wave

import pytest
from audio import (
    CONTENT_TYPE,
    RecordingError,
    Track,
    custom_slots_left,
    decode_to_pcm,
    delete_recording,
    duration_ms,
    encrypt_recording,
    new_iv,
    parse_catalogue,
    track_id_for,
    track_id_in,
    track_labels,
    track_options,
    upload_recording,
    wav_from_pcm,
)
from const import LULLABY_BIZ_CODE, LULLABY_TRACKS
from Crypto.Cipher import AES
from Crypto.Util.Padding import unpad

KEY = "0123456789abcdef"
IV = b"AbCdEfGh12345678"


def run(coro):
    return asyncio.run(coro)


def decrypt_like_the_app(blob: bytes, key: str) -> bytes:
    """FileEncryptionUtil.decryptFile, ported line by line from the APK."""
    stream = io.BytesIO(blob)
    version = struct.unpack("<I", stream.read(4))[0]
    assert version == 1
    stream.read(36)
    iv = stream.read(16)
    out = b""
    while header := stream.read(24):
        length = struct.unpack("<I", header[4:8])[0]
        out += unpad(AES.new(key.encode(), AES.MODE_CBC, iv).decrypt(stream.read(length)), 16)
    return out


def catalogue(*custom: tuple[int, str], limit: int | None = 17) -> dict:
    """A reply of tuya.m.audio.by.code.get as an SCD951 gave it: ids counting down."""
    entries = [
        {"id": 19204255 - index, "name": name.replace("'", "\N{RIGHT SINGLE QUOTATION MARK}"), "isPreset": True}
        for index, name in enumerate(LULLABY_TRACKS)
    ]
    entries += [{"id": audio_id, "name": name, "isPreset": False} for audio_id, name in custom]
    result = {"audioInfoVOList": entries}
    if limit is not None:
        result["limitNum"] = limit
    return result


class FakeAPI:
    def __init__(self, listing: dict, slot: dict | None = None, new_id=777):
        self.listing = listing
        self.slot = slot if slot is not None else {
            "url": "https://bucket.example/put", "localKey": KEY, "bucket": "ty-eu",
            "key": "/audio/abc", "headers": {"content-type": "text/plain", "x-amz-date": "now"},
        }
        self.new_id = new_id
        self.calls: list = []

    async def get_audio_catalogue(self, dev_id, code):
        self.calls.append(("catalogue", dev_id, code))
        return self.listing

    async def get_audio_upload_slot(self, dev_id, code):
        self.calls.append(("slot", dev_id, code))
        return self.slot

    async def put_file(self, url, data, headers):
        self.calls.append(("put", url, data, headers))

    async def register_audio(self, dev_id, code, **fields):
        self.calls.append(("register", dev_id, code, fields))
        return self.new_id

    async def delete_audio(self, dev_id, code, audio_id):
        self.calls.append(("delete", dev_id, code, audio_id))


class TestContainer:
    def test_header_is_the_one_the_app_writes(self):
        blob = encrypt_recording(b"x" * 10, KEY, IV)
        assert struct.unpack("<10I", blob[:40]) == (1, 0, 0, 0, 0, 0, 110, 0, 0, 0)
        assert blob[40:56] == IV

    def test_round_trips_through_the_apps_decryptor(self):
        data = bytes(range(256)) * 11  # 2816 bytes: two full chunks and a short one
        plain = decrypt_like_the_app(encrypt_recording(data, KEY, IV), KEY)
        assert plain[:len(data)] == data
        assert plain[len(data):] == b"\0" * (3 * 1280 - len(data))

    def test_every_frame_is_type_5_and_1296_bytes(self):
        blob = encrypt_recording(b"\1" * 3000, KEY, IV)
        frames = blob[56:]
        assert len(frames) == 3 * (24 + 1296)
        for offset in range(0, len(frames), 24 + 1296):
            assert struct.unpack("<II", frames[offset:offset + 8]) == (5, 1296)
            assert frames[offset + 8:offset + 24] == bytes(16)

    def test_rejects_a_key_aes_cannot_use(self):
        with pytest.raises(ValueError):
            encrypt_recording(b"x", "short", IV)

    def test_iv_is_16_ascii_alphanumerics(self):
        iv = new_iv()
        assert len(iv) == 16
        assert iv.decode().isalnum()


class TestWav:
    def test_header_describes_16khz_mono_16bit(self):
        pcm = b"\0\1" * 16000
        with wave.open(io.BytesIO(wav_from_pcm(pcm))) as wav:
            assert wav.getframerate() == 16000
            assert wav.getnchannels() == 1
            assert wav.getsampwidth() == 2
            assert wav.getnframes() == 16000

    def test_header_is_44_bytes(self):
        assert len(wav_from_pcm(b"")) == 44

    def test_duration_in_milliseconds(self):
        assert duration_ms(32000) == 1000
        assert duration_ms(48000) == 1500


class TestCatalogue:
    def test_parses_ids_names_and_presets(self):
        tracks = parse_catalogue(catalogue((1, "Mum")))
        assert len(tracks) == 16
        assert tracks[-1] == Track(1, "Mum", False)
        assert all(track.preset for track in tracks[:15])

    def test_tolerates_an_empty_or_odd_reply(self):
        assert parse_catalogue(None) == []
        assert parse_catalogue({"audioInfoVOList": [{"name": "no id"}, "junk"]}) == []

    def test_slots_come_from_limitnum(self):
        assert custom_slots_left(catalogue()) == 2
        assert custom_slots_left(catalogue((1, "a"))) == 1
        assert custom_slots_left(catalogue((1, "a"), (2, "b"))) == 0

    def test_slots_fall_back_to_two_without_limitnum(self):
        assert custom_slots_left(catalogue((1, "a"), limit=None)) == 1


class TestTrackLabels:
    def test_presets_keep_the_names_the_select_always_had(self):
        labels = track_labels(parse_catalogue(catalogue()))
        assert labels[19204255] == "Baa Baa Black Sheep"
        assert labels[19204254] == "Brahms' Lullaby"
        assert labels[19204241] == "Rain Shower"

    def test_lullaby_options_are_the_presets_alone(self):
        assert track_options(parse_catalogue(catalogue((5, "Mum singing")))) == LULLABY_TRACKS

    def test_recording_options_are_the_recordings_alone(self):
        tracks = parse_catalogue(catalogue((5, "Mum singing"), (6, "White Noise")))
        assert track_options(tracks, preset=False) == ["Mum singing", "White Noise (6)"]
        assert track_options(parse_catalogue(catalogue()), preset=False) == []

    def test_options_without_a_catalogue(self):
        assert track_options(None) == LULLABY_TRACKS
        assert track_options(None, preset=False) == []

    def test_a_recording_named_like_a_preset_gets_its_id(self):
        labels = track_labels(parse_catalogue(catalogue((5, "White Noise"))))
        assert labels[5] == "White Noise (5)"

    def test_option_resolves_to_this_cameras_id(self):
        tracks = parse_catalogue(catalogue((5, "Mum singing")))
        assert track_id_for("Brahms' Lullaby", tracks) == 19204254
        assert track_id_for("Mum singing", tracks) == 5
        assert track_id_for("Mum singing", None) is None


class TestTrackIdIn:
    def test_the_escaped_form_the_cloud_poll_returns(self):
        assert track_id_in('{\\"bizcode\\":\\"phi-no-bm\\",\\"id\\":19204254,\\"errcode\\":0}') == 19204254

    def test_plain_json_and_a_dict(self):
        assert track_id_in('{"bizcode":"phi-no-bm","id":5,"errcode":0}') == 5
        assert track_id_in({"bizcode": "phi-no-bm", "id": 5}) == 5

    def test_anything_else_is_none(self):
        for value in (None, "", "junk", '{"id":"5"}', "[1]", 5):
            assert track_id_in(value) is None


class TestUpload:
    def test_slot_put_register_in_that_order(self):
        api = FakeAPI(catalogue())
        audio_id = run(upload_recording(api, "cam", b"\0\1" * 16000, "Song", iv=IV))
        assert audio_id == 777
        assert [call[0] for call in api.calls] == ["catalogue", "slot", "put", "register"]
        assert all(call[2] == LULLABY_BIZ_CODE for call in api.calls if call[0] != "put")

    def test_put_sends_the_encrypted_wav_as_audio_x_wav(self):
        api = FakeAPI(catalogue())
        pcm = b"\0\1" * 16000
        run(upload_recording(api, "cam", pcm, "Song", iv=IV))
        _, url, data, headers = api.calls[2]
        assert url == "https://bucket.example/put"
        assert decrypt_like_the_app(data, KEY)[:44 + len(pcm)] == wav_from_pcm(pcm)
        assert headers == {"x-amz-date": "now", "Content-Type": CONTENT_TYPE}

    def test_register_describes_the_plain_wav(self):
        api = FakeAPI(catalogue())
        run(upload_recording(api, "cam", b"\0\1" * 16000, "Song", iv=IV))
        fields = api.calls[3][3]
        assert fields == {
            "name": "Song", "duration_ms": 1000, "size": 44 + 32000,
            "key": KEY, "bucket": "ty-eu", "path": "/audio/abc",
        }

    def test_a_full_camera_is_refused_before_anything_is_sent(self):
        api = FakeAPI(catalogue((1, "a"), (2, "b")))
        with pytest.raises(RecordingError, match="a, b"):
            run(upload_recording(api, "cam", b"\0\0", "Song"))
        assert [call[0] for call in api.calls] == ["catalogue"]

    def test_no_upload_url_is_an_error(self):
        api = FakeAPI(catalogue(), slot={})
        with pytest.raises(RecordingError):
            run(upload_recording(api, "cam", b"\0\0", "Song"))

    def test_no_id_back_is_an_error(self):
        api = FakeAPI(catalogue(), new_id=None)
        with pytest.raises(RecordingError):
            run(upload_recording(api, "cam", b"\0\0", "Song"))


class TestDelete:
    def test_by_name(self):
        api = FakeAPI(catalogue((5, "Mum singing")))
        assert run(delete_recording(api, "cam", "Mum singing")).id == 5
        assert api.calls[-1] == ("delete", "cam", LULLABY_BIZ_CODE, 5)

    def test_by_id_or_select_label(self):
        api = FakeAPI(catalogue((5, "White Noise")))
        assert run(delete_recording(api, "cam", "5")).id == 5
        assert run(delete_recording(api, "cam", "White Noise (5)")).id == 5

    def test_never_a_preset(self):
        api = FakeAPI(catalogue((5, "Mum singing")))
        with pytest.raises(RecordingError, match="Mum singing"):
            run(delete_recording(api, "cam", "Rain Shower"))
        assert not any(call[0] == "delete" for call in api.calls)

    def test_ambiguous_name_asks_for_the_id(self):
        api = FakeAPI(catalogue((5, "Song"), (6, "Song")))
        with pytest.raises(RecordingError, match="id"):
            run(delete_recording(api, "cam", "Song"))


FFMPEG = shutil.which("ffmpeg")


@pytest.mark.skipif(FFMPEG is None, reason="ffmpeg not installed")
class TestDecode:
    def _tone(self, path, seconds):
        subprocess.run(
            [FFMPEG, "-loglevel", "error", "-f", "lavfi", "-i", f"sine=frequency=440:duration={seconds}",
             "-ac", "2", "-ar", "44100", str(path)],
            check=True,
        )

    def test_any_input_becomes_16khz_mono_16bit(self, tmp_path):
        source = tmp_path / "tone.mp3"
        self._tone(source, 2)
        pcm = run(decode_to_pcm(FFMPEG, str(source), 90))
        assert abs(duration_ms(len(pcm)) - 2000) < 100

    def test_cut_at_the_maximum(self, tmp_path):
        source = tmp_path / "tone.wav"
        self._tone(source, 3)
        pcm = run(decode_to_pcm(FFMPEG, str(source), 1))
        assert duration_ms(len(pcm)) == 1000

    def test_unreadable_file_is_a_recording_error(self, tmp_path):
        source = tmp_path / "not-audio.mp3"
        source.write_text("hello")
        with pytest.raises(RecordingError, match="ffmpeg"):
            run(decode_to_pcm(FFMPEG, str(source), 90))
