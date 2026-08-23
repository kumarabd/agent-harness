"""CompressContext activity.

Real design: triggered by the turn workflow's two-tier compression gate
(docs/components/context-slot.md, "Resolved: Duties and Strategies" #3 —
soft fires async via CompressContextWorkflow, hard blocks; see turn.go).
The compression operation itself is a model call (summarization), so it has
to be an activity, not inline workflow logic
(components/temporal-workflow.md, "Resolved: Compression / Context
Management").

Real body, not a stub — delegates entirely to lcm.compact (Three-Level
Escalation: LLM summarize, preserve detail -> LLM summarize, aggressive
bullets -> deterministic truncate). This activity's own job is just
resolving turn_id -> session_key and injecting the (pool, openai_client)
this needs, same shape as ModelCallActivity.
"""

from __future__ import annotations

import logging
import os

from temporalio import activity

from . import ids, lcm

logger = logging.getLogger(__name__)


class CompressContextActivity:
    def __init__(self, pool, openai_client):
        self._pool = pool
        self._openai_client = openai_client

    @activity.defn(name="CompressContext")
    async def __call__(self, turn_id: str) -> None:
        session_key = ids.session_key_of(turn_id)
        # Reuses the same model ModelCall's real calls use (PIONEER_MODEL) —
        # no separate model-registry tier for this (components/
        # model-registry.md doesn't exist in code yet), consistent with how
        # memory-slot.md's own extraction-model question was simplified
        # away when agent-brain turned out not to need caller-side
        # extraction at all.
        model = os.environ.get("PIONEER_MODEL")
        if not model:
            logger.warning("CompressContext[%s]: PIONEER_MODEL not set, skipping compaction", turn_id)
            return
        async with self._pool.acquire() as conn:
            await lcm.compact(conn, session_key, self._openai_client, model)
