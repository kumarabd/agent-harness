"""CompressContext activity.

Real design: triggered by the turn workflow's inline token-budget gate; the
compression operation itself is a model call (summarization), so it has to be
an activity, not inline workflow logic
(components/temporal-workflow.md, "Resolved: Compression / Context Management").

Reshaped 2026-08-14 for the reference-passing contract: input is `turn_id`
only, not a message slice — the workflow holds no messages to pass. This stub
remains a no-op (real summarization against the messages table is future
work, consistent with the design doc marking exact compression mechanics as
not fully specified beyond the gate itself) — it exists so the gate is
exercised end-to-end if a scenario is scripted to reach the compression
threshold, without yet doing real compaction.
"""

from __future__ import annotations

import logging

from temporalio import activity

logger = logging.getLogger(__name__)


@activity.defn(name="CompressContext")
async def compress_context(turn_id: str) -> None:
    logger.info("CompressContext[%s]: no-op stub (real summarization is future work)", turn_id)
