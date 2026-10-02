"""SeedChildSessionContext activity — docs/components/gateway.md, "Resolved:
Multi-Session Channels", the LCM-copy genesis mechanism. Runs once, at a
child session's CoordinatorWorkflow's very first execution (coordinator.go
only calls this when CoordinatorInput.ParentSessionKey is set at all, which
the Gateway only ever sets on genuine genesis — its own sessions-table
INSERT's RowsAffected check, not re-derived here). Duplicates the parent
session's CURRENT fully-assembled context — lcm.assemble()'s own
summary-DAG-plus-verbatim-window output, not just context_summaries rows
alone, since assemble() is the one place both are actually combined — into a
single new leaf-kind context_summaries row under the CHILD session. A real
duplicate, not a shared reference: the child evolves independently
afterward via its own future compact() calls, same as any other session.

covers is deliberately empty ('{}') — this doesn't summarize any of the
CHILD's own messages (it has none yet), so there's nothing under this
session's own turn tree for a normal leaf summary's covers array to
reference.
"""

from __future__ import annotations

import logging

from temporalio import activity

from . import lcm

logger = logging.getLogger(__name__)


class SeedChildSessionContextActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="SeedChildSessionContext")
    async def __call__(self, parent_session_key: str, child_session_key: str) -> None:
        async with self._pool.acquire() as conn:
            # Empty system_prompt: this call only wants the summary+verbatim
            # history assemble() combines, not a prompt-wrapped conversation
            # ready for ModelCall — the leading system-role entry it always
            # prepends gets dropped below rather than copied into the child.
            conversation, _ = await lcm.assemble(conn, parent_session_key, system_prompt="")
            entries = conversation[1:]
            if not entries:
                logger.info(
                    "seed_child_session_context[%s -> %s]: parent has no context yet, nothing to seed",
                    parent_session_key,
                    child_session_key,
                )
                return

            rendered = "\n".join(_render_entry(e) for e in entries)
            await conn.execute(
                "INSERT INTO context_summaries (session_key, kind, covers, content, token_count) "
                "VALUES ($1, 'leaf', '{}', $2, $3)",
                child_session_key,
                rendered,
                lcm.estimate_tokens(rendered),
            )
            logger.info(
                "seed_child_session_context[%s -> %s]: seeded %d entries",
                parent_session_key,
                child_session_key,
                len(entries),
            )


def _render_entry(entry: dict) -> str:
    role = entry.get("role", "unknown")
    if role == "tool":
        return f"tool result: {entry.get('content') or ''}"
    content = entry.get("content")
    tool_calls = entry.get("tool_calls")
    if tool_calls:
        names = ", ".join(tc["function"]["name"] for tc in tool_calls)
        suffix = f" [called: {names}]"
    else:
        suffix = ""
    return f"{role}: {content or ''}{suffix}"
