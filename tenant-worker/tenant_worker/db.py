"""Postgres connection-pool access for the activity layer.

Real design: every content-touching activity (ModelCall, ToolCall, the
start-of-turn message insert) reads/writes directly against that tenant's own
Postgres instance (docs/components/temporal-workflow.md, "Resolved:
Reference-Passing Contract"; docs/components/multi-tenancy.md). This module
owns the one connection pool for the process — created once in worker.py,
injected into activity instances rather than held as module-global state, so
there's exactly one lifecycle to reason about.

Env vars (already set by deploy/helm/agent-harness-tenant/templates/tenant-worker-deployment.yaml):
    POSTGRES_HOST, POSTGRES_PORT, POSTGRES_DB, POSTGRES_USER, POSTGRES_PASSWORD
"""

from __future__ import annotations

import os

import asyncpg


async def create_pool() -> asyncpg.Pool:
    """Create the process-wide connection pool from POSTGRES_* env vars."""
    pool = await asyncpg.create_pool(
        host=os.environ.get("POSTGRES_HOST", "localhost"),
        port=int(os.environ.get("POSTGRES_PORT", "5432")),
        database=os.environ.get("POSTGRES_DB", "agent_harness"),
        user=os.environ.get("POSTGRES_USER", "agent_harness"),
        password=os.environ.get("POSTGRES_PASSWORD", ""),
        min_size=1,
        max_size=10,
    )
    if pool is None:
        raise RuntimeError("asyncpg.create_pool returned None")
    return pool
