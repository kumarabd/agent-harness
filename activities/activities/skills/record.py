"""RecordSkillOutcome — skill subsystem phase 2
(docs/components/skill-subsystem.md, "Recording").

Dispatched detached at the end of every completed top-level task turn of
moderate/complex complexity. Writes one `skill_candidates` row — the full
trajectory plus its terminal reward — and EMA-updates the `confidence` /
`trigger_embedding` of every procedure that was composed into the turn.

Best-effort: the response is already delivered; a failure here changes
nothing the user sees. Synthesis (phase 3, not built) consumes the
candidates.
"""

from __future__ import annotations

import json
import logging

from temporalio import activity

from .. import ids, plan
from ..types import RecordSkillOutcomeInput
from . import embedding, store

logger = logging.getLogger(__name__)

_MAX_TRANSCRIPT_CHARS = 20_000
_MAX_TASK_TEXT_CHARS = 2_000
# The reason the reason-act loop stops cleanly — the model chose to answer
# rather than being cut off by a limit or a run of failures.
_SUCCESS_STOP_REASONS = {"no_tool_calls"}


class RecordSkillOutcomeActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="RecordSkillOutcome")
    async def __call__(self, input: RecordSkillOutcomeInput) -> None:
        async with self._pool.acquire() as conn:
            messages = await conn.fetch(
                "SELECT role, content, message_id, seq FROM messages WHERE parent_id = $1 ORDER BY seq",
                input.turn_id,
            )
            if not messages:
                logger.info("RecordSkillOutcome[%s]: no messages, skipping", input.turn_id)
                return
            tool_calls = await conn.fetch(
                "SELECT tool_name, status, message_id FROM tool_calls "
                "WHERE parent_id = $1 ORDER BY tool_call_id",
                input.turn_id,
            )
            skill_rows = await conn.fetch(
                "SELECT metadata FROM turn_retrieval WHERE turn_id = $1 AND kind = 'skill'",
                input.turn_id,
            )
            checkpoints = await plan.read(conn, input.turn_id)

        user_messages = [m for m in messages if m["role"] == "user"]
        task_text = (user_messages[0]["content"] or "").strip() if user_messages else ""
        if not task_text:
            logger.info("RecordSkillOutcome[%s]: no user message, skipping", input.turn_id)
            return

        had_tool_error = any(tc["status"] == "error" for tc in tool_calls)
        outcome = (
            "success"
            if input.stop_reason in _SUCCESS_STOP_REASONS and not had_tool_error
            else "failure"
        )
        # A follow-up user message inside the turn is the phase-2 approximation
        # of a mid-task correction. Refined later by a cheap classifier.
        required_correction = len(user_messages) > 1

        composed_from: list[str] = []
        for row in skill_rows:
            meta = json.loads(row["metadata"]) if row["metadata"] else {}
            if meta.get("procedure_id"):
                composed_from.append(meta["procedure_id"])

        transcript = _build_transcript(messages, tool_calls)
        # The plan ledger's final state, prepended — the checkpoints in their
        # final order (with revised/skipped/added steps marked) ARE the
        # effective procedure the run followed; synthesis (generalize.py) reads
        # it as the structured skeleton alongside the raw trajectory
        # (request-pipeline/08-planning.md, "Feeds synthesis").
        plan_final = plan.render_final(checkpoints)
        if plan_final:
            transcript = plan_final + "\n\n" + transcript
        task_embedding = await embedding.embed(task_text)

        try:
            await store.insert_candidate(
                self._pool,
                input.turn_id,
                task_text[:_MAX_TASK_TEXT_CHARS],
                task_embedding,
                transcript,
                outcome,
                required_correction,
                composed_from,
            )
        except Exception:  # noqa: BLE001 - best-effort, the turn is already done
            logger.warning("RecordSkillOutcome[%s]: candidate write failed", input.turn_id, exc_info=True)

        if composed_from:
            reward = (0.5 if required_correction else 1.0) if outcome == "success" else 0.0
            for procedure_id in set(composed_from):
                try:
                    await store.ema_update(
                        self._pool, procedure_id, reward, task_embedding if reward > 0.0 else None
                    )
                except Exception:  # noqa: BLE001
                    logger.warning(
                        "RecordSkillOutcome[%s]: ema_update %s failed",
                        input.turn_id,
                        procedure_id,
                        exc_info=True,
                    )

            # Co-occurrence (phase 4): this turn's procedures × each other, and
            # × the procedures used earlier in the same session.
            try:
                session_key = ids.session_key_of(input.turn_id)
                recent = await store.session_composed_procedure_ids(
                    self._pool, session_key, input.turn_id
                )
                await store.update_cooccurrence(self._pool, list(set(composed_from)), recent, reward)
            except Exception:  # noqa: BLE001
                logger.warning(
                    "RecordSkillOutcome[%s]: co-occurrence update failed", input.turn_id, exc_info=True
                )

        logger.info(
            "RecordSkillOutcome[%s]: outcome=%s required_correction=%s composed_from=%s",
            input.turn_id,
            outcome,
            required_correction,
            composed_from,
        )


def _build_transcript(messages, tool_calls) -> str:
    """The trajectory as text — messages in order, each assistant message's
    tool calls listed right after it with their outcomes."""
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
