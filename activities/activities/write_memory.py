"""WriteMemory activity — docs/components/memory-slot.md's "Resolved:
Write-Path Construction". Dispatched fire-and-forget from exactly two
places: coordinator.go's idle-timeout exit (session completion) and
turn.go's hard-compression branch (context compaction).

**Stateless, no watermark** (established 2026-08-29, unchanged by the
2026-09-13 retain rewrite below): the event this activity writes IS the
session's current active context, not a delta computed against a watermark.
Concretely, on every dispatch:
  1. Every currently-active summary for the session (`context_summaries
     WHERE folded_into IS NULL`) — already the cheapest, most-current
     representation of whatever span it covers, by construction; no
     "is this summary wholly new" analysis needed, since an active summary
     is always correct to include as-is.
  2. Every raw message never covered by any leaf summary at all (`covers`
     is immutable once a leaf is created, so this is exactly "content
     compaction hasn't touched yet" — not window-limited the way
     `lcm.assemble()` is, since retain wants everything uncovered, not
     just what fits in a live model call's verbatim window).
  3. Merged, oldest-to-newest, into one transcript: summaries first
     (ordered by `created_at` — compaction always processes the oldest
     uncovered content first, so creation order among currently-active
     nodes tracks their underlying chronological position even after
     folding), then the uncovered raw tail (ordered by `turn_seq`, `seq`).

This merge-construction logic is independent of the wire format — deciding
*what session content is worth persisting* doesn't change when the backend
tool does — so it survives unchanged across the rewrite below.

**Rewritten 2026-09-13 — agent-brain's memory_write is gone, calls
memory_retain instead.** The old `memory_write` contract (event_id/type/
trigger/platform envelope) doesn't exist on the new retain MCP server at
all: `memory_retain(bank_id, text, mention_time?)` is a flat call with no
envelope. `event_id`, `type`, `trigger`, and `platform` are all dropped —
none has a home in the new schema (platform in particular has no downstream
consumer anymore either; nothing on agent-brain's side extracts from it).

**Real idempotency loss, not just a simplification**: the old `event_id`
was a hash of the merged content — a genuine, load-bearing dedup key (a
Temporal retry or two back-to-back triggers with nothing new in between
produced the identical event_id, so agent-brain's own "re-submitting the
same event_id is a no-op" contract absorbed it for free). `memory_retain`
has no equivalent — a retried call re-runs real fact/entity extraction on
the same text, a genuine duplicate, not a no-op. There is no client-side
mitigation for the "two triggers overlap because nothing changed between
them" case (accepted, same as before — agent-brain's own extraction has to
tolerate seeing restated content, same as a person actually repeating
themselves in conversation). The retry-storm case IS mitigated: turn.go's
WriteMemoryWorkflow now caps retries at 3 attempts (was unlimited, safe
only under the old free-no-op contract) rather than leaving a transient
failure to retry indefinitely, each attempt a real duplicate.

Deliberately still simpler than memory-slot.md originally specified, in
the same way established earlier (see git history) — no separate LLM
extraction step client-side. No `participants` field either — the new
schema has no field for it at all (bank_id is the entire tenant-scoping
mechanism); per-speaker attribution still rides inside `text` itself via
`_attributed_line` below.
"""

from __future__ import annotations

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
            SELECT m.message_id, m.role, m.content, m.speaker_id, m.seq, m.created_at, t.turn_id, t.turn_seq
            FROM turns t
            JOIN messages m ON m.parent_id = t.turn_id
            WHERE t.parent_id = $1 AND t.parent_type = 'session'
            ORDER BY t.turn_seq, m.seq
            """,
            session_key,
        )
        uncovered_rows = [row for row in message_rows if row["message_id"] not in covered_ids]

        # speaker_id is only ever set on a real human message (see
        # insert_message.py) — enrichment on top of role, never a
        # substitute for it. role alone is always accurate (it's NOT NULL);
        # speaker_id is shown additionally when known, not in its place.
        def _attributed_line(row):
            label = row["role"]
            if row["speaker_id"]:
                label = f"{label} ({row['speaker_id']})"
            return f"{label}: {row['content']}"

        lines = [f"[summary] {row['content']}" for row in summary_rows if row["content"]]
        lines += [_attributed_line(row) for row in uncovered_rows if row["content"]]
        content = "\n".join(lines)
        if not content:
            logger.info("WriteMemory[%s]: nothing to write (no active summaries, no uncovered message content)", session_key)
            return

        payload = {"bank_id": agent_brain.retain_bank_id(), "text": content}
        # Most recent real message's own timestamp — more meaningful than "now" for a
        # merge that may include content written well before this dispatch fired.
        # Omitted (server defaults to now()) only in the practically-impossible case of
        # summaries existing with zero messages ever recorded.
        if message_rows:
            payload["mention_time"] = message_rows[-1]["created_at"].isoformat()

        try:
            await agent_brain.call_retain_tool("memory_retain", payload)
        except agent_brain.AgentBrainNotConfiguredError:
            logger.info("WriteMemory[%s]: agent-brain retain server not configured, skipping", session_key)
            return

        logger.info(
            "WriteMemory[%s]: retained (%d active summaries, %d uncovered messages)",
            session_key, len(summary_rows), len(uncovered_rows),
        )
