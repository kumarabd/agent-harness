"""Silero VAD classification, reimplemented against the raw ONNX session
directly rather than silero_vad's OnnxWrapper convenience class — that
wrapper holds state/context as hidden instance attributes, which is exactly
wrong for this sidecar's stateless-over-the-wire contract
(docs/components/gateway/discord-voice.md's "Resolved: Silero VAD"): the
caller (Gateway) owns state per speaker, this process holds none.

Real I/O contract, verified directly against the bundled ONNX model
(inputs/outputs inspected via onnxruntime.InferenceSession.get_inputs() /
get_outputs(), then round-tripped against real synthesized speech + silence
to confirm classification actually works, not just that shapes line up):

  input:  float32 [1, 64 + FRAME_SAMPLES]  — 64-sample rolling context
          prepended to the new frame, both at 16kHz.
  state:  float32 [2, 1, 128]              — model's recurrent state.
  sr:     int64 scalar                     — 16000 (8000 also supported,
          unused here since Gateway resamples to 16kHz).

  output:  float32 [1, 1]                  — speech probability.
  stateN:  float32 [2, 1, 128]             — updated state for the next call.

The new rolling context to carry into the next call is just the last 64
samples of this call's input (context + frame), not a separate computation.
"""

from __future__ import annotations

import numpy as np
import onnxruntime as ort
from silero_vad import load_silero_vad

SAMPLE_RATE = 16000
FRAME_SAMPLES = 512  # Silero's fixed window size at 16kHz — not negotiable.
CONTEXT_SAMPLES = 64
STATE_SHAPE = (2, 1, 128)


def _locate_bundled_model_path() -> str:
    # load_silero_vad(onnx=True) already resolves/caches the real bundled
    # .onnx file (via silero_vad's own packaged assets) — reuse its
    # resolution rather than re-implementing a model-path lookup, but throw
    # away the OnnxWrapper itself since we only want the raw session.
    return load_silero_vad(onnx=True).session._model_path


class SileroVADModel:
    def __init__(self) -> None:
        self._session = ort.InferenceSession(_locate_bundled_model_path())

    def zero_state(self) -> np.ndarray:
        return np.zeros(STATE_SHAPE, dtype=np.float32)

    def zero_context(self) -> np.ndarray:
        return np.zeros(CONTEXT_SAMPLES, dtype=np.float32)

    def classify(
        self,
        frame: np.ndarray,
        state: np.ndarray,
        context: np.ndarray,
    ) -> tuple[float, np.ndarray, np.ndarray]:
        if frame.shape != (FRAME_SAMPLES,):
            raise ValueError(f"frame must have shape ({FRAME_SAMPLES},), got {frame.shape}")
        if state.shape != STATE_SHAPE:
            raise ValueError(f"state must have shape {STATE_SHAPE}, got {state.shape}")
        if context.shape != (CONTEXT_SAMPLES,):
            raise ValueError(f"context must have shape ({CONTEXT_SAMPLES},), got {context.shape}")

        x = np.concatenate([context, frame]).astype(np.float32).reshape(1, -1)
        out, state_n = self._session.run(
            None,
            {
                "input": x,
                "state": state.astype(np.float32),
                "sr": np.array(SAMPLE_RATE, dtype=np.int64),
            },
        )
        probability = float(out[0][0])
        new_context = x[0, -CONTEXT_SAMPLES:]
        return probability, state_n, new_context
