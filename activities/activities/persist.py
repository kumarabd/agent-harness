"""Persist activity — narrowed 2026-08-14 for the reference-passing contract.

Real design: previously wrote the whole turn's messages/tool_calls in one
end-of-turn transaction. Under the reference-passing contract
(docs/components/temporal-workflow.md, docs/components/state-layer.md), those
writes moved earlier and elsewhere — the start-of-turn message insert
(insert_message.py) and every ModelCall/ToolCall write incrementally as the
turn progresses. There's no content left for Persist to batch; its only
remaining job is marking the turn's own row complete.

plan_id (added for a Deliberate subagent's own task-run, turn.go's
"task-run resolution"): InsertMessage writes turns.plan_id once at turn
start, from TurnInput.PlanID as it stood *before* ClassifyRequest ran. A
subagent that opens its own fresh task-run (planID == its own turn_id,
decided only after classify) has no way to get that value into the
start-of-turn write — it doesn't exist yet. This activity is the turn's own
end-of-turn write, so it's the natural place to fold the value in once it's
known, per turn.go's "recorded at turn end" comment. `plan_id=""` (every
call site where the turn never resolved a fresh plan id) is a no-op —
NULLIF collapses it to NULL and COALESCE keeps whatever InsertMessage
already wrote.
"""

from __future__ import annotations

import logging

from temporalio import activity

logger = logging.getLogger(__name__)


class PersistActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="Persist")
    async def __call__(self, turn_id: str, status: str, plan_id: str = "") -> None:
        await self._pool.execute(
            "UPDATE turns SET status = $2, completed_at = now(), "
            "plan_id = COALESCE(NULLIF($3, ''), plan_id) WHERE turn_id = $1",
            turn_id,
            status,
            plan_id,
        )
        logger.info("Persist[%s]: status=%s plan_id=%s", turn_id, status, plan_id or "(unchanged)")
