"""WriteMemory activity — docs/components/memory-slot.md's "Resolved:
Write-Path Construction". Dispatched fire-and-forget from exactly two
places: coordinator.go's idle-timeout exit (session completion) and
turn.go's hard-compression branch (context compaction).

**Rewritten 2026-08-29 (second revision, same day) — fully stateless, no
watermark.** The original watermark design (`sessions.memory_write_
watermark_turn_seq`, migration 009) shipped only ever-raw message text,
batched as a delta since the last write — meaning it never leveraged LCM's
own compaction at all: if the session had already condensed m1-m25 into a
leaf summary by the time WriteMemory ran, this activity re-read and
re-shipped m1-m25 as raw text anyway, completely blind to the DAG.

The real fix, per direct design discussion: **the event this activity
writes IS the session's current active context** — not a delta computed
against a watermark, and not required to avoid all overlap with a prior
call. Concretely, on every dispatch:
  1. Every currently-active summary for the session (`context_summaries
     WHERE folded_into IS NULL`) — already the cheapest, most-current
     representation of whatever span it covers, by construction; no
     "is this summary wholly new" analysis needed, since an active summary
     is always correct to include as-is.
  2. Every raw message never covered by any leaf summary at all (`covers`
     is immutable once a leaf is created, so this is exactly "content
     compaction hasn't touched yet" — not window-limited the way
     `lcm.assemble()` is, since mining wants everything uncovered, not
     just what fits in a live model call's verbatim window).
  3. Merged, oldest-to-newest, into one transcript: summaries first
     (ordered by `created_at` — compaction always processes the oldest
     uncovered content first, so creation order among currently-active
     nodes tracks their underlying chronological position even after
     folding), then the uncovered raw tail (ordered by `turn_seq`, `seq`).

No watermark, no Postgres write of any kind from this activity — reading
`sessions` for a watermark and writing it back is simply gone. Accepted
cost, explicitly: a session with more than one hard-compaction event will
re-send a merge that substantially overlaps a prior call's merge (whatever
summaries didn't change between the two calls get sent again). Relies on
agent-brain's own extraction pipeline to be dedup-safe against seeing
overlapping/restated content more than once — a property any real memory-
consolidation pipeline needs anyway (a person re-stating something already
said within one real conversation is not a new problem this design
introduces). `event_id` is a hash of the merged content itself, not a
turn_seq range — genuinely idempotent (identical content across two calls,
whether a real Temporal retry or two triggers landing back to back with
nothing new in between, produces the identical event_id, so agent-brain's
own "re-submitting the same event_id is a no-op" contract absorbs it for
free) without needing any session-side state to compute.

Deliberately still simpler than memory-slot.md originally specified, in
the same two ways established earlier (see git history for the original
three-way list) — no separate LLM extraction step client-side, no
participants sent by default. Both still apply unchanged; see agent-brain's
own memory_write tool contract (internal/mcp/tools_events.go) for why.
"""

from __future__ import annotations

import hashlib
import logging

from temporalio import activity

from . import agent_brain

logger = logging.getLogger(__name__)


class WriteMemoryActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="WriteMemory")
    async def __call__(self, session_key: str) -> None:
        # Every currently-active summary — already the cheapest, most-
        # current representation of whatever it covers. created_at ASC
        # tracks chronological position among active nodes even after
        # folding (see module docstring).
        summary_rows = await self._pool.fetch(
            "SELECT content FROM context_summaries "
            "WHERE session_key = $1 AND folded_into IS NULL ORDER BY created_at",
            session_key,
        )

        # A message counts as "covered" the moment ANY leaf's covers ever
        # included it — covers is immutable once a leaf is created, so this
        # doesn't care whether that leaf has since been folded further; a
        # message covered by a folded leaf is still represented (at
        # whatever level) by one of the active summaries above, not by its
        # own raw text.
        covered_rows = await self._pool.fetch(
            "SELECT unnest(covers) AS message_id FROM context_summaries "
            "WHERE session_key = $1 AND kind = 'leaf'",
            session_key,
        )
        covered_ids = {row["message_id"] for row in covered_rows}

        message_rows = await self._pool.fetch(
            """
            SELECT m.message_id, m.role, m.content, m.seq, t.turn_id, t.turn_seq
            FROM turns t
            JOIN messages m ON m.parent_id = t.turn_id
            WHERE t.parent_id = $1 AND t.parent_type = 'session'
            ORDER BY t.turn_seq, m.seq
            """,
            session_key,
        )
        uncovered_rows = [row for row in message_rows if row["message_id"] not in covered_ids]

        lines = [f"[summary] {row['content']}" for row in summary_rows if row["content"]]
        lines += [f"{row['role']}: {row['content']}" for row in uncovered_rows if row["content"]]
        content = "\n".join(lines)
        if not content:
            logger.info("WriteMemory[%s]: nothing to write (no active summaries, no uncovered message content)", session_key)
            return

        # Purely informational on agent-brain's side (trigger.content is
        # the only thing the mining pipeline actually reads) — the most
        # recent real turn in the session, more meaningful than session_key
        # in a field literally named turn_id. Falls back to session_key
        # only in the practically-impossible case of summaries existing
        # with zero messages ever recorded.
        turn_id = message_rows[-1]["turn_id"] if message_rows else session_key

        # Content hash, not a turn_seq range — see module docstring. Same
        # content in, same event_id out, with no session-side state needed
        # to compute it.
        content_hash = hashlib.sha256(content.encode()).hexdigest()[:16]

        try:
            await agent_brain.call_tool(
                "memory_write",
                {
                    "event_id": f"{session_key}:memory-write:{content_hash}",
                    "type": "conversation_turn",
                    "trigger": {"type": "input", "turn_id": turn_id, "content": content},
                },
            )
        except agent_brain.AgentBrainNotConfiguredError:
            logger.info("WriteMemory[%s]: agent-brain not configured, skipping", session_key)
            return

        logger.info(
            "WriteMemory[%s]: wrote event (%d active summaries, %d uncovered messages, hash=%s)",
            session_key, len(summary_rows), len(uncovered_rows), content_hash,
        )
