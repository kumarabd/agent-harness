"""Task-run resolution + checkpoint feed — docs/components/request-pipeline/08-planning.md.

Decision B (episode-lifecycle.md): there is no `episodes` table. A Deliberate
task-run *is* a `PlanWorkflow` (id "<plan_id>:plan"); `plan_id` == the anchor
turn id; `turns.plan_id` marks every turn in the run.

  - `ResolveOpenPlan` — dispatch.go's front-door question: is a task-run in
    progress for this session, and does this new message continue it?
  - `NextCheckpoint` — the PlanWorkflow's execution feed: PLAN.md → the first
    non-terminal checkpoint, formatted as a checkpoint turn's seed message.
"""

from __future__ import annotations

import logging

from temporalio import activity

from . import plan
from .skills import embedding
from .skills.vectors import cosine
from .types import (
    NextCheckpointResult,
    ResolveOpenPlanInput,
    ResolveOpenPlanResult,
)

logger = logging.getLogger(__name__)

_CONT_FLOOR = 0.55  # low-confidence continuation tiebreaker (biased toward continue)


async def _anchor_text(conn, turn_id: str) -> str:
    row = await conn.fetchrow(
        "SELECT content FROM messages WHERE parent_id = $1 AND seq = 0 AND role = 'user'", turn_id
    )
    return (row["content"] if row and row["content"] else "").strip()


class ResolveOpenPlanActivity:
    def __init__(self, pool, temporal_client):
        self._pool = pool
        self._client = temporal_client

    @activity.defn(name="ResolveOpenPlan")
    async def __call__(self, inp: ResolveOpenPlanInput) -> ResolveOpenPlanResult:
        async with self._pool.acquire() as conn:
            row = await conn.fetchrow(
                "SELECT plan_id FROM turns "
                "WHERE parent_id = $1 AND parent_type = 'session' AND plan_id IS NOT NULL "
                "ORDER BY started_at DESC LIMIT 1",
                inp.session_key,
            )
        if not row or not row["plan_id"]:
            return ResolveOpenPlanResult()
        plan_id = row["plan_id"]

        # Is that PlanWorkflow still running?
        try:
            desc = await self._client.get_workflow_handle(plan_id + ":plan").describe()
            running = desc.status is not None and desc.status.name == "RUNNING"
        except Exception:  # noqa: BLE001 — not found / gone ⇒ not running
            running = False
        if not running:
            return ResolveOpenPlanResult()

        # Continue it, or supersede it with a fresh run?
        task = inp.task
        if task.confidence >= 0.5:
            cont = bool(task.continues_prior)
        else:
            async with self._pool.acquire() as conn:
                new_txt = await _anchor_text(conn, inp.turn_id)
                prev_txt = await _anchor_text(conn, plan_id)
            cont = True
            try:
                a, b = await embedding.embed(new_txt), await embedding.embed(prev_txt)
                if a and b:
                    cont = cosine(a, b) >= _CONT_FLOOR
            except Exception:  # noqa: BLE001 — optional signal, default to continue
                logger.warning("ResolveOpenPlan: embedding tiebreaker failed — defaulting to continue")

        logger.info("ResolveOpenPlan[%s]: plan %s running, continue=%s", inp.session_key, plan_id, cont)
        if cont:
            return ResolveOpenPlanResult(plan_id=plan_id, should_continue=True)
        return ResolveOpenPlanResult(plan_id=plan_id, supersede=True)


class NextCheckpointActivity:
    def __init__(self, pool):
        self._pool = pool  # unused (PLAN.md is on the PV, not Postgres) — kept for symmetry

    @activity.defn(name="NextCheckpoint")
    async def __call__(self, plan_id: str) -> NextCheckpointResult:
        checkpoints = await plan.read(plan_id)
        rendered = plan.render_block(checkpoints)
        for cp in checkpoints:
            if cp.status in ("done", "skipped"):
                continue
            if cp.complex:
                # Seeds a nested PlanWorkflow's planning turn (3C-iii) — it will
                # propose_plan, not checkpoint_done. PlanWorkflow marks this
                # checkpoint done when the nested plan finishes.
                seed = (
                    f"Plan and carry out this piece of a larger task: {cp.intent}"
                )
                if cp.done_when:
                    seed += f"\n  done when: {cp.done_when}"
            else:
                seed = (
                    f"You are executing one checkpoint of a plan. Do this step, then call "
                    f"checkpoint_done({{\"checkpoint_id\": \"{cp.cp_id}\", \"status\": \"done\"}}) "
                    f'(or "skipped" if you deliberately bypass it). If the rest of the plan needs '
                    f"to change given what you found, pass revised_tail.\n\n"
                    f"THIS CHECKPOINT ({cp.cp_id}): {cp.intent}"
                )
                if cp.done_when:
                    seed += f"\n  done when: {cp.done_when}"
            if rendered:
                seed += f"\n\nThe whole plan, for context:\n{rendered}"
            return NextCheckpointResult(
                has_next=True, checkpoint_id=cp.cp_id, seed_text=seed, complex=cp.complex
            )
        return NextCheckpointResult(has_next=False)


class MarkCheckpointDoneActivity:
    def __init__(self, pool):
        self._pool = pool  # unused (PLAN.md is on the PV)

    @activity.defn(name="MarkCheckpointDone")
    async def __call__(self, plan_id: str, checkpoint_id: str) -> int:
        """3C-iii merge-back: a nested PlanWorkflow finished the checkpoint that
        spawned it, so mark it done on the *parent* ledger. The nested plan does
        no checkpoint_done of its own."""
        return await plan.apply_checkpoint_done(
            plan_id, [{"checkpoint_id": checkpoint_id, "status": "done"}]
        )
