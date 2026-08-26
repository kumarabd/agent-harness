"""gRPC server for the VAD sidecar.

Protocol: gRPC, per proto/vad_sidecar.proto — a same-pod, unary, small-
payload call, but standardized on gRPC (not the earlier plain-HTTP+JSON
prototype) so the wire contract is a typed .proto shared between this
service and its Go client, and health-checking uses the standard
grpc.health.v1 protocol rather than a bespoke endpoint.

Stateless: every RPC carries the full state (frame, model state, rolling
context) it needs; this process holds no per-connection or per-speaker
state at all (docs/components/gateway/discord-voice.md's "Resolved: Silero
VAD" — Gateway owns state per speaker, not this sidecar).
"""

from __future__ import annotations

import logging
from concurrent import futures

import grpc
import numpy as np
from grpc_health.v1 import health, health_pb2, health_pb2_grpc

from .model import CONTEXT_SAMPLES, FRAME_SAMPLES, STATE_SHAPE, SileroVADModel
from .pb import vad_sidecar_pb2, vad_sidecar_pb2_grpc

logger = logging.getLogger("vad_sidecar")

SERVICE_NAME = "vadsidecar.VAD"


class VADServicer(vad_sidecar_pb2_grpc.VADServicer):
    def __init__(self, model: SileroVADModel) -> None:
        self._model = model

    def Classify(self, request, context):
        try:
            frame = np.frombuffer(request.frame, dtype="<f4")
            state = np.frombuffer(request.state, dtype="<f4")
            ctx = np.frombuffer(request.context, dtype="<f4")
            if frame.shape != (FRAME_SAMPLES,):
                raise ValueError(f"frame must have {FRAME_SAMPLES} float32 samples, got {frame.shape}")
            if ctx.shape != (CONTEXT_SAMPLES,):
                raise ValueError(f"context must have {CONTEXT_SAMPLES} float32 samples, got {ctx.shape}")
            state = state.reshape(STATE_SHAPE)
        except Exception as exc:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, f"bad request: {exc}")
            return

        try:
            probability, new_state, new_context = self._model.classify(frame, state, ctx)
        except Exception as exc:
            logger.exception("classification failed")
            context.abort(grpc.StatusCode.INTERNAL, f"classification failed: {exc}")
            return

        return vad_sidecar_pb2.ClassifyResponse(
            probability=probability,
            state=new_state.astype("<f4").tobytes(),
            context=new_context.astype("<f4").tobytes(),
        )


def run_server(bind: str, port: int) -> None:
    model = SileroVADModel()

    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    vad_sidecar_pb2_grpc.add_VADServicer_to_server(VADServicer(model), server)

    # Standard grpc.health.v1 protocol (google.golang.org/grpc/health on the
    # Go client side) — resolves the "not yet designed: health-check
    # mechanism" question left open in the design pass. Set SERVING only
    # once the model has actually loaded above; a client checking health
    # gets a real, meaningful answer, not "the process is up."
    health_servicer = health.HealthServicer()
    health_pb2_grpc.add_HealthServicer_to_server(health_servicer, server)
    health_servicer.set(SERVICE_NAME, health_pb2.HealthCheckResponse.SERVING)
    health_servicer.set("", health_pb2.HealthCheckResponse.SERVING)  # overall server health

    server.add_insecure_port(f"{bind}:{port}")
    server.start()
    logger.info("vad-sidecar (gRPC) listening on %s:%d", bind, port)
    server.wait_for_termination()
