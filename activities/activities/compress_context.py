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
resolving turn_id -> session_key and injecting the (pool, provider) this
needs — the provider comes from llm_client.get_provider(config), keyed
on the medium tier's own config (docs/components/model-registry.md).
"""

from __future__ import annotations

import logging

from temporalio import activity

from . import ids, lcm, llm_client, model_registry

logger = logging.getLogger(__name__)


class CompressContextActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="CompressContext")
    async def __call__(self, turn_id: str) -> None:
        session_key = ids.session_key_of(turn_id)
        # Resolved via model_registry, same pattern as
        # activities/activities/tool_call.py (which resolves the same
        # default-tier model for exploration_summary's LLM path).
        # Compaction is a fixed-purpose call, not a
        # model-hint-driven-per-step call, so it uses the bootstrap
        # default tier directly rather than threading a hint through —
        # the "medium" tier is a deliberate middle-ground pick for
        # summarization quality/cost, revisit if real usage evidence
        # suggests fast is enough or expert is needed.
        #
        # The AsyncOpenAI client is per-tier now (2026-08-28) — resolved
        # from the tier's own base_url/api_key via llm_client, not
        # injected here at construction.
        config = model_registry.resolve(*model_registry.default_hint())
        if not config.model:
            logger.warning(
                "CompressContext[%s]: LANGUAGE_%s_MODEL not set, skipping compaction",
                turn_id,
                model_registry.default_hint()[1].upper(),
            )
            return
        try:
            provider = llm_client.get_provider(config)
        except RuntimeError as exc:
            # Tier's provider/base_url/api_key isn't configured either —
            # degrade gracefully rather than failing the whole compaction
            # (which runs fire-and-forget from turn.go's soft-compression path).
            logger.warning("CompressContext[%s]: %s, skipping compaction", turn_id, exc)
            return
        async with self._pool.acquire() as conn:
            await lcm.compact(conn, session_key, provider, config.model)
