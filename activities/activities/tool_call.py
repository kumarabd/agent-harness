"""ToolCall activity.

Real design: a generic, retryable tool-call activity, cooperative cancellation
for Tier B/C tools via heartbeat (components/activities-outbound-delivery.md,
"Resolved: Heartbeat Interval Policy"). This stub is written Tier-B-shaped
(explicit heartbeating) specifically so this slice can exercise real
cancellation delivery end-to-end, rather than the Tier-A "no heartbeat, runs to
completion" fallback — a genuine interrupt needs something to actually
interrupt.

Reshaped 2026-08-14 for the reference-passing contract
(docs/components/temporal-workflow.md): input/output are IDs only. This
activity reads its own `arguments` from Postgres (written by ModelCall) and
writes its `status`/`result`/`reason`/`side_effect` back there — the workflow
only ever sees `{tool_call_id, status}`.

Real cancellation delivery for a non-local Temporal activity is
heartbeat-driven: the SDK only learns a cancellation was requested from the
*response* to a heartbeat call it sends to the server on its own internal
timer. Without calling activity.heartbeat() here, a RequestCancelActivity on
the workflow side can sit server-side indefinitely and never reach this
coroutine at all — so heartbeating isn't optional plumbing, it's the actual
mechanism cancellation depends on.
"""

from __future__ import annotations

import asyncio
import json
import logging

from temporalio import activity
from temporalio.exceptions import CancelledError

from .types import ToolCallInput, ToolCallOutput

logger = logging.getLogger(__name__)


class ToolCallActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="ToolCall")
    async def __call__(self, input: ToolCallInput) -> ToolCallOutput:
        # Connections are acquired only for the actual DB read/write below,
        # never held across the simulated-work sleep loop — under the design's
        # parallel tool-call fan-out, holding a pooled connection idle for the
        # full ~4s of "work" would exhaust the pool (max_size=10) with just a
        # handful of concurrent tool calls.
        row = await self._pool.fetchrow(
            "SELECT tool_name, arguments FROM tool_calls WHERE tool_call_id = $1", input.tool_call_id
        )
        if row is None:
            raise RuntimeError(f"ToolCall: no tool_calls row found for {input.tool_call_id!r}")
        tool_name: str = row["tool_name"]
        arguments: dict = json.loads(row["arguments"])

        logger.info("ToolCall start: %s(%r)", tool_name, arguments)
        try:
            # An artificial multi-step delay so a mid-turn interrupt has a
            # real window to land while this activity is still in flight.
            # Heartbeat on every tick — this is what actually lets a
            # RequestCancelActivity from the workflow reach this coroutine
            # (see module docstring); without it, cancellation would
            # silently never arrive regardless of how long this loop runs.
            for _ in range(20):
                await asyncio.sleep(0.2)
                activity.heartbeat()
                if activity.is_cancelled():
                    raise CancelledError("tool call cancelled")
        except (asyncio.CancelledError, CancelledError):
            logger.info("ToolCall cancelled: %s", tool_name)
            await self._pool.execute(
                "UPDATE tool_calls SET status = 'cancelled', reason = $2, side_effect = 'unknown', "
                "completed_at = now() WHERE tool_call_id = $1",
                input.tool_call_id,
                "interrupted_by_new_message",
            )
            return ToolCallOutput(tool_call_id=input.tool_call_id, status="cancelled")

        result = {"tool": tool_name, "echo_arguments": arguments}
        logger.info("ToolCall done: %s -> %r", tool_name, result)
        await self._pool.execute(
            "UPDATE tool_calls SET status = 'ok', result = $2, completed_at = now() WHERE tool_call_id = $1",
            input.tool_call_id,
            json.dumps(result),
        )
        return ToolCallOutput(tool_call_id=input.tool_call_id, status="ok")
