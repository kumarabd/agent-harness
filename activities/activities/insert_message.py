"""InsertMessage activity — the one place content still crosses an activity
input boundary under the reference-passing contract.

Real design (docs/components/temporal-workflow.md, "Resolved: Reference/ID
Schema"): the inbound message write moves from end-of-turn to start-of-turn —
ModelCall's first call needs the inbound message already in Postgres to read.
This activity is called once, at the start of a turn, with the user's message
from the coordinator's signal payload (already durable via Temporal's own
signal history, so this isn't a second copy of anything, just the first
Postgres write of it).

On is_turn_start=True, this activity also inserts the `turns` row itself
(status='running') — per the read/write table in
docs/components/state-layer.md ("Turn workflow, via its own activities —
inserts the row when the turn starts"), this is the natural point to do it:
the same activity call that's already the turn's first real write.

Subagent case: a subagent's inbound "message" is really the parent's tool-call
argument (e.g. {"prompt": "..."}), which the workflow never holds under the
reference-passing contract — only ModelCall (which wrote that tool_calls row)
has it. So for parent_type == "turn", this activity ignores `input.message`
entirely and instead reads its own kickoff content from
`tool_calls.arguments WHERE tool_call_id = turn_id` (a subagent's turn_id IS
its tool_call_id, per the ID scheme) — no content needs to flow through the
workflow to get it there.

messages.seq is computed here (MAX(seq)+1 within the turn), not passed in —
decoupled from ModelCall's ContextSeq, which is a separate fixture-lookup
index that only coincidentally starts at the same value.
"""

from __future__ import annotations

import json
import logging

from temporalio import activity

from .types import InsertMessageInput

logger = logging.getLogger(__name__)


class InsertMessageActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="InsertMessage")
    async def __call__(self, input: InsertMessageInput) -> None:
        async with self._pool.acquire() as conn:
            async with conn.transaction():
                if input.is_turn_start and input.parent_type == "session":
                    # sessions.session_key is what session_filesystem_leases'
                    # FK requires (leases.py) — no Gateway component exists
                    # yet in this codebase to upsert a real row on first
                    # contact (state-layer.md's documented owner), so this is
                    # the closest real "first contact with a session" write
                    # available. platform/channel_id are genuinely unknown at
                    # this layer — 'unknown' is an honest placeholder, not a
                    # fabricated real value; a real Gateway replaces this
                    # entirely rather than this needing to guess correctly.
                    await conn.execute(
                        "INSERT INTO sessions (session_key, platform, channel_id) "
                        "VALUES ($1, 'unknown', 'unknown') ON CONFLICT (session_key) DO NOTHING",
                        input.parent_id,
                    )

                if input.is_turn_start:
                    await conn.execute(
                        "INSERT INTO turns (turn_id, parent_id, parent_type, turn_seq, status) "
                        "VALUES ($1, $2, $3, $4, 'running') ON CONFLICT (turn_id) DO NOTHING",
                        input.turn_id,
                        input.parent_id,
                        input.parent_type,
                        input.turn_seq,
                    )

                if input.is_turn_start and input.parent_type == "turn":
                    # Subagent: derive content from the parent's own
                    # tool_calls write rather than input.message (which the
                    # workflow can't have populated).
                    row = await conn.fetchrow(
                        "SELECT arguments FROM tool_calls WHERE tool_call_id = $1", input.turn_id
                    )
                    if row is None:
                        raise RuntimeError(
                            f"InsertMessage: subagent turn {input.turn_id!r} has no corresponding "
                            "tool_calls row to derive its kickoff content from"
                        )
                    arguments = json.loads(row["arguments"])
                    role, content = "user", str(arguments.get("prompt", ""))
                else:
                    role, content = input.message.role, input.message.content

                # seq computed inline (MAX+1) — one round-trip, and no window
                # between the read and the insert. Safe here: the transaction
                # plus the fact that a turn's messages are written serially.
                await conn.execute(
                    "INSERT INTO messages (parent_id, role, content, seq) "
                    "VALUES ($1, $2, $3, "
                    "        (SELECT COALESCE(MAX(seq), -1) + 1 FROM messages WHERE parent_id = $1))",
                    input.turn_id,
                    role,
                    content,
                )
        logger.info("InsertMessage[%s]: %s: %r", input.turn_id, role, content[:80])
