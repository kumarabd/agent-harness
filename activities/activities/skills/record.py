"""RecordSkill — the skill subsystem's write path
(docs/components/skill-subsystem.md REVISION 2026-09-02;
docs/components/request-pipeline/08-planning.md).

Fires ONCE when a task-run closes (plan complete, superseded, idle-exit, or a
subagent turn ending). Collapses the old RecordSkillOutcome + `skill_candidates`
+ SkillSynthesize chain into one online step:

  1. Gather the task-run's whole multi-turn trajectory + its terminal outcome.
  2. EMA-update the `confidence` / `trigger_embedding` of every procedure that
     was composed INTO the task-run, and the co-occurrence graph.
  3. Match the trajectory's task against the current procedures:
       - match + success + it's a divergence (this run started from that
         procedure) → re-`generalize` a new version;
       - match + success, not a divergence → a positive EMA on that procedure;
       - no match + success → `generalize` a brand-new `learned:` procedure;
       - match + failure → append a distilled caution note.

No candidates table, no debounce. Best-effort — the response is already
delivered; a failure here changes nothing the user sees.
"""

from __future__ import annotations

import json
import logging

from temporalio import activity

from .. import ids, plan
from ..types import RecordSkillInput
from . import embedding, generalize, store
from .vectors import cosine

logger = logging.getLogger(__name__)

_MAX_TRANSCRIPT_CHARS = 20_000
_MAX_TASK_TEXT_CHARS = 2_000
_SUCCESS_STOP_REASONS = {"no_tool_calls"}
_RECORD_INTENTS = {"task", "question"}
_RECORD_COMPLEXITIES = {"moderate", "complex"}
_SCOPES = ("global",)
# cosine floor for "this trajectory represents an existing procedure" when that
# procedure has no learned cluster_radius yet. Numeric-tuning-deferred.
_MATCH_RADIUS = 0.82


class RecordSkillActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="RecordSkill")
    async def __call__(self, input: RecordSkillInput) -> None:
        plan_id = input.plan_id  # the task-run id (decision B — no `episodes` table)
        intent = input.intent or "task"
        complexity = input.complexity or "moderate"
        close_reason = input.close_reason or ""

        if intent not in _RECORD_INTENTS or complexity not in _RECORD_COMPLEXITIES:
            logger.info(
                "RecordSkill[%s]: intent=%s complexity=%s — nothing worth learning",
                plan_id, intent, complexity,
            )
            return

        async with self._pool.acquire() as conn:
            # Every turn in the run (planning + checkpoints + mid-plan handling)
            # carries turns.plan_id. A nested PlanWorkflow's turn ids all sit
            # under the root's (`<root>:cp:N:sub:...`), so a prefix match sweeps
            # the whole tree — nested plans record nothing of their own (3C-iii,
            # Option A). A Deliberate subagent: its single turn, plan_id unset,
            # so also match turn_id.
            trajectory_filter = (
                "(t.plan_id = $1 OR starts_with(t.plan_id, $1 || ':') "
                "OR (t.plan_id IS NULL AND t.turn_id = $1))"
            )
            messages = await conn.fetch(
                "SELECT m.role, m.content, m.message_id, m.seq "
                "FROM messages m JOIN turns t ON m.parent_id = t.turn_id "
                f"WHERE {trajectory_filter} "
                "ORDER BY t.turn_seq NULLS FIRST, t.started_at, m.seq",
                plan_id,
            )
            if not messages:
                logger.info("RecordSkill[%s]: no messages, skipping", plan_id)
                return
            last_stop_reason = input.stop_reason
            tool_calls = await conn.fetch(
                "SELECT tc.tool_name, tc.status, tc.message_id "
                "FROM tool_calls tc JOIN turns t ON tc.parent_id = t.turn_id "
                f"WHERE {trajectory_filter} ORDER BY tc.tool_call_id",
                plan_id,
            )
            skill_rows = await conn.fetch(
                "SELECT metadata FROM turn_retrieval WHERE owner_id = $1 AND kind = 'skill'",
                plan_id,
            )
            checkpoints = await plan.read(plan_id)

        user_messages = [m for m in messages if m["role"] == "user"]
        task_text = (user_messages[0]["content"] or "").strip() if user_messages else ""
        if not task_text:
            logger.info("RecordSkill[%s]: no user message, skipping", plan_id)
            return

        had_tool_error = any(tc["status"] == "error" for tc in tool_calls)
        clean_stop = last_stop_reason in _SUCCESS_STOP_REASONS and not had_tool_error
        if close_reason == "plan_complete":
            outcome = "success"
        elif close_reason in ("idle", "turn_end", "") and clean_stop:
            outcome = "success"
        else:
            outcome = "failure"
        required_correction = len(user_messages) > 1

        composed_from = sorted(
            {
                (json.loads(r["metadata"]) if r["metadata"] else {}).get("procedure_id")
                for r in skill_rows
            }
            - {None}
        )

        transcript = _build_transcript(messages, tool_calls)
        plan_final = plan.render_final(checkpoints)
        if plan_final:
            transcript = plan_final + "\n\n" + transcript
        task_embedding = await embedding.embed(task_text)

        # --- 2. reinforce the procedures the task-run was composed from
        if composed_from:
            reward = (0.5 if required_correction else 1.0) if outcome == "success" else 0.0
            for procedure_id in composed_from:
                try:
                    await store.ema_update(
                        self._pool, procedure_id, reward, task_embedding if reward > 0.0 else None
                    )
                except Exception:  # noqa: BLE001
                    logger.warning("RecordSkill[%s]: ema_update %s failed", plan_id, procedure_id, exc_info=True)
            try:
                session_key = ids.session_key_of(plan_id)
                recent = await store.session_composed_procedure_ids(self._pool, session_key, plan_id)
                await store.update_cooccurrence(self._pool, composed_from, recent, reward)
            except Exception:  # noqa: BLE001
                logger.warning("RecordSkill[%s]: co-occurrence update failed", plan_id, exc_info=True)

        # --- 3. match-or-insert against the current store
        try:
            await self._match_or_insert(
                plan_id, task_text, task_embedding, transcript, outcome, set(composed_from)
            )
        except Exception:  # noqa: BLE001 — best-effort, the task-run is already done
            logger.warning("RecordSkill[%s]: match-or-insert failed", plan_id, exc_info=True)

        logger.info(
            "RecordSkill[%s]: close_reason=%s outcome=%s required_correction=%s composed_from=%s",
            plan_id, close_reason, outcome, required_correction, composed_from,
        )

    async def _match_or_insert(
        self,
        plan_id: str,
        task_text: str,
        task_embedding,
        transcript: str,
        outcome: str,
        composed_from: set,
    ) -> None:
        procedures = await store.current_procedures(self._pool, _SCOPES)
        nearest, best = None, 0.0
        for p in procedures:
            if not p.trigger_embedding or not task_embedding:
                continue
            sim = cosine(task_embedding, p.trigger_embedding)
            if sim > best:
                nearest, best = p, sim
        radius = (nearest.cluster_radius or _MATCH_RADIUS) if nearest else _MATCH_RADIUS
        matched = nearest is not None and best >= radius
        divergence = nearest is not None and nearest.id in composed_from

        if outcome == "failure":
            if matched:
                await store.append_notes(
                    self._pool, nearest.id, [f"A previous attempt at this failed: {task_text[:160]}"]
                )
                logger.info("RecordSkill[%s]: appended failure note to %s", plan_id, nearest.id)
            return

        # success:
        if matched and divergence:
            spec = await generalize.generalize([transcript], [], current_body_text=nearest.render())
            if spec:
                vec = await embedding.embed(spec["trigger_text"])
                await store.new_version(self._pool, nearest.id, spec, vec, [plan_id])
                logger.info("RecordSkill[%s]: re-versioned %s (divergence)", plan_id, nearest.id)
        elif matched:
            await store.ema_update(self._pool, nearest.id, 1.0, task_embedding)
            logger.info("RecordSkill[%s]: reinforced %s (matched, sim=%.2f)", plan_id, nearest.id, best)
        else:
            spec = await generalize.generalize([transcript], [], current_body_text=None)
            if spec:
                vec = await embedding.embed(spec["trigger_text"])
                pid = await store.insert_learned(self._pool, spec, vec, [plan_id])
                logger.info("RecordSkill[%s]: created %s (%r)", plan_id, pid, spec["title"])


def _build_transcript(messages, tool_calls) -> str:
    calls_by_message: dict = {}
    for tc in tool_calls:
        calls_by_message.setdefault(tc["message_id"], []).append(f"{tc['tool_name']} -> {tc['status']}")

    lines: list[str] = []
    for m in messages:
        content = (m["content"] or "").strip()
        if content:
            lines.append(f"{m['role']}: {content}")
        for call in calls_by_message.get(m["message_id"], []):
            lines.append(f"  tool: {call}")

    text = "\n".join(lines)
    if len(text) <= _MAX_TRANSCRIPT_CHARS:
        return text
    return text[:_MAX_TRANSCRIPT_CHARS] + "\n…(truncated)"
