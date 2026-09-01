"""SkillSynthesize — skill subsystem phase 3
(docs/components/skill-subsystem.md, "Synthesis").

Processes the tenant's queue of un-synthesized `skill_candidates`:

  1. Assignment — each candidate goes to the nearest current procedure
     (cosine >= that procedure's own `cluster_radius`, or ASSIGN_RADIUS
     before it has one) or starts a new group.
  2. Creation — a new group with >= 1 success candidate → `generalize` its
     transcripts → a new `learned` procedure at the skeptical prior.
  3. Refinement — an existing procedure's group with >= N_REFINE success
     candidates, OR any candidate whose `composed_from` names it (a
     divergence — re-synthesize now) → `generalize` (current body + the
     trajectories + any matched failures) → a new version.
  4. Failure notes — failure candidates matched to a procedure that isn't
     being refined this run → append distilled cautions, no version bump.
  5. Mark every fetched candidate `synthesized_at`.

Triggered write-first (debounced by a fixed workflow id, same pattern as
agent-brain's mining trigger), best-effort — a failure leaves candidates
queued for the next run.
"""

from __future__ import annotations

import logging
from itertools import combinations

from temporalio import activity

from ..types import SkillSynthesizeInput
from . import embedding, generalize, store
from .vectors import cosine

logger = logging.getLogger(__name__)

ASSIGN_RADIUS = 0.82
SUBCLUSTER_SIM = 0.80
N_REFINE = 3
MAX_CANDIDATES_PER_RUN = 200
_SCOPES = ("global",)

# Per-procedure `cluster_radius` = how tightly its own trajectories cluster,
# minus a slack so future near-misses still assign. Floored so a freak-tight
# group can't make the procedure practically unmatchable; `None` when there
# aren't two embeddings to measure a spread from (fall back to ASSIGN_RADIUS).
_RADIUS_SLACK = 0.05
_RADIUS_FLOOR = 0.60


def _radius(candidates: list) -> float | None:
    vecs = [c.task_embedding for c in candidates if c.task_embedding]
    if len(vecs) < 2:
        return None
    sims = [cosine(a, b) for a, b in combinations(vecs, 2)]
    return max(_RADIUS_FLOOR, (sum(sims) / len(sims)) - _RADIUS_SLACK)


class SkillSynthesizeActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="SkillSynthesize")
    async def __call__(self, input: SkillSynthesizeInput) -> None:
        async with self._pool.acquire() as conn:
            candidates = await store.unsynthesized_candidates(conn, MAX_CANDIDATES_PER_RUN)
            if not candidates:
                return
            procedures = await store.current_procedures(conn, _SCOPES)

        by_procedure: dict[str, list] = {}
        new_groups: list[list] = []
        for cand in candidates:
            nearest_id, best, radius = None, 0.0, ASSIGN_RADIUS
            for proc in procedures:
                sim = cosine(cand.task_embedding, proc.trigger_embedding)
                if sim > best:
                    nearest_id, best = proc.id, sim
                    radius = proc.cluster_radius or ASSIGN_RADIUS
            if nearest_id is not None and best >= radius:
                by_procedure.setdefault(nearest_id, []).append(cand)
            else:
                self._assign_to_new_group(cand, new_groups)

        created, refined, annotated = 0, 0, 0

        for group in new_groups:
            if await self._create(group):
                created += 1

        proc_by_id = {p.id: p for p in procedures}
        for procedure_id, group in by_procedure.items():
            proc = proc_by_id.get(procedure_id)
            if proc is None:
                continue
            outcome = await self._refine_or_annotate(proc, group)
            refined += outcome == "refined"
            annotated += outcome == "annotated"

        async with self._pool.acquire() as conn:
            await store.mark_synthesized(conn, [c.id for c in candidates])

        logger.info(
            "SkillSynthesize: %d candidate(s) → %d created, %d refined, %d annotated (trigger=%s)",
            len(candidates),
            created,
            refined,
            annotated,
            input.trigger_turn_id,
        )

    @staticmethod
    def _assign_to_new_group(cand, new_groups: list[list]) -> None:
        for group in new_groups:
            if cosine(cand.task_embedding, group[0].task_embedding) >= SUBCLUSTER_SIM:
                group.append(cand)
                return
        new_groups.append([cand])

    async def _create(self, group: list) -> bool:
        successes = [c for c in group if c.outcome == "success"]
        if not successes:
            return False
        failures = [c for c in group if c.outcome == "failure"]
        spec = await generalize.generalize(
            [c.transcript for c in successes],
            [c.transcript for c in failures],
            current_body_text=None,
        )
        if spec is None:
            return False
        vector = await embedding.embed(spec["trigger_text"])
        async with self._pool.acquire() as conn:
            procedure_id = await store.insert_learned(
                conn, spec, vector, [c.turn_id for c in group], cluster_radius=_radius(group)
            )
        logger.info("SkillSynthesize: created %s (%r)", procedure_id, spec["title"])
        return True

    async def _refine_or_annotate(self, proc, group: list) -> str:
        successes = [c for c in group if c.outcome == "success"]
        failures = [c for c in group if c.outcome == "failure"]
        is_divergence = any(proc.id in c.composed_from for c in group)

        if successes and (len(successes) >= N_REFINE or is_divergence):
            spec = await generalize.generalize(
                [c.transcript for c in successes],
                [c.transcript for c in failures],
                current_body_text=proc.render(),
            )
            if spec is not None:
                vector = await embedding.embed(spec["trigger_text"])
                async with self._pool.acquire() as conn:
                    await store.new_version(
                        conn,
                        proc.id,
                        spec,
                        vector,
                        [c.turn_id for c in group],
                        cluster_radius=_radius(group),
                    )
                logger.info(
                    "SkillSynthesize: refined %s (divergence=%s, %d successes)",
                    proc.id,
                    is_divergence,
                    len(successes),
                )
                return "refined"

        if failures:
            notes = [
                f"A previous attempt at this failed: {c.task_text[:160]}"
                for c in failures[:3]
            ]
            async with self._pool.acquire() as conn:
                await store.append_notes(conn, proc.id, notes)
            return "annotated"

        return "none"
