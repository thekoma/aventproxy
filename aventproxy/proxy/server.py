"""RTSP proxy server for Tuya cameras that reject OPTIONS requests."""

import logging
import re
import socket
import threading
from dataclasses import dataclass

log = logging.getLogger(__name__)

FAKE_OPTIONS_RESP = (
    "RTSP/1.0 200 OK\r\n"
    "CSeq: {cseq}\r\n"
    "Public: DESCRIBE, SETUP, TEARDOWN, PLAY, PAUSE\r\n"
    "\r\n"
)

RTSP_METHODS = frozenset({
    "DESCRIBE", "SETUP", "PLAY", "PAUSE", "TEARDOWN",
    "GET_PARAMETER", "SET_PARAMETER", "ANNOUNCE", "RECORD",
})


@dataclass
class CameraTarget:
    name: str
    host: str
    port: int = 554


class RTSPProxy:
    def __init__(
        self,
        cameras: list[CameraTarget],
        bind: str = "127.0.0.1",
        port: int = 8554,
    ):
        self.cameras = {c.name: c for c in cameras}
        self.bind = bind
        self.port = port
        self._sock: socket.socket | None = None
        self._running = False

    def start(self):
        self._sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._sock.bind((self.bind, self.port))
        self._sock.listen(10)
        self._running = True

        log.info("Listening on %s:%d", self.bind, self.port)
        for name, cam in self.cameras.items():
            log.info("  /%s/* -> %s:%d", name, cam.host, cam.port)

        try:
            while self._running:
                try:
                    client, addr = self._sock.accept()
                except OSError:
                    break
                threading.Thread(
                    target=self._handle_client,
                    args=(client, addr),
                    daemon=True,
                ).start()
        finally:
            self._sock.close()

    def stop(self):
        self._running = False
        if self._sock:
            try:
                self._sock.close()
            except OSError:
                pass

    def _handle_client(self, client: socket.socket, addr: tuple):
        log.info("Client connected: %s:%d", *addr)
        try:
            _Session(self, client).run()
        except Exception:
            log.exception("Session error for %s:%d", *addr)
        finally:
            client.close()
            log.info("Client disconnected: %s:%d", *addr)


class _Session:
    def __init__(self, proxy: RTSPProxy, client: socket.socket):
        self.proxy = proxy
        self.client = client
        self.client.settimeout(120)
        self.cam_sock: socket.socket | None = None
        self.cam: CameraTarget | None = None
        self.proxy_base: str | None = None
        self.cam_bases: set[str] = set()

    def run(self):
        try:
            self._run_rtsp()
        finally:
            if self.cam_sock:
                self.cam_sock.close()

    def _run_rtsp(self):
        while True:
            msg = read_rtsp_message(self.client)
            if msg is None:
                return

            text = msg.decode("utf-8", errors="replace")
            parts = text.split(" ", 2)
            if len(parts) < 2:
                return

            method = parts[0]

            if method == "OPTIONS":
                cseq = extract_cseq(text)
                self.client.sendall(
                    FAKE_OPTIONS_RESP.format(cseq=cseq).encode()
                )
                log.debug("Faked OPTIONS (CSeq=%s)", cseq)
                continue

            if method not in RTSP_METHODS:
                continue

            url = parts[1]
            cam_name, remainder = split_camera_path(url)

            if not cam_name or cam_name not in self.proxy.cameras:
                cseq = extract_cseq(text)
                self.client.sendall(
                    f"RTSP/1.0 404 Not Found\r\nCSeq: {cseq}\r\n\r\n".encode()
                )
                return

            if self.cam is None or self.cam.name != cam_name:
                self._connect_camera(cam_name)
                self.proxy_base = extract_base_from_url(url, cam_name)

            cam_url = f"rtsp://{self.cam.host}:{self.cam.port}{remainder}"
            self.cam_bases.add(f"rtsp://{self.cam.host}:{self.cam.port}")

            rewritten = text.replace(url, cam_url, 1)
            self.cam_sock.sendall(rewritten.encode())

            resp_bytes = read_rtsp_message(self.cam_sock)
            if resp_bytes is None:
                return

            resp = resp_bytes.decode("utf-8", errors="replace")

            for m in re.finditer(r"rtsp://[\w.\-]+(?::\d+)?", resp):
                found = m.group(0)
                if found != self.proxy_base:
                    self.cam_bases.add(found)

            for base in sorted(self.cam_bases, key=len, reverse=True):
                resp = resp.replace(base, self.proxy_base)

            self.client.sendall(resp.encode())
            log.debug("%s -> %s", method, cam_url)

            if method == "PLAY":
                self._passthrough()
                return

    def _connect_camera(self, name: str):
        if self.cam_sock:
            self.cam_sock.close()
        self.cam = self.proxy.cameras[name]
        self.cam_sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.cam_sock.settimeout(10)
        self.cam_sock.connect((self.cam.host, self.cam.port))
        log.info(
            "Connected to camera '%s' at %s:%d",
            name, self.cam.host, self.cam.port,
        )

    def _passthrough(self):
        def fwd(src, dst):
            try:
                while True:
                    data = src.recv(65536)
                    if not data:
                        break
                    dst.sendall(data)
            except (OSError, ConnectionError):
                pass
            finally:
                try:
                    dst.shutdown(socket.SHUT_WR)
                except OSError:
                    pass

        t1 = threading.Thread(
            target=fwd, args=(self.client, self.cam_sock), daemon=True,
        )
        t2 = threading.Thread(
            target=fwd, args=(self.cam_sock, self.client), daemon=True,
        )
        t1.start()
        t2.start()
        t1.join()
        t2.join()


def split_camera_path(url: str) -> tuple[str | None, str]:
    """Extract camera name and remaining path from RTSP URL.

    >>> split_camera_path("rtsp://host:8554/baby/stream_0")
    ('baby', '/stream_0')
    >>> split_camera_path("rtsp://host:8554/baby/stream_0/trackID=0")
    ('baby', '/stream_0/trackID=0')
    >>> split_camera_path("rtsp://host:8554/")
    (None, '/')
    """
    path = re.sub(r"^rtsp://[^/]*", "", url)
    segments = path.strip("/").split("/", 1)
    if not segments or not segments[0]:
        return None, path
    name = segments[0]
    remainder = "/" + segments[1] if len(segments) > 1 else "/"
    return name, remainder


def extract_base_from_url(url: str, camera_name: str) -> str:
    """Extract the proxy base URL up to and including the camera name.

    >>> extract_base_from_url("rtsp://myhost:8554/baby/stream_0", "baby")
    'rtsp://myhost:8554/baby'
    """
    idx = url.index(f"/{camera_name}")
    return url[: idx + len(camera_name) + 1]


def extract_cseq(text: str) -> str:
    m = re.search(r"CSeq:\s*(\d+)", text, re.IGNORECASE)
    return m.group(1) if m else "1"


def read_rtsp_message(sock: socket.socket) -> bytes | None:
    data = b""
    while b"\r\n\r\n" not in data:
        try:
            chunk = sock.recv(65536)
        except (socket.timeout, OSError):
            return None
        if not chunk:
            return None
        data += chunk

    hdr_end = data.index(b"\r\n\r\n") + 4
    hdr_text = data[:hdr_end].decode("utf-8", errors="replace")

    m = re.search(r"Content-[Ll]ength:\s*(\d+)", hdr_text)
    if m:
        need = int(m.group(1))
        while len(data) - hdr_end < need:
            try:
                chunk = sock.recv(65536)
            except (socket.timeout, OSError):
                return None
            if not chunk:
                return None
            data += chunk
        return data[: hdr_end + need]

    return data[:hdr_end]
