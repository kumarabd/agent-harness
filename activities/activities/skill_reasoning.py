"""SummarizeReasoningTurn — docs/05-architecture-domain-control-loops.md.

A skill's own scoped reasoning turn (skills.RunReasoningTurn, Go) runs the
exact same reason-act loop TurnWorkflow does, then needs to report a short,
structured outcome back to the skill's own Go code to close out with —
never raw message content (the reference-passing contract). This activity is
the one place that's allowed to actually read the reasoning turn's messages
(Python, not workflow code), and it only ever hands back a short summary, the
same class of crossing CloseSkillCall's own `result` param already makes.

stop_reason comes from the Go side directly (RunReasonActLoopResult.StopReason)
rather than being re-derived here from turns.status — the loop already knows
exactly why it stopped; re-deriving that from a 4-value status column would
be strictly lossier.
"""

from __future__ import annotations

import logging

from temporalio import activity

from .types import ReasoningTurnOutcome

logger = logging.getLogger(__name__)

_MAX_SUMMARY_CHARS = 500

# docs/05-architecture-domain-control-loops.md's own resolution: "blocked"
# with nothing pending is a degenerate case for a scoped reasoning turn —
# nothing can ever resolve it inside a bounded skill invocation, since
# nobody durably answers a question that was never actually asked via
# ask_user. Every non-clean stop maps to "error"; only a genuine "done" is
# "ok", and an outer interrupt is "cancelled".
_STATUS_BY_STOP_REASON = {
    "no_tool_calls": "ok",
    "cancelled_by_user": "cancelled",
}


class SummarizeReasoningTurnActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="SummarizeReasoningTurn")
    async def __call__(self, args: dict) -> ReasoningTurnOutcome:
        reasoning_turn_id: str = args["reasoning_turn_id"]
        stop_reason: str = args["stop_reason"]

        status = _STATUS_BY_STOP_REASON.get(stop_reason, "error")

        last_assistant = await self._pool.fetchval(
            "SELECT content FROM messages WHERE parent_id = $1 AND role = 'assistant' "
            "ORDER BY seq DESC LIMIT 1",
            reasoning_turn_id,
        )
        summary = (last_assistant or "").strip()[:_MAX_SUMMARY_CHARS]

        logger.info("SummarizeReasoningTurn[%s]: stop_reason=%s status=%s", reasoning_turn_id, stop_reason, status)
        return ReasoningTurnOutcome(status=status, summary=summary)
