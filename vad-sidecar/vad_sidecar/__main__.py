import logging
import os

from .server import run_server

if __name__ == "__main__":
    logging.basicConfig(level=os.environ.get("LOG_LEVEL", "INFO"))
    run_server(
        bind=os.environ.get("VAD_SIDECAR_BIND", "0.0.0.0"),
        port=int(os.environ.get("VAD_SIDECAR_PORT", "8500")),
    )
