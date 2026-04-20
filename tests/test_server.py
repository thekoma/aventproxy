"""Tests for the RTSP proxy server."""

import socket
import time

from proxy.server import (
    extract_base_from_url,
    extract_cseq,
    split_camera_path,
)


class TestSplitCameraPath:
    def test_simple(self):
        assert split_camera_path("rtsp://h:8554/cam/stream_0") == (
            "cam", "/stream_0",
        )

    def test_with_track(self):
        assert split_camera_path("rtsp://h:8554/cam/stream_0/trackID=0") == (
            "cam", "/stream_0/trackID=0",
        )

    def test_camera_only(self):
        assert split_camera_path("rtsp://h:8554/cam") == ("cam", "/")

    def test_no_camera(self):
        assert split_camera_path("rtsp://h:8554/") == (None, "/")

    def test_empty_path(self):
        assert split_camera_path("rtsp://h:8554") == (None, "")

    def test_deep_path(self):
        assert split_camera_path("rtsp://h:8554/cam/a/b/c") == (
            "cam", "/a/b/c",
        )


class TestExtractBaseFromUrl:
    def test_simple(self):
        result = extract_base_from_url("rtsp://h:8554/cam/stream", "cam")
        assert result == "rtsp://h:8554/cam"

    def test_different_host(self):
        result = extract_base_from_url(
            "rtsp://192.168.1.5:9000/baby/stream_0", "baby",
        )
        assert result == "rtsp://192.168.1.5:9000/baby"


class TestExtractCseq:
    def test_found(self):
        assert extract_cseq("DESCRIBE x\r\nCSeq: 42\r\n\r\n") == "42"

    def test_missing(self):
        assert extract_cseq("DESCRIBE x\r\n\r\n") == "1"

    def test_case_insensitive(self):
        assert extract_cseq("cseq: 7\r\n") == "7"


class TestOptionsHandling:
    def test_options_returns_200(self, proxy_with_camera):
        proxy, port, fake_cam = proxy_with_camera

        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.settimeout(5)
        sock.connect(("127.0.0.1", port))

        sock.sendall(
            b"OPTIONS rtsp://127.0.0.1/test_cam RTSP/1.0\r\n"
            b"CSeq: 1\r\n\r\n"
        )
        resp = sock.recv(4096).decode()
        sock.close()

        assert "200 OK" in resp
        assert "CSeq: 1" in resp
        assert "Public:" in resp
        assert "OPTIONS" not in [r for r in fake_cam.requests]

    def test_options_not_forwarded(self, proxy_with_camera):
        proxy, port, fake_cam = proxy_with_camera

        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.settimeout(5)
        sock.connect(("127.0.0.1", port))

        sock.sendall(
            b"OPTIONS rtsp://127.0.0.1/test_cam RTSP/1.0\r\n"
            b"CSeq: 1\r\n\r\n"
        )
        sock.recv(4096)
        sock.close()
        time.sleep(0.1)

        assert "OPTIONS" not in fake_cam.requests


class TestDescribe:
    def test_describe_forwarded(self, proxy_with_camera):
        proxy, port, fake_cam = proxy_with_camera

        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.settimeout(5)
        sock.connect(("127.0.0.1", port))

        sock.sendall(
            f"DESCRIBE rtsp://127.0.0.1:{port}/test_cam/stream_0 RTSP/1.0\r\n"
            f"CSeq: 2\r\n"
            f"Accept: application/sdp\r\n\r\n".encode()
        )
        resp = sock.recv(8192).decode()
        sock.close()

        assert "200 OK" in resp
        assert "application/sdp" in resp
        assert "DESCRIBE" in fake_cam.requests

    def test_url_rewrite_in_response(self, proxy_with_camera):
        proxy, port, fake_cam = proxy_with_camera

        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.settimeout(5)
        sock.connect(("127.0.0.1", port))

        sock.sendall(
            f"DESCRIBE rtsp://127.0.0.1:{port}/test_cam/stream_0 RTSP/1.0\r\n"
            f"CSeq: 2\r\n\r\n".encode()
        )
        resp = sock.recv(8192).decode()
        sock.close()

        assert f"rtsp://127.0.0.1:{port}/test_cam" in resp
        assert f"rtsp://127.0.0.1:{fake_cam.port}" not in resp


class TestUnknownCamera:
    def test_404_for_unknown(self, proxy_with_camera):
        proxy, port, fake_cam = proxy_with_camera

        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.settimeout(5)
        sock.connect(("127.0.0.1", port))

        sock.sendall(
            b"DESCRIBE rtsp://127.0.0.1/nonexistent/stream_0 RTSP/1.0\r\n"
            b"CSeq: 1\r\n\r\n"
        )
        resp = sock.recv(4096).decode()
        sock.close()

        assert "404" in resp


class TestFullSession:
    def test_describe_setup_play(self, proxy_with_camera):
        proxy, port, fake_cam = proxy_with_camera

        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.settimeout(5)
        sock.connect(("127.0.0.1", port))

        base_url = f"rtsp://127.0.0.1:{port}/test_cam"

        # OPTIONS
        sock.sendall(
            f"OPTIONS {base_url} RTSP/1.0\r\nCSeq: 1\r\n\r\n".encode()
        )
        resp = sock.recv(4096).decode()
        assert "200 OK" in resp

        # DESCRIBE
        sock.sendall(
            f"DESCRIBE {base_url}/stream_0 RTSP/1.0\r\n"
            f"CSeq: 2\r\nAccept: application/sdp\r\n\r\n".encode()
        )
        resp = sock.recv(8192).decode()
        assert "200 OK" in resp
        assert "H264" in resp

        # SETUP
        sock.sendall(
            f"SETUP {base_url}/stream_0/trackID=0 RTSP/1.0\r\n"
            f"CSeq: 3\r\n"
            f"Transport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n\r\n".encode()
        )
        resp = sock.recv(4096).decode()
        assert "200 OK" in resp
        assert "Session:" in resp

        # PLAY
        sock.sendall(
            f"PLAY {base_url}/stream_0 RTSP/1.0\r\n"
            f"CSeq: 4\r\nSession: 12345\r\n\r\n".encode()
        )
        resp = sock.recv(8192)
        resp_text = resp.decode("utf-8", errors="replace")
        assert "200 OK" in resp_text

        sock.close()

        assert fake_cam.requests == ["DESCRIBE", "SETUP", "PLAY"]
