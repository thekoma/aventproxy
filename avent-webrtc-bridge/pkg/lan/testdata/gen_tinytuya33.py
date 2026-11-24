"""Generate the protocol 3.3 test vectors in tinytuya33.json with tinytuya 1.20.0.

Fake key and device id only. Run it in a throwaway container:

    docker run --rm -v "$PWD":/vec:ro python:3.13-slim \\
        sh -c "pip install -q tinytuya==1.20.0 && python -I /vec/gen_tinytuya33.py" > tinytuya33.json

Client frames are built as tinytuya's XenonDevice._encode_message builds them
for 3.3 (the "3.3" header in clear in front of most commands, none on DP
queries); device frames carry the return code its unpack_message strips, and
are checked by decoding them back with tinytuya.
"""
import json
import tinytuya
from tinytuya.core import message_helper as M, header as H, crypto_helper as C
from tinytuya.core.message_helper import TuyaMessage, MessagePayload

KEY = b"fake-local-key!!"            # 16 bytes, not a real key
out = {"local_key": KEY.decode()}

d = tinytuya.Device("dev1", address="192.0.2.10", local_key=KEY.decode(), version=3.3)


def client(name, seq, cmd, body):
    d.seqno = seq
    out[name] = {"seq": seq, "cmd": cmd, "plain": body, "frame": d._encode_message(MessagePayload(cmd, body.encode())).hex()}


def device(name, seq, cmd, body, header):
    enc = C.AESCipher(KEY).encrypt(body.encode(), False)
    payload = b"\0\0\0\0" + (H.PROTOCOL_33_HEADER if header else b"") + enc
    frame = M.pack_message(TuyaMessage(seq, cmd, None, payload, 0, True, H.PREFIX_55AA_VALUE, None))
    msg = M.unpack_message(frame)
    assert msg.crc_good and msg.retcode == 0
    assert d._decode_payload(msg.payload) == json.loads(body)
    out[name] = {"seq": seq, "cmd": cmd, "plain": body, "frame": frame.hex()}


client("client7Header", 1, 7, json.dumps({"devId": "dev1", "uid": "dev1", "t": "1700000000", "dps": {"201": "stop"}}, separators=(",", ":")))
client("client10NoHeader", 2, 10, json.dumps({"gwId": "dev1", "devId": "dev1", "uid": "dev1", "t": "1700000000"}, separators=(",", ":")))
device("device10Retcode", 3, 10, json.dumps({"devId": "dev1", "dps": {"1": True, "246": "playing"}}, separators=(",", ":")), False)
device("device8RetcodeHeader", 4, 8, json.dumps({"devId": "dev1", "dps": {"246": "stopping"}, "t": 1700000001}, separators=(",", ":")), True)
print(json.dumps(out, indent=1))
