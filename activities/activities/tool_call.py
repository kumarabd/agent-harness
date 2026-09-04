"""ToolCall activity.

Dispatches to a real tool implementation via tools.TOOL_REGISTRY
(docs/components/activities-outbound-delivery.md's Tier A/B/C heartbeat
policy, applied for real here rather than simulated). An unrecognized
tool_name, or any exception a handler raises, produces a real `status='error'`
result — this stub previously had no error path at all, only success and
cancellation.

A per-task **resolved** tool (docs/components/tool-registry.md, "Resolved:
Three-Layer Tool Taxonomy & Per-Task Resolution") is offered to the model
under its own name, not `call_tool` — TOOL_REGISTRY has no handler for it. The
`resolved_server`/`resolved_tool` columns ModelCall set at mint time (migration
026) route that case through the internal `call_tool` proxy instead of a
TOOL_REGISTRY lookup, using `call_tool`'s own timing profile.

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
import time

from temporalio import activity
from temporalio.exceptions import CancelledError

from . import ids, llm_client, model_registry
from .tools import TOOL_REGISTRY, ToolContext, call_tool, resolve_session_dir
from .types import ToolCallInput, ToolCallOutput

logger = logging.getLogger(__name__)


class ToolCallActivity:
    def __init__(self, pool, temporal_client=None):
        self._pool = pool
        # The AsyncOpenAI client is NOT injected anymore (2026-08-28,
        # per-tier provider revision) — it's resolved per call via
        # llm_client.get_client(model_config), from whichever tier's
        # config the summary path actually uses. See __call__ below.
        #
        # temporal_client IS injected — the intention tools
        # (tools_intention.py, docs/components/proactivity.md) are thin
        # wrappers over it (start/signal/cancel/query an IntentionWorkflow).
        # Same "an activity may hold its own client" pattern ModelCallActivity
        # already uses for its streaming path.
        self._temporal_client = temporal_client

    @activity.defn(name="ToolCall")
    async def __call__(self, input: ToolCallInput) -> ToolCallOutput:
        # Connection acquired only for this read, never held across real
        # tool work below - under the design's parallel tool-call fan-out,
        # holding a pooled connection idle for however long a real tool
        # takes would exhaust the pool (max_size=10) with just a handful of
        # concurrent tool calls.
        row = await self._pool.fetchrow(
            "SELECT tool_name, arguments, parent_id, resolved_server, resolved_tool "
            "FROM tool_calls WHERE tool_call_id = $1",
            input.tool_call_id,
        )
        if row is None:
            raise RuntimeError(f"ToolCall: no tool_calls row found for {input.tool_call_id!r}")
        tool_name: str = row["tool_name"]
        arguments: dict = json.loads(row["arguments"])
        turn_id: str = row["parent_id"]

        # docs/components/tool-registry.md, "Resolved: Three-Layer Tool
        # Taxonomy & Per-Task Resolution" — a per-task resolved (ToolDiscover)
        # call is offered to the model under its own name (e.g.
        # "weather_lookup"), so TOOL_REGISTRY has no handler for it; migration
        # 026's resolved_server/resolved_tool (set only for that case, never
        # for shell_exec/call_tool themselves) says to route it through the
        # internal call_tool proxy instead — same timing profile as call_tool
        # (a network round-trip to mcp-hub either way), different handler.
        resolved_server: str | None = row["resolved_server"]
        resolved_tool: str | None = row["resolved_tool"]
        if resolved_server:
            spec = TOOL_REGISTRY["call_tool"]
            handler = call_tool
            handler_arguments = {"server": resolved_server, "tool": resolved_tool, "arguments": arguments}
        else:
            spec = TOOL_REGISTRY.get(tool_name)
            if spec is None:
                logger.warning("ToolCall: unknown tool %r for %s", tool_name, input.tool_call_id)
                return await self._finish_error(input.tool_call_id, f"unknown tool: {tool_name}")
            handler = spec.handler
            handler_arguments = arguments

        fs_path = ids.session_fs_path(turn_id)
        # Resolve the default-tier config once here so per-tool
        # exploration_summary calls don't each re-read env vars. Uses
        # the "medium" tier deliberately (bootstrap default) — the same
        # tier compress_context uses, for the same reasoning
        # (fixed-purpose call, not model-hint-driven). If that tier
        # isn't configured we degrade to no-LLM summary, matching
        # exploration_summary's own graceful-degradation contract —
        # never fails a tool call over the summary path.
        summary_config = model_registry.resolve(*model_registry.default_hint())
        summary_provider = None
        if summary_config.model:
            try:
                summary_provider = llm_client.get_provider(summary_config)
            except RuntimeError as exc:
                logger.info(
                    "ToolCall[%s]: no summary provider (%s) — exploration_summary will run deterministic-only",
                    input.tool_call_id, exc,
                )
                summary_provider = None

        ctx = ToolContext(
            pool=self._pool,
            session_key=ids.session_key_of(turn_id),
            fs_path=fs_path,
            session_dir=resolve_session_dir(fs_path),
            holder_id=activity.info().task_token.hex(),
            tool_call_id=input.tool_call_id,
            summary_provider=summary_provider,
            summary_model=summary_config.model,
            heartbeat_interval_seconds=spec.heartbeat_interval_seconds,
            lease_ttl_seconds=spec.heartbeat_timeout_seconds,
            temporal_client=self._temporal_client,
        )

        if resolved_server:
            logger.info("ToolCall start: %s -> call_tool(%s/%s, %r)", tool_name, resolved_server, resolved_tool, arguments)
        else:
            logger.info("ToolCall start: %s(%r)", tool_name, arguments)
        # docs/components/budget-guardrails.md, "Resolved: Metrics Export" —
        # labeled by tool_name/status (the model's own chosen name even for a
        # resolved dispatch — that's what's meaningful in the metric, not the
        # internal call_tool proxy it happens to route through), emitted on
        # every exit path below.
        meter = activity.metric_meter().with_additional_attributes({"tool_name": tool_name})
        started = time.monotonic()

        def record(status: str) -> None:
            duration = time.monotonic() - started
            meter.with_additional_attributes({"status": status}).create_counter("tool_call_total").add(1)
            meter.create_histogram_float("tool_call_latency_seconds", unit="s").record(duration)

        try:
            result = await handler(handler_arguments, ctx)
        except (asyncio.CancelledError, CancelledError):
            logger.info("ToolCall cancelled: %s", tool_name)
            record("cancelled")
            await self._pool.execute(
                "UPDATE tool_calls SET status = 'cancelled', reason = $2, side_effect = 'unknown', "
                "completed_at = now() WHERE tool_call_id = $1",
                input.tool_call_id,
                "interrupted_by_new_message",
            )
            return ToolCallOutput(tool_call_id=input.tool_call_id, status="cancelled")
        except Exception as exc:  # noqa: BLE001 - deliberately broad, see module docstring
            logger.exception("ToolCall error: %s", tool_name)
            record("error")
            return await self._finish_error(input.tool_call_id, str(exc))

        logger.info("ToolCall done: %s -> %r", tool_name, result)
        record("ok")
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


class DenyToolCallActivity:
    """docs/components/user-input.md — ApprovalGatedToolCallWorkflow's own
    exit path when a call is denied or its approval wait is cancelled before
    ever reaching the real ToolCall activity. The tool_calls row was minted
    by ModelCall at status='pending' (see the schema's own note on why that
    default exists) — nothing else transitions it out of 'pending' if
    ToolCall itself never runs."""

    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="DenyToolCall")
    async def __call__(self, tool_call_id: str, reason: str) -> None:
        await self._pool.execute(
            "UPDATE tool_calls SET status = 'cancelled', reason = $2, side_effect = 'none', "
            "completed_at = now() WHERE tool_call_id = $1",
            tool_call_id,
            reason,
        )
        logger.info("DenyToolCall[%s]: reason=%s", tool_call_id, reason)
