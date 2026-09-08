"""UserInputRequestWorkflow's two activities — docs/components/user-input.md.
Everything content-bearing about a pending request lives in Postgres, read
back by the workflow only as the {status, response} it needs to decide
whether it's still waiting; the request/response payloads themselves never
round-trip through the workflow beyond what UserInputRequestWorkflow already
holds as its own input/output (reference-passing contract,
components/temporal-workflow.md).

Two request kinds share this primitive: `permission` (approval gating — the
tool_calls row is transitioned by ToolCall/DenyToolCall) and `question`
(`ask_user`, docs/components/turn-pipeline.md — turn.go dispatches this as a
child workflow and parks the loop on it, and the answer is written back into
the ask_user tool_calls row here so the next ModelCall sees it as an
observation).
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
        prompt = request.prompt
        options = [{"id": o.id, "label": o.label} for o in request.options]

        # ask_user (Kind "question"): turn.go passes only the request id (== the
        # ask_user tool_call_id) — the question + options live in the model's
        # own tool_calls.arguments, read here so the workflow never holds them.
        # Safe option ids (opt_N) since a model-authored option string can
        # contain ':' and would break discord_user_input.go's custom_id parse.
        if request.kind == "question" and not prompt:
            tc = await self._pool.fetchrow(
                "SELECT arguments FROM tool_calls WHERE tool_call_id = $1", request.request_id
            )
            args = json.loads(tc["arguments"]) if tc and tc["arguments"] else {}
            prompt = str(args.get("question", "")).strip() or "(the assistant needs your input)"
            options = [
                {"id": f"opt_{i}", "label": str(o).strip()}
                for i, o in enumerate(args.get("options") or [])
                if str(o).strip()
            ]

        await self._pool.execute(
            "INSERT INTO user_input_requests "
            "(request_id, turn_id, workflow_id, kind, prompt, options, allow_free_text, context) "
            "VALUES ($1, $2, $3, $4, $5, $6, $7, $8) "
            "ON CONFLICT (request_id) DO NOTHING",
            request.request_id,
            request.turn_id,
            workflow_id,
            request.kind,
            prompt,
            json.dumps(options),
            request.allow_free_text,
            json.dumps(request.context),
        )
        logger.info(
            "RequestUserInput[%s]: kind=%s %r options=%r",
            request.request_id, request.kind, prompt[:80], [o["label"] for o in options],
        )


class CloseUserInputActivity:
    """Closes out a pending request in exactly one of two terminal states —
    'answered' (a real response arrived) or 'cancelled' (expired, or the
    workflow was cancelled by an unrelated interrupt). UserInputRequestWorkflow
    calls this on all of its own exit paths.

    For an `ask_user` (kind 'question') it ALSO transitions the model's own
    ask_user tool_calls row (request_id == tool_call_id): 'answered' → status
    'ok' with {"answer": ...} so the next ModelCall reads it as an observation;
    otherwise 'cancelled' (the user typically replied with a plain message
    instead, which turn.go folds in as the next user turn)."""

    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="CloseUserInput")
    async def __call__(
        self, request_id: str, status: str, selected_option_id: str | None, free_text: str | None
    ) -> None:
        row = await self._pool.fetchrow(
            "UPDATE user_input_requests SET status = $2, selected_option_id = $3, "
            "free_text = $4, answered_at = now() WHERE request_id = $1 RETURNING kind, options",
            request_id,
            status,
            selected_option_id,
            free_text,
        )
        logger.info("CloseUserInput[%s]: status=%s selected=%r", request_id, status, selected_option_id)

        if not row or row["kind"] != "question":
            return

        answer = (free_text or "").strip() or None
        if answer is None and selected_option_id is not None:
            opts = json.loads(row["options"]) if row["options"] else []
            answer = next((o["label"] for o in opts if o["id"] == selected_option_id), selected_option_id)

        if status == "answered" and answer:
            await self._pool.execute(
                "UPDATE tool_calls SET status = 'ok', result = $2, side_effect = 'none', "
                "completed_at = now() WHERE tool_call_id = $1 AND status = 'pending'",
                request_id,
                json.dumps({"answer": answer}),
            )
        else:
            await self._pool.execute(
                "UPDATE tool_calls SET status = 'cancelled', reason = 'no_button_answer', "
                "side_effect = 'none', completed_at = now() WHERE tool_call_id = $1 AND status = 'pending'",
                request_id,
            )
