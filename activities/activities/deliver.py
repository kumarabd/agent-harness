"""Deliver activity.

Real design: dispatched directly to a gateway-embedded Temporal worker via
deterministic task-queue routing, no message broker
(components/gateway.md, components/activities-outbound-delivery.md). No gateway
exists in this slice — this just logs what it would have delivered.

Reshaped 2026-08-14 for the reference-passing contract
(docs/components/temporal-workflow.md): input is `turn_id` only — this
activity reads the turn's final assistant message from Postgres itself
(the most recent assistant-role message for that turn) rather than receiving
the content as an argument, same shape as every other content-touching
activity in this design.
"""

from __future__ import annotations

import logging

from temporalio import activity

logger = logging.getLogger(__name__)


class DeliverActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="Deliver")
    async def __call__(self, turn_id: str) -> None:
        row = await self._pool.fetchrow(
            "SELECT content FROM messages WHERE parent_id = $1 AND role = 'assistant' "
            "ORDER BY seq DESC LIMIT 1",
            turn_id,
        )
        content = row["content"] if row else ""
        logger.info(
            "Deliver[%s]: %r (stub — no gateway in this slice)",
            turn_id,
            (content or "")[:200],
        )
