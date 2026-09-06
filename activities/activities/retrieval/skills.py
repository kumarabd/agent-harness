"""SkillDiscover — request pipeline step 5
(docs/components/request-pipeline/05-skill-discovery.md; design in
docs/components/skill-subsystem.md, "Retrieval").

Embed the retrieval query, flat-cosine it against the current procedures for
the session's applicable scopes, run the greedy scoring/selection
(`skills.select`), and stage the chosen procedures to `turn_retrieval` as
`kind='skill'` under the plan_id. `prompt.assemble` renders them into the
planning turn's prompt; `RecordSkill` reads them back to attribute the run's
reward to the procedures that fed it.

Best-effort: embeddings unavailable, an empty store, or nothing over the
score floor all return `empty` — the turn runs with no procedural guidance.
"""

from __future__ import annotations

import logging
from datetime import datetime, timezone

from temporalio import activity

from ..metrics import observe_outcome
from ..skills import embedding
from ..skills import select as skill_select
from ..skills import store
from ..types import SkillDiscoverInput, SubsystemResult
from .staging import RetrievalRow, write_rows

logger = logging.getLogger(__name__)


def _days_since(last_used_at, now: datetime) -> float | None:
    if last_used_at is None:
        return None
    if last_used_at.tzinfo is None:
        last_used_at = last_used_at.replace(tzinfo=timezone.utc)
    return max(0.0, (now - last_used_at).total_seconds() / 86400.0)


def _applicable_scopes(plan_id: str) -> tuple[str, ...]:
    # Phase 1: global only. project:/user: scoping lands once the session
    # carries that context (skill-subsystem.md, "scope shadowing").
    return ("global",)


class SkillDiscoverActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="SkillDiscover")
    @observe_outcome("skill_discover_total")
    async def __call__(self, input: SkillDiscoverInput) -> SubsystemResult:
        query = input.retrieval_query.strip()
        if not query:
            return SubsystemResult(status="empty", count=0)

        query_embedding = await embedding.embed(query)
        if query_embedding is None:
            logger.info("SkillDiscover[%s]: embeddings unavailable — no skill retrieval", input.plan_id)
            return SubsystemResult(status="empty", count=0)

        procedures = await store.current_procedures(self._pool, _applicable_scopes(input.plan_id))
        if not procedures:
            return SubsystemResult(status="empty", count=0)

        renders = {p.id: p.render() for p in procedures}
        now = datetime.now(timezone.utc)
        candidates = [
            skill_select.Candidate(
                id=p.id,
                version=p.version,
                trigger_embedding=p.trigger_embedding,
                confidence=p.confidence,
                rendered_size=max(1, len(renders[p.id]) // 4),
                days_since_used=_days_since(p.last_used_at, now),
            )
            for p in procedures
        ]
        edges = await store.edge_weights(self._pool, [p.id for p in procedures])
        chosen = skill_select.select(query_embedding, candidates, edges)
        if not chosen:
            logger.info(
                "SkillDiscover[%s]: no procedure over the score floor (query=%r)", input.plan_id, query
            )
            return SubsystemResult(status="empty", count=0)

        by_id = {p.id: p for p in procedures}
        rows = [
            RetrievalRow(
                kind="skill",
                seq=i,
                # The full rendered procedure — prompt.assemble drops this
                # straight into the planning turn's prompt.
                content=renders[s.id],
                score=s.score,
                metadata={
                    "procedure_id": s.id,
                    "version": s.version,
                    "provenance": by_id[s.id].provenance,
                    "confidence": by_id[s.id].confidence,
                },
            )
            for i, s in enumerate(chosen)
        ]
        written = await write_rows(self._pool, input.plan_id, rows)
        logger.info(
            "SkillDiscover[%s]: staged %d skill row(s): %s",
            input.plan_id,
            written,
            [s.id for s in chosen],
        )
        return SubsystemResult(status="ok", count=written)
