"""Small vector helpers for the skill subsystem — cosine, L2-normalize, and
the EMA blend used to drift a procedure's `trigger_embedding` toward the
tasks that match it (docs/components/skill-subsystem.md, "EMA updates").
Pure, no I/O."""

from __future__ import annotations

import math


def cosine(a: list[float] | None, b: list[float] | None) -> float:
    if not a or not b or len(a) != len(b):
        return 0.0
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    if na == 0.0 or nb == 0.0:
        return 0.0
    return dot / (na * nb)


def normalize(v: list[float]) -> list[float]:
    n = math.sqrt(sum(x * x for x in v))
    return v if n == 0.0 else [x / n for x in v]


def ema_blend(old: list[float] | None, new: list[float] | None, beta: float) -> list[float] | None:
    """(1-beta)*old + beta*new, renormalized. If either is missing or the
    dimensions disagree, the present one wins (no partial blend)."""
    if not old:
        return normalize(list(new)) if new else None
    if not new or len(old) != len(new):
        return old
    return normalize([(1.0 - beta) * o + beta * n for o, n in zip(old, new)])
