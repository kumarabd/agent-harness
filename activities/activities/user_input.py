"""UserInputRequestWorkflow's two activities — docs/components/user-input.md.
Everything content-bearing about a pending request lives in Postgres, read
back by the workflow only as the {status, response} it needs to decide
whether it's still waiting; the request/response payloads themselves never
round-trip through the workflow beyond what UserInputRequestWorkflow already
holds as its own input/output (reference-passing contract,
components/temporal-workflow.md).

RequestUserInputActivity's "deliver this to the user" step is a stub, same
honest shape as deliver.py itself — no real Gateway exists yet
(components/gateway.md) to actually push a prompt+options out to any
platform. Logging what would have been shown is the same choice deliver.py
already made for exactly this reason.
"""

from __future__ import annotations

import json
import logging

from temporalio import activity

from .types import UserInputRequest

logger = logging.getLogger(__name__)


class RequestUserInputActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="RequestUserInput")
    async def __call__(self, request: UserInputRequest, workflow_id: str) -> None:
        await self._pool.execute(
            "INSERT INTO user_input_requests "
            "(request_id, turn_id, workflow_id, kind, prompt, options, allow_free_text, context) "
            "VALUES ($1, $2, $3, $4, $5, $6, $7, $8) "
            "ON CONFLICT (request_id) DO NOTHING",
            request.request_id,
            request.turn_id,
            workflow_id,
            request.kind,
            request.prompt,
            json.dumps([{"id": o.id, "label": o.label} for o in request.options]),
            request.allow_free_text,
            json.dumps(request.context),
        )
        logger.info(
            "RequestUserInput[%s]: %r options=%r (stub — no gateway in this slice)",
            request.request_id,
            request.prompt,
            [o.label for o in request.options],
        )


class CloseUserInputActivity:
    """Closes out a pending request in exactly one of two terminal states —
    'answered' (a real response arrived) or 'cancelled' (expired, or the
    workflow was cancelled by an unrelated interrupt). UserInputRequestWorkflow
    calls this on all three of its own exit paths, not just the "real answer"
    one — an earlier version of that workflow only updated Postgres on the
    happy path, leaving a row stuck at 'pending' forever on cancellation.
    Found and fixed alongside generalizing this activity to cover all three."""

    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="CloseUserInput")
    async def __call__(
        self, request_id: str, status: str, selected_option_id: str | None, free_text: str | None
    ) -> None:
        await self._pool.execute(
            "UPDATE user_input_requests SET status = $2, selected_option_id = $3, "
            "free_text = $4, answered_at = now() WHERE request_id = $1",
            request_id,
            status,
            selected_option_id,
            free_text,
        )
        logger.info("CloseUserInput[%s]: status=%s selected=%r", request_id, status, selected_option_id)
