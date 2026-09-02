"""Tenant worker entrypoint. Registers ModelCall, ToolCall, InsertMessage,
Persist, Deliver, WriteMemory, and CompressContext on the configured task
queue and polls a Temporal server for activity tasks. Run alongside the
shared loop-worker
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
    Model + provider config is entirely PER-TIER now — see
    docs/components/model-registry.md. Each tier the deployment actually
    uses (fast/medium/expert) must set its own full triple:
        LANGUAGE_<TIER>_MODEL         Required.
        LANGUAGE_<TIER>_API_KEY       Required.
        LANGUAGE_<TIER>_BASE_URL      Required.
        LANGUAGE_<TIER>_MAX_TOKENS    Optional, defaults to 4096.
        LANGUAGE_<TIER>_CONTEXT_WINDOW           Optional, defaults to 128000.
        LANGUAGE_<TIER>_{INPUT,OUTPUT,CACHED_INPUT}_COST_PER_MILLION_TOKENS
                                      Optional, defaults to 0 (unknown, not free).
    There is no cross-tier fallback and no process-wide LLM_PROVIDER_*
    defaults (both removed 2026-08-28). Fixture-only turns (a row in
    _test_scripted_responses) never touch any of these. The AsyncOpenAI
    client is cached per (base_url, api_key) pair by llm_client.py, so a
    deployment where all three tiers point at the same provider still
    gets one shared HTTP connection pool for free.
    METRICS_BIND_ADDRESS Host:port the Prometheus exposition endpoint listens
                         on. Default: 0.0.0.0:9090. See
                         docs/components/budget-guardrails.md, "Resolved:
                         Metrics Export" — plain scrape, no ServiceMonitor.
    AGENT_BRAIN_BASE_URL/AGENT_BRAIN_API_KEY/AGENT_BRAIN_AGENT_ID
                         docs/components/memory-slot.md's memory backend
                         (agent_brain.py, tools.py's memory_search/
                         memory_expand, write_memory.py). Not required —
                         a session that never touches memory works fine
                         without these set; memory_search/memory_expand/
                         WriteMemory all degrade to a no-op (or, for a
                         mid-session tool call, a clear error observation)
                         rather than failing the turn.
    MCP_HUB_URL          docs/components/tool-registry.md's mcp-hub-mediated
                         tool tier (mcp_hub.py, tools.py's search_tools/
                         call_tool). Not required — search_tools degrades to
                         shell-hub-only results if unset.
    EMBEDDING_BASE_URL/EMBEDDING_API_KEY/EMBEDDING_MODEL/EMBEDDING_DIM
                         shell-hub's own embedding provider (shell_hub.py) —
                         reuses mcp-hub's own LiteLLM config, not a separate
                         credential. Not required — shell_hub.search()
                         degrades to returning no results if unset.

Usage:
    python -m activities.tenant_worker
"""

from __future__ import annotations

import asyncio
import logging
import os

from temporalio.client import Client
from temporalio.runtime import PrometheusConfig, Runtime, TelemetryConfig
from temporalio.worker import Worker

from . import llm_client, shell_hub
from .metrics import LATENCY_BUCKETS_SECONDS, SECONDS_LATENCY_METRICS
from .classify import ClassifyRequestActivity
from .episode import EpisodeActivities
from .skills import seed as skill_seed
from .skills.record import RecordSkillOutcomeActivity
from .skills.synthesize import SkillSynthesizeActivity
from .compress_context import CompressContextActivity
from .db import create_pool
from .deliver import DeliverActivity
from .get_max_turn_seq import GetMaxTurnSeqActivity
from .insert_message import InsertMessageActivity
from .model_call import ModelCallActivity
from .persist import PersistActivity
from .retrieval import (
    ComposeSkillActivity,
    MemoryRetrieveActivity,
    SkillDiscoverActivity,
    ToolDiscoverActivity,
)
from .seed_child_session import SeedChildSessionContextActivity
from .subagent_manifest import SubagentManifestActivity
from .tool_call import DenyToolCallActivity, ToolCallActivity
from .user_input import CloseUserInputActivity, RequestUserInputActivity
from .write_memory import WriteMemoryActivity


def _episode_activities(pool):
    """docs/components/episode-lifecycle.md — EpisodeActivities holds four
    @activity.defn methods (OpenEpisode / CompleteEpisode / CloseSubagentEpisode
    / CloseSessionEpisodes); register each bound method."""
    ea = EpisodeActivities(pool)
    return [ea.open_episode, ea.complete_episode, ea.close_subagent_episode, ea.close_session_episodes]


async def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")

    address = os.environ.get("TEMPORAL_ADDRESS", "localhost:7233")
    namespace = os.environ.get("TEMPORAL_NAMESPACE", "default")
    task_queue = os.environ.get("TEMPORAL_TASK_QUEUE", "agent-loop")

    pool = await create_pool()
    # docs/components/tool-registry.md, "Resolved: Native-Tool Discovery" —
    # builds shell_hub's in-process zvec index once at startup.
    # No-op if shell_hub.CATALOG is empty or EMBEDDING_BASE_URL isn't set.
    await shell_hub.init()
    # docs/components/skill-subsystem.md phase 1 — load the authored seed
    # procedures into skill_procedures. Idempotent; embeds only new/changed
    # seeds. No-op if EMBEDDING_BASE_URL isn't set (seeds present but not
    # retrievable until it is).
    await skill_seed.init(pool)
    # AsyncOpenAI clients are no longer constructed here (2026-08-28,
    # per-tier provider revision) — every activity that needs one
    # resolves it via llm_client.get_client(model_config), keyed on the
    # tier's own base_url/api_key. See docs/components/model-registry.md.
    # The cache in llm_client keeps the connection-pool benefit for the
    # common case where every configured tier points at the same provider.

    # docs/components/budget-guardrails.md, "Resolved: Metrics Export" —
    # temporalio's built-in Prometheus support: no new dependency, no
    # bespoke registry. Emits the SDK's own built-in activity/workflow
    # metrics for free, plus whatever ModelCallActivity/ToolCallActivity
    # record via activity.metric_meter(). Scraped directly (plain
    # prometheus.io/scrape pod annotations — no ServiceMonitor).
    # The SDK's own duration metrics (temporal_activity_execution_latency etc.)
    # are milliseconds and keep the core default boundaries. Our three
    # hand-rolled histograms record *seconds* (ModelCall/ToolCall/Classify
    # measure a provider round-trip, where seconds is the natural unit) — the
    # ms-oriented default boundaries would collapse every real value into the
    # first bucket, so widen them by name here. Keep in sync with
    # metrics.SECONDS_LATENCY_METRICS / LATENCY_BUCKETS_SECONDS.
    runtime = Runtime(
        telemetry=TelemetryConfig(
            metrics=PrometheusConfig(
                bind_address=os.environ.get("METRICS_BIND_ADDRESS", "0.0.0.0:9090"),
                histogram_bucket_overrides={
                    name: list(LATENCY_BUCKETS_SECONDS) for name in SECONDS_LATENCY_METRICS
                },
            )
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
            ModelCallActivity(pool, client).__call__,
            ClassifyRequestActivity(pool).__call__,
            *_episode_activities(pool),
            MemoryRetrieveActivity(pool).__call__,
            ToolDiscoverActivity(pool).__call__,
            SkillDiscoverActivity(pool).__call__,
            ComposeSkillActivity(pool).__call__,
            RecordSkillOutcomeActivity(pool).__call__,
            SkillSynthesizeActivity(pool).__call__,
            ToolCallActivity(pool).__call__,
            InsertMessageActivity(pool).__call__,
            GetMaxTurnSeqActivity(pool).__call__,
            PersistActivity(pool).__call__,
            DeliverActivity(pool).__call__,
            WriteMemoryActivity(pool).__call__,
            CompressContextActivity(pool).__call__,
            DenyToolCallActivity(pool).__call__,
            RequestUserInputActivity(pool).__call__,
            CloseUserInputActivity(pool).__call__,
            SeedChildSessionContextActivity(pool).__call__,
            SubagentManifestActivity(pool).__call__,
        ],
    )
    logging.getLogger(__name__).info(
        "tenant worker starting: temporal=%r namespace=%r task_queue=%r", address, namespace, task_queue
    )
    try:
        await worker.run()
    finally:
        await llm_client.close_all()
        await pool.close()


if __name__ == "__main__":
    asyncio.run(main())
