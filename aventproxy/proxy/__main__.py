"""Entry point for the Avent RTSP Proxy."""

import logging
import signal

from .config import load_config
from .server import RTSPProxy


def main():
    config = load_config()
    config.validate()

    logging.basicConfig(
        level=getattr(logging, config.log_level.upper()),
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )

    proxy = RTSPProxy(
        cameras=config.cameras,
        bind=config.bind_address,
        port=config.port,
    )

    def shutdown(signum, frame):
        logging.getLogger(__name__).info("Shutting down...")
        proxy.stop()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)

    proxy.start()


if __name__ == "__main__":
    main()
