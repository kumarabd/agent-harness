"""StatusPing activity — docs/components/turn-pipeline.md, "Progress watchdog".

TurnWorkflow's watchdog goroutine calls this when its durable timer fired with
no user-visible delivery since the last one. It reads the turn's current state
(latest tool_calls row, iteration count, any error), builds a one-liner from a
fixed template, writes it as a transient turn_status_pings row, and returns the
line so the workflow can push it out through the platform's delivery queue.

The ping is NOT transcript: it never enters `messages`, never enters LCM
context. It is a third liveness concept, separate from Temporal's activity
heartbeat (worker liveness) and the model's own `message` (content).

`reason='wedged'` is the coordinator's fallback path — the TurnWorkflow child
hit its WorkflowRunTimeout and failTurn could not run, so the coordinator
(which holds the child future) asks for a fixed "still working, something may
be stuck" line instead of a progress-derived one.
"""

from __future__ import annotations

import json
import logging

from temporalio import activity

logger = logging.getLogger(__name__)

_WEDGED_LINE = "This is taking longer than expected — still working on it."
_GENERIC_LINE = "Still working on this…"

# _describe_call — docs/components/gateway/first-party-plan.md, "every
# progress frame". Different tools name their main argument differently
# (shell_exec's "command", search_memory/discover_tools' "query",
# spawn_subagent's "prompt"); tried in priority order, first real string
# wins. Deliberately generic rather than a per-tool-name switch — a new tool
# that happens to use one of these key names is covered for free, and one
# that doesn't just falls back to the bare tool name, same as before this
# existed.
_ARG_KEYS_BY_PRIORITY = ("query", "command", "prompt", "content")


def _describe_call(tool_name: str, arguments: dict) -> str:
    for key in _ARG_KEYS_BY_PRIORITY:
        val = arguments.get(key)
        if isinstance(val, str) and val.strip():
            snippet = val.strip()
            if len(snippet) > 60:
                snippet = snippet[:57] + "..."
            return f'{tool_name} — "{snippet}"'
    return tool_name


class StatusPingActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="StatusPing")
    async def __call__(self, turn_id: str, reason: str = "") -> str:
        if reason == "wedged":
            line = _WEDGED_LINE
        else:
            line = await self._progress_line(turn_id)

        row = await self._pool.fetchrow(
            "INSERT INTO turn_status_pings (turn_id, seq, content, reason) "
            "VALUES ($1, COALESCE((SELECT max(seq) + 1 FROM turn_status_pings WHERE turn_id = $1), 0), $2, $3) "
            "RETURNING seq",
            turn_id,
            line,
            reason,
        )
        logger.info("StatusPing[%s]: seq=%s reason=%r %r", turn_id, row["seq"], reason, line)
        return line

    async def _progress_line(self, turn_id: str) -> str:
        # Iteration count — one assistant message per reason-act pass.
        steps = await self._pool.fetchval(
            "SELECT count(*) FROM messages WHERE parent_id = $1 AND role = 'assistant'",
            turn_id,
        )
        latest = await self._pool.fetchrow(
            "SELECT tc.tool_name, tc.status, tc.arguments FROM tool_calls tc "
            "JOIN messages m ON m.message_id = tc.message_id "
            "WHERE tc.parent_id = $1 "
            "ORDER BY m.seq DESC, tc.tool_call_id DESC LIMIT 1",
            turn_id,
        )
        if latest is None:
            return _GENERIC_LINE
        if latest["status"] == "error":
            return f"…hit a snag on {latest['tool_name']}, retrying."
        n = (steps or 0) + 1
        arguments = json.loads(latest["arguments"]) if latest["arguments"] else {}
        return f"Working on it — {_describe_call(latest['tool_name'], arguments)}, step {n}."
