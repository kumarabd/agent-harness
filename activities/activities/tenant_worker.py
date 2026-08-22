"""Tenant worker entrypoint. Registers ModelCall, ToolCall, InsertMessage,
Persist, Deliver, and CompressContext on the configured task queue and polls a
Temporal server for activity tasks. Run alongside the shared loop-worker
(workflows/cmd/loop-worker), which registers the Session Coordinator and Turn
Workflow on the same task queue and dispatches activities to this process by
name.

Configured via env vars (not hardcoded) so this process is deployable — see
deploy/docker/tenant-worker.Dockerfile and deploy/helm/agent-harness-tenant:

    TEMPORAL_ADDRESS     Temporal frontend host:port. Default: localhost:7233.
    TEMPORAL_NAMESPACE   Temporal namespace. Default: default.
    TEMPORAL_TASK_QUEUE  Task queue name. Default: agent-loop. Must match the
                         shared loop-worker's TEMPORAL_TASK_QUEUE.
    POSTGRES_HOST/PORT/DB/USER/PASSWORD  This tenant's Postgres instance
                         (docs/components/multi-tenancy.md). See db.py.
    SESSION_ROOT         Root of the session filesystem tree real tools
                         (tools.py) operate in — real deployments point this
                         at the tenant PV's session mount (/sessions in the
                         Helm chart); defaults to /tmp/agent-harness-sessions
                         for local dev. See tools.py.
    PIONEER_API_KEY      Required for real ModelCall calls (llm.py) — Pioneer,
                         an OpenAI-API-compatible provider (openai SDK's
                         base_url override). Fixture-only turns (a row in
                         _test_scripted_responses) never need it.
    PIONEER_BASE_URL     Default: https://api.pioneer.ai/v1. See llm.py.
    PIONEER_MODEL        Required for real calls — no default (Pioneer's
                         model catalog isn't known ahead of time; guessing
                         one would likely just be wrong). See llm.py.
    PIONEER_MAX_TOKENS   Default: 4096. See llm.py.
    METRICS_BIND_ADDRESS Host:port the Prometheus exposition endpoint listens
                         on. Default: 0.0.0.0:9090. See
                         docs/components/budget-guardrails.md, "Resolved:
                         Metrics Export" — plain scrape, no ServiceMonitor.

Usage:
    python -m activities.tenant_worker
"""

from __future__ import annotations

import asyncio
import logging
import os

from openai import AsyncOpenAI
from temporalio.client import Client
from temporalio.runtime import PrometheusConfig, Runtime, TelemetryConfig
from temporalio.worker import Worker

from .compress_context import compress_context
from .db import create_pool
from .deliver import DeliverActivity
from .insert_message import InsertMessageActivity
from .model_call import ModelCallActivity
from .persist import PersistActivity
from .tool_call import ToolCallActivity


async def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")

    address = os.environ.get("TEMPORAL_ADDRESS", "localhost:7233")
    namespace = os.environ.get("TEMPORAL_NAMESPACE", "default")
    task_queue = os.environ.get("TEMPORAL_TASK_QUEUE", "agent-loop")

    pool = await create_pool()
    # Constructed once and reused, same rationale as the Postgres pool above —
    # not created per-call, not global state, injected via ModelCallActivity's
    # constructor. AsyncOpenAI() validates api_key eagerly at construction
    # (confirmed directly - raises OpenAIError immediately if unset), which
    # would otherwise crash the whole worker at startup even for pure
    # fixture-only usage that never needs a real model call at all
    # (model_call.py's fixture-first branch never calls llm.py). A placeholder
    # here defers any real failure to actual call time, where a bad/missing
    # key surfaces the same way any other real API error does.
    #
    # Pioneer is OpenAI-API-compatible - same SDK, just pointed at a
    # different base_url, no separate client library needed.
    openai_client = AsyncOpenAI(
        api_key=os.environ.get("PIONEER_API_KEY") or "unset",
        base_url=os.environ.get("PIONEER_BASE_URL", "https://api.pioneer.ai/v1"),
    )

    # docs/components/budget-guardrails.md, "Resolved: Metrics Export" —
    # temporalio's built-in Prometheus support: no new dependency, no
    # bespoke registry. Emits the SDK's own built-in activity/workflow
    # metrics for free, plus whatever ModelCallActivity/ToolCallActivity
    # record via activity.metric_meter(). Scraped directly (plain
    # prometheus.io/scrape pod annotations — no ServiceMonitor).
    runtime = Runtime(
        telemetry=TelemetryConfig(
            metrics=PrometheusConfig(bind_address=os.environ.get("METRICS_BIND_ADDRESS", "0.0.0.0:9090"))
        )
    )
    client = await Client.connect(address, namespace=namespace, runtime=runtime)
    # Worker needs the bound __call__ methods, not the instances themselves —
    # @activity.defn attaches its metadata to the decorated function, and
    # `instance.__call__` (bound) carries that metadata; the bare instance
    # does not.
    worker = Worker(
        client,
        task_queue=task_queue,
        activities=[
            ModelCallActivity(pool, openai_client).__call__,
            ToolCallActivity(pool).__call__,
            InsertMessageActivity(pool).__call__,
            PersistActivity(pool).__call__,
            DeliverActivity(pool).__call__,
            compress_context,
        ],
    )
    logging.getLogger(__name__).info(
        "tenant worker starting: temporal=%r namespace=%r task_queue=%r", address, namespace, task_queue
    )
    try:
        await worker.run()
    finally:
        await openai_client.close()
        await pool.close()


if __name__ == "__main__":
    asyncio.run(main())
