"""Wake activities — docs/components/proactivity.md.

`WakeSession` SignalWithStarts the session coordinator's `Wake` handler: the same
entry point the gateway uses for a user message, so everything downstream is an
ordinary turn. **One activity for every source of a wake** — a Temporal Schedule
tick, an event callback landing on the harness ingress, or the manual starter CLI
— because a wake does not care what armed it.

There is deliberately **no condition-checking activity**. A watch is a wake whose
decider calls a tool to check something, so the check is the model's own tool call
inside the turn it woke — not a poll loop with a fast-tier judge bolted to it. The
old `CheckCondition` (a probe plus an LLM-judged natural-language predicate, every
poll interval) made cost scale with watches x frequency to answer questions a
comparison settles, and it was a stub in any case: it always returned
fired=False, so `condition` / `state` / `event` watches never fired at all.
"""

from __future__ import annotations

import logging
import os

from temporalio import activity
from temporalio.common import WorkflowIDConflictPolicy, WorkflowIDReusePolicy

from .types import WakeSessionInput

logger = logging.getLogger(__name__)

_COORDINATOR_WORKFLOW = "CoordinatorWorkflow"
_WAKE_SIGNAL = "Wake"


class WakeSessionActivity:
    def __init__(self, pool, temporal_client):
        self._pool = pool  # unused today; kept for symmetry with the other activities
        self._client = temporal_client
        # 2026-09-26: renamed from TEMPORAL_TASK_QUEUE — docs/components/
        # multi-tenancy.md's "Resolved: Shared Temporal Namespace, Per-Tenant Task
        # Queues". CoordinatorWorkflow is a separate execution the shared
        # loop-worker pool polls on the fixed workflow-task queue, never this
        # tenant's own activity queue.
        self._task_queue = os.environ.get("TEMPORAL_WORKFLOW_TASK_QUEUE", "agent-loop")
        self._tenant_slug = os.environ.get("TENANT_SLUG", "")

    @activity.defn(name="WakeSession")
    async def __call__(self, input: WakeSessionInput) -> None:
        # SignalWithStart: reuse the running coordinator if there is one, else
        # start it headless (empty ConnectionID -> the proactive turn's output
        # routes to Postgres, surfaced on next open — proactivity.md "Delivery").
        # This is what makes "the session may or may not be running" a non-issue:
        # the primitive covers both, and an active turn gets the wake folded in
        # by the coordinator's own Wake handler.
        #
        # ALLOW_DUPLICATE is not decoration. The two policies here cover disjoint
        # cases and neither substitutes for the other: id_conflict_policy applies
        # while an execution is RUNNING, id_reuse_policy when the previous one has
        # CLOSED. The coordinator exits cleanly on idle TTL (coordinator.go's
        # `return nil`), and Temporal's default ALLOW_DUPLICATE_FAILED_ONLY reuses
        # an id only if the last run FAILED — so without this line the common case,
        # waking a session that has gone idle, is refused outright. The starter CLI
        # sets it for the same reason (cmd/starter/main.go).
        await self._client.start_workflow(
            _COORDINATOR_WORKFLOW,
            {"session_key": input.session_key, "tenant_slug": self._tenant_slug},
            id=input.session_key,
            task_queue=self._task_queue,
            id_reuse_policy=WorkflowIDReusePolicy.ALLOW_DUPLICATE,
            id_conflict_policy=WorkflowIDConflictPolicy.USE_EXISTING,
            start_signal=_WAKE_SIGNAL,
            start_signal_args=[
                {
                    "wake_id": input.wake_id,
                    "objective": input.objective,
                    "why": input.why,
                }
            ],
        )
        logger.info("WakeSession[%s]: woke coordinator %s", input.wake_id, input.session_key)
