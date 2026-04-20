"""Shared fixtures for tests."""

import socket
import threading
import time

import pytest
from proxy.server import CameraTarget, RTSPProxy


class FakeTuyaCamera:
    """Mock RTSP server that rejects OPTIONS but accepts everything else."""

    def __init__(self, host: str = "127.0.0.1", port: int = 0):
        self.host = host
        self.port = port
        self._sock: socket.socket | None = None
        self._running = False
        self._thread: threading.Thread | None = None
        self.requests: list[str] = []

    def start(self):
        self._sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._sock.bind((self.host, self.port))
        self._sock.settimeout(2)
        self.port = self._sock.getsockname()[1]
        self._sock.listen(5)
        self._running = True
        self._thread = threading.Thread(target=self._serve, daemon=True)
        self._thread.start()

    def stop(self):
        self._running = False
        if self._sock:
            self._sock.close()
        if self._thread:
            self._thread.join(timeout=3)

    def _serve(self):
        while self._running:
            try:
                client, _ = self._sock.accept()
            except (OSError, socket.timeout):
                continue
            threading.Thread(
                target=self._handle, args=(client,), daemon=True,
            ).start()

    def _handle(self, client: socket.socket):
        client.settimeout(5)
        try:
            while True:
                data = b""
                while b"\r\n\r\n" not in data:
                    chunk = client.recv(4096)
                    if not chunk:
                        return
                    data += chunk

                text = data.decode("utf-8", errors="replace")
                method = text.split(" ", 1)[0]
                self.requests.append(method)
                cseq = "1"
                import re
                m = re.search(r"CSeq:\s*(\d+)", text)
                if m:
                    cseq = m.group(1)

                if method == "OPTIONS":
                    resp = (
                        f"RTSP/1.0 400 Bad OPTIONS request\r\n"
                        f"CSeq: {cseq}\r\n\r\n"
                    )
                    client.sendall(resp.encode())
                    return

                if method == "DESCRIBE":
                    sdp = (
                        "v=0\r\n"
                        f"o=TestServer 1 1 IN IP4 {self.host}\r\n"
                        "s=Test\r\n"
                        "m=video 0 RTP/AVP 96\r\n"
                        "a=rtpmap:96 H264/90000\r\n"
                        "a=control:trackID=0\r\n"
                    )
                    resp = (
                        f"RTSP/1.0 200 OK\r\n"
                        f"CSeq: {cseq}\r\n"
                        f"Content-Type: application/sdp\r\n"
                        f"Content-Base: rtsp://{self.host}:{self.port}/stream_0/\r\n"
                        f"Content-Length: {len(sdp)}\r\n"
                        f"\r\n"
                        f"{sdp}"
                    )
                    client.sendall(resp.encode())

                elif method == "SETUP":
                    resp = (
                        f"RTSP/1.0 200 OK\r\n"
                        f"CSeq: {cseq}\r\n"
                        f"Transport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n"
                        f"Session: 12345;timeout=60\r\n"
                        f"\r\n"
                    )
                    client.sendall(resp.encode())

                elif method == "PLAY":
                    resp = (
                        f"RTSP/1.0 200 OK\r\n"
                        f"CSeq: {cseq}\r\n"
                        f"Session: 12345\r\n"
                        f"RTP-Info: url=rtsp://{self.host}:{self.port}/stream_0;seq=0\r\n"
                        f"\r\n"
                    )
                    client.sendall(resp.encode())
                    client.sendall(b"$\x00\x00\x04FAKE")
                    return

                elif method == "TEARDOWN":
                    resp = (
                        f"RTSP/1.0 200 OK\r\n"
                        f"CSeq: {cseq}\r\n\r\n"
                    )
                    client.sendall(resp.encode())
                    return
                else:
                    resp = (
                        f"RTSP/1.0 200 OK\r\n"
                        f"CSeq: {cseq}\r\n\r\n"
                    )
                    client.sendall(resp.encode())
        except (OSError, ConnectionError):
            pass
        finally:
            client.close()


@pytest.fixture
def fake_camera():
    cam = FakeTuyaCamera()
    cam.start()
    yield cam
    cam.stop()


@pytest.fixture
def proxy_with_camera(fake_camera):
    cameras = [
        CameraTarget(
            name="test_cam",
            host=fake_camera.host,
            port=fake_camera.port,
        ),
    ]
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind(("127.0.0.1", 0))
    proxy_port = sock.getsockname()[1]
    sock.close()

    proxy_obj = RTSPProxy(cameras=cameras, bind="127.0.0.1", port=proxy_port)
    thread = threading.Thread(target=proxy_obj.start, daemon=True)
    thread.start()
    time.sleep(0.3)

    yield proxy_obj, proxy_port, fake_camera

    proxy_obj.stop()
