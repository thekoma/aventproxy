#!/usr/bin/env python3
"""Decode a Tuya LAN (:6668) packet capture, with an eye on IPC_LAN_302 signaling.

Contributed by @leonardpitzu in https://github.com/thekoma/aventproxy/issues/51,
the same work that produced sections 14, 16 and 19 of WHITEPAPER.md.

The camera's local control channel carries more than DPS traffic: when the app
is on the same LAN it also negotiates the video stream there, as frame type 32
(``IPC_LAN_302``), and the camera answers on the same TCP session. This script
turns a capture of that channel into readable JSON.

What it does
    Reassembles the TCP streams in a pcap, replays the ``SESS_KEY_NEG``
    exchange to recover the session key, unpacks every Tuya frame and prints it
    with its command name, decrypting and pretty-printing the JSON body.
    Frame type 32 is flagged in the output.

Usage
    AVENT_LOCAL_KEY=<localKey> tools/lan302_decode.py capture.pcap
    tools/lan302_decode.py capture.pcap --local-key <localKey> --only-302

    Requires ``tinytuya`` and ``pycryptodome``. Input must be a classic pcap;
    convert pcapng with ``tcpdump -r in.pcapng -w out.pcap``.

Capturing
    Take the capture where the frames actually pass. A host on the same subnet
    is not enough -- an access point bridges station-to-station traffic locally
    and it never reaches a third party, so capture on the AP (or mirror the
    camera's switch port) with a filter such as::

        tcpdump -i <iface> -s0 -w cam.pcap 'host <camera-ip> and tcp port 6668'

Notes on the format, each of which will otherwise cost you an afternoon
    - ``tinytuya.parse_header`` rejects frames over ``MAX_PAYLOAD_LENGTH``. The
      offer is ~1.6 kB and trips it, so a naive walker skips the most
      interesting frame and the sequence numbers merely appear to jump. Headers
      are parsed here instead, and passed into ``unpack_message`` so it does not
      re-parse and re-apply the same ceiling.
    - For 6699/GCM frames ``unpack_message`` does not raise on a wrong key; it
      returns ``crc_good=False``. Keys are selected by that flag, not by
      catching exceptions.
    - Frames from the camera carry a 4-byte retcode, frames from the app do not,
      so the two negotiation nonces sit at different offsets. The wrong offset
      yields a plausible session key that decrypts nothing. Self-test:
      ``RESP[16:48] == hmac_sha256(localKey, local_nonce)``.
    - ``json.raw_decode`` parses the ``3.5`` version header as the number 3.5
      and hides the body behind it, so only objects and arrays are accepted.
"""
from __future__ import annotations

import argparse
import binascii
import json
import os
import struct
import sys
from collections import defaultdict

from tinytuya.core.crypto_helper import AESCipher
from tinytuya.core.message_helper import TuyaHeader, unpack_message

TUYA_PORT = 6668

# Header layouts, parsed here rather than via tinytuya.parse_header, which
# rejects anything over MAX_PAYLOAD_LENGTH. That guard is meant for live
# sockets; the IPC_LAN_302 offer is ~1.6 kB and trips it, silently hiding the
# single most interesting frame in a capture.
HEADER_FMT_55AA = ">IIII"
HEADER_FMT_6699 = ">IHIII"
SUFFIX_LEN_6699 = 4
MAX_FRAME = 65535

COMMANDS = {
    1: "AP_CONFIG", 2: "ACTIVE", 3: "SESS_KEY_NEG_START", 4: "SESS_KEY_NEG_RESP",
    5: "SESS_KEY_NEG_FINISH", 6: "UNBIND", 7: "CONTROL", 8: "STATUS",
    9: "HEART_BEAT", 10: "DP_QUERY", 11: "QUERY_WIFI", 12: "TOKEN_BIND",
    13: "CONTROL_NEW", 14: "ENABLE_WIFI", 16: "DP_QUERY_NEW", 17: "SCENE_EXECUTE",
    18: "UPDATEDPS", 19: "UDP_NEW", 20: "AP_CONFIG_NEW",
    32: "IPC_LAN_302", 33: "IPC_LAN_LOCAL_CONFIG", 34: "IPC_LAN_LOCAL_CONFIG_WIFI",
    35: "BOARDCAST_LPV34", 37: "REQ_DEVINFO", 40: "LAN_EXT_STREAM",
}

PREFIXES = (b"\x00\x00\x55\xaa", b"\x00\x00\x66\x99")


# --- pcap -------------------------------------------------------------------

def read_pcap(path):
    """Yield (ts, ip_src, sport, ip_dst, dport, tcp_seq, payload) for TCP packets."""
    with open(path, "rb") as fh:
        blob = fh.read()

    if blob[:4] == b"\x0a\x0d\x0d\x0a":
        sys.exit("pcapng not supported; re-save as classic pcap "
                 "(tcpdump -r in.pcapng -w out.pcap)")

    magic = blob[:4]
    if magic in (b"\xd4\xc3\xb2\xa1", b"\x4d\x3c\xb2\xa1"):
        endian = "<"
    elif magic in (b"\xa1\xb2\xc3\xd4", b"\xa1\xb2\x3c\x4d"):
        endian = ">"
    else:
        sys.exit(f"not a pcap file: magic {binascii.hexlify(magic)!r}")

    linktype = struct.unpack(endian + "I", blob[20:24])[0]
    off = 24
    while off + 16 <= len(blob):
        ts_sec, ts_frac, incl, _orig = struct.unpack(endian + "IIII", blob[off:off + 16])
        off += 16
        frame = blob[off:off + incl]
        off += incl
        pkt = _strip_link(frame, linktype)
        if pkt:
            parsed = _parse_ipv4_tcp(pkt)
            if parsed:
                yield (ts_sec + ts_frac / 1e6, *parsed)


def _strip_link(frame, linktype):
    if linktype == 1:  # Ethernet
        if len(frame) < 14:
            return None
        etype = struct.unpack(">H", frame[12:14])[0]
        payload = frame[14:]
        while etype in (0x8100, 0x88A8):  # VLAN tags
            if len(payload) < 4:
                return None
            etype = struct.unpack(">H", payload[2:4])[0]
            payload = payload[4:]
        return payload if etype == 0x0800 else None
    if linktype == 101:  # RAW IP
        return frame
    if linktype == 113:  # Linux cooked v1
        return frame[16:] if len(frame) > 16 and frame[14:16] == b"\x08\x00" else None
    if linktype == 0:  # BSD loopback
        return frame[4:]
    return None


def _parse_ipv4_tcp(pkt):
    if len(pkt) < 20 or (pkt[0] >> 4) != 4:
        return None
    ihl = (pkt[0] & 0x0F) * 4
    if pkt[9] != 6:  # TCP
        return None
    total_len = struct.unpack(">H", pkt[2:4])[0]
    src = ".".join(str(b) for b in pkt[12:16])
    dst = ".".join(str(b) for b in pkt[16:20])
    tcp = pkt[ihl:total_len] if total_len else pkt[ihl:]
    if len(tcp) < 20:
        return None
    sport, dport = struct.unpack(">HH", tcp[:4])
    seq = struct.unpack(">I", tcp[4:8])[0]
    doff = (tcp[12] >> 4) * 4
    return src, sport, dst, dport, seq, tcp[doff:]


def reassemble(packets):
    """Group TCP payloads into per-direction byte streams, ordered by seq."""
    chunks = defaultdict(dict)
    first_seen = {}
    for ts, src, sport, dst, dport, seq, payload in packets:
        if not payload or TUYA_PORT not in (sport, dport):
            continue
        key = (src, sport, dst, dport)
        chunks[key].setdefault(seq, payload)  # first copy wins, drops retransmits
        first_seen.setdefault(key, ts)

    streams = {}
    for key, by_seq in chunks.items():
        buf = bytearray()
        expected = None
        for seq in sorted(by_seq):
            data = by_seq[seq]
            if expected is not None and seq < expected:
                data = data[expected - seq:]  # trim overlap
                if not data:
                    continue
            buf += data
            expected = seq + len(by_seq[seq])
        streams[key] = (first_seen[key], bytes(buf))
    return streams


# --- Tuya frames ------------------------------------------------------------

def parse_frame_header(data):
    """Header of a Tuya frame, without tinytuya's payload-size ceiling."""
    if data[:4] == PREFIXES[1]:
        size = struct.calcsize(HEADER_FMT_6699)
        prefix, _unknown, seqno, cmd, length = struct.unpack(HEADER_FMT_6699, data[:size])
        total = length + size + SUFFIX_LEN_6699
    else:
        size = struct.calcsize(HEADER_FMT_55AA)
        prefix, seqno, cmd, length = struct.unpack(HEADER_FMT_55AA, data[:size])
        total = length + size
    if not 0 < length <= MAX_FRAME:
        raise ValueError(f"implausible frame length {length}")
    return TuyaHeader(prefix, seqno, cmd, length, total)


def iter_frames(stream):
    """Yield (offset, raw_frame, header) for each Tuya frame in a byte stream."""
    pos = 0
    while pos < len(stream):
        nxt = min((i for i in (stream.find(p, pos) for p in PREFIXES) if i >= 0), default=-1)
        if nxt < 0:
            return
        try:
            header = parse_frame_header(stream[nxt:])
        except Exception:
            pos = nxt + 4
            continue
        frame = stream[nxt:nxt + header.total_length]
        if len(frame) < header.total_length:
            return  # truncated capture tail
        yield nxt, frame, header
        pos = nxt + header.total_length


def session_key(local_key, local_nonce, remote_nonce, version):
    """Derive the 3.4/3.5 session key the way the Tuya SDK does."""
    mixed = bytes(a ^ b for a, b in zip(local_nonce, remote_nonce))
    cipher = AESCipher(local_key)
    if version < 3.5:
        return cipher.encrypt(mixed, use_base64=False, pad=False)
    return cipher.encrypt(mixed, use_base64=False, pad=False, iv=local_nonce[:12])[12:28]


def _as_json(raw):
    """Parse a leading JSON object/array from raw bytes, tolerating trailing padding.

    Scalars are rejected on purpose: raw_decode reads the "3.5" version header
    as the number 3.5 and would swallow the frame body behind it.
    """
    try:
        obj = json.JSONDecoder().raw_decode(raw.decode("utf-8"))[0]
    except Exception:
        return None
    return obj if isinstance(obj, (dict, list)) else None


VERSION_HEADERS = (b"3.1", b"3.2", b"3.3", b"3.4", b"3.5")
VERSION_HEADER_LEN = 15


def _unwrap(payload):
    """Byte offsets worth trying: raw, past a retcode, past a version header."""
    candidates = []
    for base in (payload, payload[4:]):
        for view in (base, base[VERSION_HEADER_LEN:] if base[:3] in VERSION_HEADERS else None):
            if view and view not in candidates:
                candidates.append(view)
    return candidates


def decode_payload(payload, keys):
    """Return a readable payload: JSON object, text, or hex."""
    if not payload:
        return ""
    for candidate in _unwrap(payload):
        obj = _as_json(candidate)
        if obj is not None:
            return obj
        for key in keys:
            try:
                clear = AESCipher(key).decrypt(candidate, use_base64=False, decode_text=False, verify_padding=False)
            except Exception:
                continue
            obj = _as_json(clear)
            if obj is not None:
                return obj
            text = clear.decode("utf-8", "replace")
            if text.isprintable():
                return text
    return "<hex> " + binascii.hexlify(payload).decode()


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("pcap")
    ap.add_argument("--local-key", default=os.environ.get("AVENT_LOCAL_KEY"), help="device localKey (default: $AVENT_LOCAL_KEY)")
    ap.add_argument("--version", type=float, default=3.5)
    ap.add_argument("--only-302", action="store_true", help="print only frame type 32")
    args = ap.parse_args()

    if not args.local_key:
        sys.exit("no localKey: pass --local-key or export AVENT_LOCAL_KEY")
    local_key = args.local_key.encode()

    streams = reassemble(read_pcap(args.pcap))
    if not streams:
        sys.exit(f"no TCP/{TUYA_PORT} payload in {args.pcap}")

    # Pair the two directions of each connection so a negotiation spans both.
    conns = defaultdict(list)
    for (src, sport, dst, dport), (ts, buf) in streams.items():
        conns[frozenset(((src, sport), (dst, dport)))].append((ts, src, sport, dst, dport, buf))

    for members in conns.values():
        members.sort()
        endpoints = " <-> ".join(f"{s}:{p}" for _, s, p, _, _, _ in members[:1]) or "?"
        print(f"\n{'=' * 78}\nconnection {endpoints}  ({len(members)} directions)\n{'=' * 78}")

        nonces = {}
        keys = [local_key]
        events = []
        for ts, src, sport, dst, dport, buf in members:
            for _, frame, header in iter_frames(buf):
                events.append((src, sport, dst, dport, frame, header))

        # First pass: recover the session key from the negotiation frames.
        # Frames from the camera carry a 4-byte retcode, frames from the app do
        # not, so the two nonces sit at different offsets. Getting this wrong
        # still yields a plausible-looking key that decrypts nothing.
        for _src, _sport, _dst, _dport, frame, header in events:
            if header.cmd not in (3, 4):
                continue
            try:
                msg = unpack_message(frame, hmac_key=local_key, header=header, no_retcode=(header.cmd == 3))
            except Exception:
                continue
            if not msg.crc_good or len(msg.payload) < 16:
                continue
            nonces["local" if header.cmd == 3 else "remote"] = msg.payload[:16]
        if "local" in nonces and "remote" in nonces:
            sk = session_key(local_key, nonces["local"], nonces["remote"], args.version)
            keys.insert(0, sk)
            print(f"[session key recovered] {binascii.hexlify(sk).decode()}")

        for src, sport, dst, dport, frame, header in events:
            if args.only_302 and header.cmd != 32:
                continue
            msg = _try_unpack(frame, keys, header)
            name = COMMANDS.get(header.cmd, f"cmd{header.cmd}")
            flag = "  <<< IPC_LAN_302" if header.cmd == 32 else ""
            direction = f"{src}:{sport} -> {dst}:{dport}"
            if msg is None:
                print(f"\n{direction}  seq={header.seqno} {name}({header.cmd})  "f"[undecodable, {header.length}B]{flag}")
                continue
            body = decode_payload(msg.payload, keys)
            if isinstance(body, dict):
                body = json.dumps(body, indent=2, sort_keys=True)
            print(f"\n{direction}  seq={msg.seqno} {name}({header.cmd}) "f"retcode={msg.retcode} ok={msg.crc_good}{flag}\n{body}")


def _try_unpack(frame, keys, header=None):
    # 6699/GCM frames do not raise on a wrong key, they come back with
    # crc_good False, so pick the key that actually authenticates. no_retcode
    # None lets tinytuya detect whether the leading 4 bytes are a retcode or
    # the start of the JSON body, which differs by direction. The header is
    # passed in so unpack_message does not re-parse it and re-apply the size
    # ceiling we deliberately avoid.
    fallback = None
    for key in [*keys, None]:
        for no_retcode in (None, True, False):
            try:
                msg = unpack_message(frame, hmac_key=key, header=header, no_retcode=no_retcode)
            except Exception:
                continue
            if msg.crc_good:
                return msg
            if fallback is None:
                fallback = msg
    return fallback


if __name__ == "__main__":
    main()
