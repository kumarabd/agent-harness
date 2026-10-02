"""Persist activity — narrowed 2026-08-14 for the reference-passing contract.

Real design: previously wrote the whole turn's messages/tool_calls in one
end-of-turn transaction. Under the reference-passing contract
(docs/components/temporal-workflow.md, docs/components/state-layer.md), those
writes moved earlier and elsewhere — the start-of-turn message insert
(insert_message.py) and every ModelCall/ToolCall write incrementally as the
turn progresses. There's no content left for Persist to batch; its only
remaining job is marking the turn's own row complete.
"""

from __future__ import annotations

import logging

from temporalio import activity

logger = logging.getLogger(__name__)


class PersistActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="Persist")
    async def __call__(self, turn_id: str, status: str) -> None:
        await self._pool.execute(
            "UPDATE turns SET status = $2, completed_at = now() WHERE turn_id = $1",
            turn_id,
            status,
        )
        logger.info("Persist[%s]: status=%s", turn_id, status)
