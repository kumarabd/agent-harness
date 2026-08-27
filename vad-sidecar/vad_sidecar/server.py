"""gRPC server for the vad-sidecar — despite the package's original name,
now serves TWO independent gRPC services on one process/port: Silero VAD
(vadsidecar.VAD, per proto/vad_sidecar.proto) and, since docs/components/
gateway/discord-voice.md's "In Progress: Turn-Taking Model", LiveKit's
turn-detector v1-mini end-of-turn model (eotsidecar.EOT, per
proto/eot_sidecar.proto). Colocated rather than a second sidecar container —
same "no new resource footprint, same-pod, low-latency" reasoning that
already justified this container's existence for Silero specifically; gRPC
supports registering multiple independent services on one server/port
without needing to touch anything about how the existing VAD service is
served.

Protocol: gRPC for both — a same-pod, unary, small-payload call, but
standardized on gRPC (not plain HTTP+JSON) so the wire contract is a typed
.proto shared between this service and its Go client, and health-checking
uses the standard grpc.health.v1 protocol rather than a bespoke endpoint.

Stateless: every VAD RPC carries the full state (frame, model state,
rolling context) it needs, and every EOT RPC carries its own full audio
window — this process holds no per-connection or per-speaker state at all
for either service (docs/components/gateway/discord-voice.md's "Resolved:
Silero VAD" — Gateway owns state per speaker, not this sidecar).
"""

from __future__ import annotations

import logging
from concurrent import futures

import grpc
import numpy as np
from grpc_health.v1 import health, health_pb2, health_pb2_grpc

from .eot_model import MAX_SAMPLES, EndOfTurnModel
from .model import CONTEXT_SAMPLES, FRAME_SAMPLES, STATE_SHAPE, SileroVADModel
from .pb import eot_sidecar_pb2, eot_sidecar_pb2_grpc, vad_sidecar_pb2, vad_sidecar_pb2_grpc

logger = logging.getLogger("vad_sidecar")

VAD_SERVICE_NAME = "vadsidecar.VAD"
EOT_SERVICE_NAME = "eotsidecar.EOT"


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


class EOTServicer(eot_sidecar_pb2_grpc.EOTServicer):
    def __init__(self, model: EndOfTurnModel) -> None:
        self._model = model

    def Predict(self, request, context):
        try:
            pcm = np.frombuffer(request.pcm, dtype="<i2")
            if pcm.shape != (MAX_SAMPLES,):
                raise ValueError(f"pcm must have {MAX_SAMPLES} int16 samples, got {pcm.shape}")
        except Exception as exc:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, f"bad request: {exc}")
            return

        try:
            probability = self._model.predict(pcm)
        except Exception as exc:
            logger.exception("end-of-turn prediction failed")
            context.abort(grpc.StatusCode.INTERNAL, f"prediction failed: {exc}")
            return

        return eot_sidecar_pb2.PredictResponse(probability=probability)


def run_server(bind: str, port: int) -> None:
    vad_model = SileroVADModel()
    eot_model = EndOfTurnModel()

    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    vad_sidecar_pb2_grpc.add_VADServicer_to_server(VADServicer(vad_model), server)
    eot_sidecar_pb2_grpc.add_EOTServicer_to_server(EOTServicer(eot_model), server)

    # Standard grpc.health.v1 protocol (google.golang.org/grpc/health on the
    # Go client side) — resolves the "not yet designed: health-check
    # mechanism" question left open in the design pass. Set SERVING only
    # once both models have actually loaded above; a client checking health
    # gets a real, meaningful answer, not "the process is up." Each service
    # gets its own health key (grpc.health.v1's own per-service design) so a
    # future caller could distinguish "VAD is up but EOT failed to load" —
    # not exercised by either Go client yet (both only check "" today), but
    # correct to register regardless.
    health_servicer = health.HealthServicer()
    health_pb2_grpc.add_HealthServicer_to_server(health_servicer, server)
    health_servicer.set(VAD_SERVICE_NAME, health_pb2.HealthCheckResponse.SERVING)
    health_servicer.set(EOT_SERVICE_NAME, health_pb2.HealthCheckResponse.SERVING)
    health_servicer.set("", health_pb2.HealthCheckResponse.SERVING)  # overall server health

    server.add_insecure_port(f"{bind}:{port}")
    server.start()
    logger.info("vad-sidecar (gRPC, VAD+EOT) listening on %s:%d", bind, port)
    server.wait_for_termination()
