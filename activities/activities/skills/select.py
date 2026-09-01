"""Retrieval scoring + selection — the pure core of `SkillDiscover`
(docs/components/skill-subsystem.md, "Retrieval").

Flat cosine over the current procedures, then greedy budget-bounded
selection blending similarity + co-occurrence + confidence + recency −
diversity. No I/O; unit-testable.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

from .vectors import cosine

__all__ = ["Candidate", "Scored", "cosine", "select"]

# Starting weights — skill-subsystem.md's own values. Tunable, deferred.
W_SIM = 1.0
W_CO = 0.5
W_CONF = 0.3
W_REC = 0.1
W_DIV = 0.3
# exp decay on days-since-last-successful-use — ~1/e per month.
RECENCY_LAMBDA = 1.0 / 30.0

# A candidate must clear this blended score to be selected at all.
SCORE_FLOOR = 0.30
# Hard caps on how much skill guidance enters one prompt.
MAX_PROCEDURES = 3
TOKEN_BUDGET = 1200


@dataclass
class Candidate:
    """The minimal shape `select` needs from a procedure."""

    id: str
    version: int
    trigger_embedding: list[float] | None
    confidence: float
    rendered_size: int  # tokens the procedure will occupy in the prompt
    days_since_used: float | None = None  # None → never used → no recency contribution


@dataclass
class Scored:
    id: str
    version: int
    score: float
    sim: float


def select(
    query_embedding: list[float],
    candidates: list[Candidate],
    edges: dict[frozenset, float] | None = None,
) -> list[Scored]:
    """Greedy selection. `edges` is `{frozenset({id_a, id_b}): weight}` from
    the co-occurrence graph — once a procedure is selected, others that
    co-occur with it (and succeeded) get a boost, so the bundle emerges from
    usage. Returns the chosen candidates with their final blended scores, in
    selection order. Empty if nothing clears the floor."""
    edges = edges or {}
    sims = {c.id: cosine(query_embedding, c.trigger_embedding) for c in candidates}
    by_id = {c.id: c for c in candidates}

    selected: list[Scored] = []
    remaining = [c for c in candidates if sims[c.id] > 0.0]
    budget = TOKEN_BUDGET

    while remaining and len(selected) < MAX_PROCEDURES:
        best, best_score = None, None
        for c in remaining:
            div = max(
                (cosine(c.trigger_embedding, by_id[s.id].trigger_embedding) for s in selected),
                default=0.0,
            )
            co = (
                sum(edges.get(frozenset((c.id, s.id)), 0.0) for s in selected) / len(selected)
                if selected
                else 0.0
            )
            rec = 0.0 if c.days_since_used is None else math.exp(-RECENCY_LAMBDA * c.days_since_used)
            score = (
                W_SIM * sims[c.id]
                + W_CO * co
                + W_CONF * c.confidence
                + W_REC * rec
                - W_DIV * div
            )
            if best_score is None or score > best_score:
                best, best_score = c, score
        if best is None or best_score < SCORE_FLOOR:
            break
        if best.rendered_size > budget:
            remaining.remove(best)
            continue
        budget -= best.rendered_size
        selected.append(Scored(id=best.id, version=best.version, score=best_score, sim=sims[best.id]))
        remaining.remove(best)

    return selected
