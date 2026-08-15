"""ToolCall activity.

Dispatches to a real tool implementation via tools.TOOL_REGISTRY (currently
just shell_exec — docs/components/activities-outbound-delivery.md's Tier
A/B/C heartbeat policy, applied for real here rather than simulated). An
unrecognized tool_name, or any exception a handler raises, produces a real
`status='error'` result — this stub previously had no error path at all,
only success and cancellation.

Reshaped 2026-08-14 for the reference-passing contract
(docs/components/temporal-workflow.md): input/output are IDs only. This
activity reads its own `tool_name`/`arguments` from Postgres (written by
ModelCall) and writes its `status`/`result`/`reason`/`side_effect` back
there — the workflow only ever sees `{tool_call_id, status}`.

Handler exceptions are caught and sanitized — only the exception's message
crosses back into `result`, never a raw traceback or captured locals
(docs/components/activities-outbound-delivery.md, "Resolved: Panic
Handling" — defense-in-depth, not the primary isolation mechanism, but
still good practice per that doc). A genuinely unexpected infrastructure
failure (e.g. the tool_calls row itself missing, or the status-write UPDATE
failing) is left to propagate and fail the activity for real — that's not a
tool-execution error, it's safe and correct for Temporal's own RetryPolicy
to retry.

Real cancellation delivery for a non-local Temporal activity is
heartbeat-driven: the SDK only learns a cancellation was requested from the
*response* to a heartbeat call it sends to the server on its own internal
timer. Each real tool handler is responsible for heartbeating on its own
tier's cadence (tools.ToolContext) - without it, a RequestCancelActivity on
the workflow side can sit server-side indefinitely and never reach the
coroutine at all.
"""

from __future__ import annotations

import asyncio
import json
import logging

from temporalio import activity
from temporalio.exceptions import CancelledError

from . import ids
from .tools import TOOL_REGISTRY, ToolContext, resolve_session_dir
from .types import ToolCallInput, ToolCallOutput

logger = logging.getLogger(__name__)


class ToolCallActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="ToolCall")
    async def __call__(self, input: ToolCallInput) -> ToolCallOutput:
        # Connection acquired only for this read, never held across real
        # tool work below - under the design's parallel tool-call fan-out,
        # holding a pooled connection idle for however long a real tool
        # takes would exhaust the pool (max_size=10) with just a handful of
        # concurrent tool calls.
        row = await self._pool.fetchrow(
            "SELECT tool_name, arguments, parent_id FROM tool_calls WHERE tool_call_id = $1",
            input.tool_call_id,
        )
        if row is None:
            raise RuntimeError(f"ToolCall: no tool_calls row found for {input.tool_call_id!r}")
        tool_name: str = row["tool_name"]
        arguments: dict = json.loads(row["arguments"])
        turn_id: str = row["parent_id"]

        spec = TOOL_REGISTRY.get(tool_name)
        if spec is None:
            logger.warning("ToolCall: unknown tool %r for %s", tool_name, input.tool_call_id)
            return await self._finish_error(input.tool_call_id, f"unknown tool: {tool_name}")

        fs_path = ids.session_fs_path(turn_id)
        ctx = ToolContext(
            pool=self._pool,
            session_key=ids.session_key_of(turn_id),
            fs_path=fs_path,
            session_dir=resolve_session_dir(fs_path),
            holder_id=activity.info().task_token.hex(),
            heartbeat_interval_seconds=spec.heartbeat_interval_seconds,
            lease_ttl_seconds=spec.heartbeat_timeout_seconds,
        )

        logger.info("ToolCall start: %s(%r)", tool_name, arguments)
        try:
            result = await spec.handler(arguments, ctx)
        except (asyncio.CancelledError, CancelledError):
            logger.info("ToolCall cancelled: %s", tool_name)
            await self._pool.execute(
                "UPDATE tool_calls SET status = 'cancelled', reason = $2, side_effect = 'unknown', "
                "completed_at = now() WHERE tool_call_id = $1",
                input.tool_call_id,
                "interrupted_by_new_message",
            )
            return ToolCallOutput(tool_call_id=input.tool_call_id, status="cancelled")
        except Exception as exc:  # noqa: BLE001 - deliberately broad, see module docstring
            logger.exception("ToolCall error: %s", tool_name)
            return await self._finish_error(input.tool_call_id, str(exc))

        logger.info("ToolCall done: %s -> %r", tool_name, result)
        await self._pool.execute(
            "UPDATE tool_calls SET status = 'ok', result = $2, completed_at = now() WHERE tool_call_id = $1",
            input.tool_call_id,
            json.dumps(result),
        )
        return ToolCallOutput(tool_call_id=input.tool_call_id, status="ok")

    async def _finish_error(self, tool_call_id: str, message: str) -> ToolCallOutput:
        await self._pool.execute(
            "UPDATE tool_calls SET status = 'error', result = $2, completed_at = now() WHERE tool_call_id = $1",
            tool_call_id,
            json.dumps({"error": message}),
        )
        return ToolCallOutput(tool_call_id=tool_call_id, status="error")
