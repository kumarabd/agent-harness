"""WriteMemory activity — docs/components/memory-slot.md's "Resolved:
Write-Path Construction". Dispatched fire-and-forget from exactly two
places (2026-08-29 correction — see below): coordinator.go's idle-timeout
exit (session completion) and turn.go's hard-compression branch (context
compaction). No longer dispatched per top-level turn.

Deliberately simpler than memory-slot.md originally specified, in three
concrete ways — each found by checking agent-brain's actual, current MCP
tool contract directly (internal/mcp/tools_events.go), not assumed from the
design doc:

  1. No separate LLM extraction step, no staging table. memory_write's own
     description is explicit: "This does not extract structured knowledge...
     Do not try to pre-structure or pre-interpret the content; write the
     complete raw trigger text, since it's the only input the mining
     pipeline sees." Agent-brain's own downstream mining pipeline does
     extraction; a caller-side extraction model would be redundant work
     fighting the actual contract. This also removes the retry-safety
     problem the staging table existed to solve — memory_write's event_id
     is already a caller-supplied idempotency key ("Re-submitting the same
     event_id is a no-op"), so a deterministic event_id (see below) makes a
     Temporal-driven retry of this whole activity safe by construction, no
     two-phase write needed.

  2. No participants sent by default. memory_write's own description:
     "Participants are third parties the event is about — never yourself,
     and never the user." memory-slot.md's originally-proposed v1 mapping
     (session_key as a "self" participant, an agent-name as an "agent"
     participant) is exactly the opposite of that — agent-brain resolves
     both the authenticated user and the writing agent's own identity
     automatically, not from a participants entry. A real third-party
     detection mechanism (e.g. "this turn was about someone else") doesn't
     exist yet — out of scope here, same as the already-deferred
     cross-session-linking/entity-resolution open question.

  3. **2026-08-29 correction**: not one event per top-level turn anymore
     either. Agent-brain's own mining-pipeline-redesign contract is
     explicit — "Don't call it per-turn or per-tool-call the way the old
     model did... it fires at natural harness-lifecycle boundaries —
     session completion, context compaction." This activity now reads by
     session_key + a Postgres watermark (sessions.memory_write_watermark_turn_seq,
     migrations/009_memory_write_watermark.sql) rather than one turn's own
     messages, batching everything accumulated since the last successful
     write into a single memory_write call, and only advances the
     watermark after a confirmed-successful call — a retry (or a call that
     found agent-brain unconfigured) naturally re-reads the same range next
     time, no separate staging table needed for that either.
"""

from __future__ import annotations

import logging

from temporalio import activity

from . import agent_brain

logger = logging.getLogger(__name__)


def _render_transcript(rows) -> str:
    """The complete raw trigger text memory_write's own description asks
    for — conversational content only (user/assistant message text), not
    tool-call mechanics (raw arguments/results), which aren't the
    conversational substance a memory-mining pipeline needs."""
    lines = [f"{row['role']}: {row['content']}" for row in rows if row["content"]]
    return "\n".join(lines)


class WriteMemoryActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="WriteMemory")
    async def __call__(self, session_key: str) -> None:
        watermark = await self._pool.fetchval(
            "SELECT memory_write_watermark_turn_seq FROM sessions WHERE session_key = $1", session_key
        )
        watermark = watermark or 0

        # turn_seq is only meaningful for a top-level turn (parent_type =
        # 'session') — state-layer.md's own schema note. Ordered by
        # (turn_seq, seq) so a multi-turn batch's transcript reads in real
        # chronological order, not just insertion order.
        rows = await self._pool.fetch(
            """
            SELECT m.role, m.content, t.turn_id, t.turn_seq
            FROM turns t
            JOIN messages m ON m.parent_id = t.turn_id
            WHERE t.parent_id = $1 AND t.parent_type = 'session' AND t.turn_seq > $2
            ORDER BY t.turn_seq, m.seq
            """,
            session_key,
            watermark,
        )
        if not rows:
            logger.info("WriteMemory[%s]: nothing new since watermark %s, skipping", session_key, watermark)
            return

        max_turn_seq = max(row["turn_seq"] for row in rows)
        # The last turn in the batch — a real, valid, addressable id for the
        # trigger's own turn_id field, more meaningful than reusing
        # session_key in a field literally named turn_id. Purely
        # informational on agent-brain's side (trigger.content is the only
        # thing the mining pipeline actually reads); doesn't need to
        # identify every turn in the batch.
        last_turn_id = next(row["turn_id"] for row in rows if row["turn_seq"] == max_turn_seq)

        content = _render_transcript(rows)
        if not content:
            # Real content exists at the row level (tool-only turns, no
            # user/assistant text) but nothing worth mining. Still advance
            # the watermark — otherwise a session that's all tool calls for
            # a while keeps re-scanning (and re-finding nothing) forever
            # instead of moving past it.
            await self._pool.execute(
                "UPDATE sessions SET memory_write_watermark_turn_seq = $2 WHERE session_key = $1",
                session_key,
                max_turn_seq,
            )
            logger.info("WriteMemory[%s]: no message content through turn_seq %s, watermark advanced, nothing sent", session_key, max_turn_seq)
            return

        try:
            await agent_brain.call_tool(
                "memory_write",
                {
                    # Deterministic per batch, not per call attempt — a
                    # Temporal-driven retry of this whole activity
                    # recomputes the identical event_id for the identical
                    # range (same watermark in, same max_turn_seq out),
                    # so memory_write's own "re-submitting the same
                    # event_id is a no-op" contract makes the retry safe
                    # without needing a separate idempotency mechanism here.
                    "event_id": f"{session_key}:memory-write:{max_turn_seq}",
                    "type": "conversation_turn",
                    "trigger": {"type": "input", "turn_id": last_turn_id, "content": content},
                },
            )
        except agent_brain.AgentBrainNotConfiguredError:
            # Deliberately do NOT advance the watermark here — nothing was
            # actually written. If agent-brain gets configured later, the
            # next trigger sends the full accumulated range in one write,
            # which is correct, not a bug (matches this project's existing
            # "degrade gracefully, don't silently lose the work" posture
            # for every other optional dependency).
            logger.info("WriteMemory[%s]: agent-brain not configured, skipping (watermark not advanced)", session_key)
            return

        await self._pool.execute(
            "UPDATE sessions SET memory_write_watermark_turn_seq = $2 WHERE session_key = $1",
            session_key,
            max_turn_seq,
        )
        logger.info("WriteMemory[%s]: wrote event covering turn_seq %s..%s", session_key, watermark, max_turn_seq)
