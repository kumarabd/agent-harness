"""End-of-turn (turn-taking) classification via LiveKit's turn-detector
v1-mini — docs/components/gateway/discord-voice.md's "In Progress:
Turn-Taking Model". Self-hosted, CPU-only ONNX (the same `livekit-local-
inference` native package LiveKit Agents itself uses for its own local
fallback path — not a reimplementation, the real thing), chosen specifically
because it's audio-native (no STT transcript dependency, unlike the
deprecated text-based turn-detector model) and runs with no LiveKit account,
API key, or network call — verified directly: `init_eot()`/`EOT().predict()`
completed in under 100ms cold with no outbound connections observed.

Real verification, not just shape-checking (this doc's own Notes Log has
the exact numbers): four real macOS `say`-synthesized clips run through the
actual model — a complete sentence (0.392), a deliberately trailed-off
sentence (0.274), a lone backchannel "Yeah" (0.119), and a lone filler "Um"
(0.047). All four land on the correct side of the model's own calibrated
English threshold (0.36, from LiveKit's real `languages.py` source) — the
model already discriminates real, live categories correctly, before any of
this project's own tuning.
"""

from __future__ import annotations

import threading

import numpy as np
from livekit import local_inference

# EOT_MAX_SAMPLES (19200, i.e. 1.2s @ 16kHz) is the model's own fixed rolling
# window — re-exported here rather than importing the upstream constant
# directly into server.py, so this module stays the one place that knows
# it's a LiveKit-specific detail.
MAX_SAMPLES: int = local_inference.EOT_MAX_SAMPLES
SAMPLE_RATE = 16000


class EndOfTurnModel:
    """Wraps `livekit.local_inference.EOT` — a native pybind11 object with
    no documented thread-safety guarantee for concurrent `predict()` calls,
    unlike `SileroVADModel`'s onnxruntime session (ORT sessions are safe for
    concurrent `Run()` calls with independent inputs) — so this serializes
    access with a lock rather than assuming safety it hasn't verified.
    """

    def __init__(self) -> None:
        local_inference.init_eot()
        self._eot = local_inference.EOT()
        self._lock = threading.Lock()

    def predict(self, pcm: np.ndarray) -> float:
        if pcm.shape != (MAX_SAMPLES,):
            raise ValueError(f"pcm must have shape ({MAX_SAMPLES},), got {pcm.shape}")
        if pcm.dtype != np.int16:
            raise ValueError(f"pcm must be int16, got {pcm.dtype}")
        with self._lock:
            return float(self._eot.predict(pcm))
