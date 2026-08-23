"""WriteMemory activity — docs/components/memory-slot.md's "Resolved:
Write-Path Construction". Dispatched fire-and-forget from turn.go right
alongside Persist (ExecuteActivity without Get()), top-level turns only
(input.ParentType == "session") — a subagent's own work only enters memory
indirectly, if the parent turn's extraction happens to pick it up.

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
     event_id is a no-op"), so a deterministic `{turn_id}:event:1` makes a
     Temporal-driven retry of this whole activity safe by construction, no
     two-phase write needed.

  2. One event per top-level turn, not "0, 1, or several" — a direct
     consequence of #1 (no extraction step deciding how many candidate
     events to split into).

  3. No participants sent by default. memory_write's own description:
     "Participants are third parties the event is about — never yourself,
     and never the user." memory-slot.md's originally-proposed v1 mapping
     (session_key as a "self" participant, an agent-name as an "agent"
     participant) is exactly the opposite of that — agent-brain resolves
     both the authenticated user and the writing agent's own identity
     automatically, not from a participants entry. A real third-party
     detection mechanism (e.g. "this turn was about someone else") doesn't
     exist yet — out of scope here, same as the already-deferred
     cross-session-linking/entity-resolution open question.
"""

from __future__ import annotations

import logging

from temporalio import activity

from . import agent_brain, ids

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
    async def __call__(self, turn_id: str) -> None:
        rows = await self._pool.fetch(
            "SELECT role, content FROM messages WHERE parent_id = $1 ORDER BY seq", turn_id
        )
        content = _render_transcript(rows)
        if not content:
            logger.info("WriteMemory[%s]: no message content, skipping", turn_id)
            return

        try:
            await agent_brain.call_tool(
                "memory_write",
                {
                    "event_id": f"{turn_id}:event:1",
                    "type": "conversation_turn",
                    "trigger": {"type": "input", "turn_id": turn_id, "content": content},
                },
            )
        except agent_brain.AgentBrainNotConfiguredError:
            logger.info("WriteMemory[%s]: agent-brain not configured, skipping", turn_id)
            return

        logger.info("WriteMemory[%s]: wrote event for session %s", turn_id, ids.session_key_of(turn_id))
