"""A skill workflow's two activities — docs/05-architecture-domain-control-loops.md,
docs/components/turn-pipeline.md ("Skills"). Everything content-bearing about
a skill invocation lives in Postgres: SkillWorkflowInput carries only IDs
(reference-passing contract, components/temporal-workflow.md), so a skill
workflow reads its own real arguments back via ReadSkillCallArguments, and
closes its own tool_calls row out itself via CloseSkillCall before returning
— the same self-close-out convention UserInputRequestWorkflow already follows
for CloseUserInput/DenyToolCall (user_input.go), not something the parent
TurnWorkflow does after the child workflow completes.
"""

from __future__ import annotations

import json
import logging

from temporalio import activity

logger = logging.getLogger(__name__)


class ReadSkillCallArgumentsActivity:
    """A skill workflow's only way to see its real input — SkillWorkflowInput
    on the Go side carries just the tool_call_id."""

    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="ReadSkillCallArguments")
    async def __call__(self, tool_call_id: str) -> dict:
        row = await self._pool.fetchrow(
            "SELECT arguments FROM tool_calls WHERE tool_call_id = $1", tool_call_id
        )
        if row is None:
            raise RuntimeError(f"ReadSkillCallArguments: no tool_calls row found for {tool_call_id!r}")
        return json.loads(row["arguments"]) if row["arguments"] else {}


class CloseSkillCallActivity:
    """Closes out a skill's tool_calls row in exactly one of the same three
    terminal states any other tool call reaches ('ok' | 'error' | 'cancelled')
    — called by the skill workflow itself, on every one of its own exit
    paths, exactly like CloseUserInput/DenyToolCall are called by
    UserInputRequestWorkflow rather than by whoever started it."""

    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="CloseSkillCall")
    async def __call__(
        self, tool_call_id: str, status: str, result: dict | None, reason: str, side_effect: str
    ) -> None:
        await self._pool.execute(
            "UPDATE tool_calls SET status = $2, result = $3, reason = $4, side_effect = $5, "
            "completed_at = now() WHERE tool_call_id = $1",
            tool_call_id,
            status,
            json.dumps(result) if result is not None else None,
            reason or None,
            side_effect or None,
        )
        logger.info("CloseSkillCall[%s]: status=%s", tool_call_id, status)
