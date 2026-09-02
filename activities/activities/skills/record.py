"""RecordSkillOutcome — skill subsystem phase 2
(docs/components/skill-subsystem.md, "Recording") + episode-scoped
(docs/components/episode-lifecycle.md).

Fires ONCE when an episode closes (plan complete, superseded, idle-exit, or a
subagent turn ending) — not per turn. Writes one `skill_candidates` row for
the whole multi-turn trajectory plus its terminal reward, and EMA-updates the
`confidence` / `trigger_embedding` of every procedure that was composed into
the episode.

Best-effort: the response is already delivered; a failure here changes
nothing the user sees. Synthesis (phase 3) consumes the candidates.
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
# The loop stop reason that means the model chose to answer rather than being
# cut off by a limit or a run of failures.
_SUCCESS_STOP_REASONS = {"no_tool_calls"}
# The Deliberate lane (docs/components/lane-model.md): only episodes of this
# shape are worth learning a procedure from. Only Deliberate turns open an
# episode at all, so in practice this always passes for an episode that exists
# — kept as a defensive restatement of the lane rule.
_RECORD_INTENTS = {"task", "question"}
_RECORD_COMPLEXITIES = {"moderate", "complex"}


class RecordSkillOutcomeActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="RecordSkillOutcome")
    async def __call__(self, input: RecordSkillOutcomeInput) -> None:
        episode_id = input.episode_id
        async with self._pool.acquire() as conn:
            episode = await conn.fetchrow(
                "SELECT session_key, intent, complexity, close_reason, last_stop_reason "
                "FROM episodes WHERE episode_id = $1",
                episode_id,
            )
            # An episode row is expected, but a subagent's "episode" that was
            # never opened (defensive) or a race falls back to a lone-turn read.
            intent = episode["intent"] if episode else "task"
            complexity = episode["complexity"] if episode else "moderate"
            close_reason = (episode["close_reason"] if episode else "") or ""
            last_stop_reason = (episode["last_stop_reason"] if episode else "") or input.stop_reason

            if intent not in _RECORD_INTENTS or complexity not in _RECORD_COMPLEXITIES:
                logger.info(
                    "RecordSkillOutcome[%s]: intent=%s complexity=%s — nothing worth learning",
                    episode_id, intent, complexity,
                )
                return

            # The whole episode trajectory: every turn that belongs to it, in
            # order. Nested-subagent turns have their own episode_id, so they
            # are excluded here and recorded under their own episode.
            messages = await conn.fetch(
                "SELECT m.role, m.content, m.message_id, m.seq "
                "FROM messages m JOIN turns t ON m.parent_id = t.turn_id "
                "WHERE t.episode_id = $1 "
                "ORDER BY t.turn_seq NULLS FIRST, t.started_at, m.seq",
                episode_id,
            )
            if not messages:
                logger.info("RecordSkillOutcome[%s]: no messages, skipping", episode_id)
                return
            tool_calls = await conn.fetch(
                "SELECT tc.tool_name, tc.status, tc.message_id "
                "FROM tool_calls tc JOIN turns t ON tc.parent_id = t.turn_id "
                "WHERE t.episode_id = $1 ORDER BY tc.tool_call_id",
                episode_id,
            )
            skill_rows = await conn.fetch(
                "SELECT metadata FROM turn_retrieval WHERE episode_id = $1 AND kind = 'skill'",
                episode_id,
            )
            checkpoints = await plan.read(conn, episode_id)

        user_messages = [m for m in messages if m["role"] == "user"]
        task_text = (user_messages[0]["content"] or "").strip() if user_messages else ""
        if not task_text:
            logger.info("RecordSkillOutcome[%s]: no user message, skipping", episode_id)
            return

        had_tool_error = any(tc["status"] == "error" for tc in tool_calls)
        clean_stop = last_stop_reason in _SUCCESS_STOP_REASONS and not had_tool_error
        # plan_complete is unambiguous success. idle / turn_end succeed when the
        # last turn ended cleanly. superseded means the user pivoted away before
        # the plan finished — a weak/failed run.
        if close_reason == "plan_complete":
            outcome = "success"
        elif close_reason in ("idle", "turn_end", "") and clean_stop:
            outcome = "success"
        else:
            outcome = "failure"

        # More than one user message across the episode: clarifying dialogue or
        # a correction. The phase-2 approximation of "required_correction";
        # refined later (episode-lifecycle.md, Deferred).
        required_correction = len(user_messages) > 1

        composed_from: list[str] = []
        for row in skill_rows:
            meta = json.loads(row["metadata"]) if row["metadata"] else {}
            if meta.get("procedure_id"):
                composed_from.append(meta["procedure_id"])

        transcript = _build_transcript(messages, tool_calls)
        plan_final = plan.render_final(checkpoints)
        if plan_final:
            transcript = plan_final + "\n\n" + transcript
        task_embedding = await embedding.embed(task_text)

        try:
            await store.insert_candidate(
                self._pool,
                episode_id,
                task_text[:_MAX_TASK_TEXT_CHARS],
                task_embedding,
                transcript,
                outcome,
                required_correction,
                composed_from,
            )
        except Exception:  # noqa: BLE001 - best-effort, the episode is already done
            logger.warning("RecordSkillOutcome[%s]: candidate write failed", episode_id, exc_info=True)

        if composed_from:
            reward = (0.5 if required_correction else 1.0) if outcome == "success" else 0.0
            for procedure_id in set(composed_from):
                try:
                    await store.ema_update(
                        self._pool, procedure_id, reward, task_embedding if reward > 0.0 else None
                    )
                except Exception:  # noqa: BLE001
                    logger.warning(
                        "RecordSkillOutcome[%s]: ema_update %s failed", episode_id, procedure_id, exc_info=True
                    )

            try:
                session_key = ids.session_key_of(episode_id)
                recent = await store.session_composed_procedure_ids(self._pool, session_key, episode_id)
                await store.update_cooccurrence(self._pool, list(set(composed_from)), recent, reward)
            except Exception:  # noqa: BLE001
                logger.warning(
                    "RecordSkillOutcome[%s]: co-occurrence update failed", episode_id, exc_info=True
                )

        logger.info(
            "RecordSkillOutcome[%s]: close_reason=%s outcome=%s required_correction=%s composed_from=%s",
            episode_id,
            close_reason,
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
