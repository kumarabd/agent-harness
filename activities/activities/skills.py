"""skills.py — docs/05-architecture-domain-control-loops.md, docs/components/
turn-pipeline.md ("Skills"). The per-tenant registry of authored domain
workflows ("skills") this tenant has enabled — sourced from this tenant's own
`skills` table (migration 040_skills.sql), not a hardcoded list.

2026-09-27: moved off a hardcoded Python list (and the hand-synced Go mirror,
workflows/internal/workflow/skills/registry.go, deleted alongside this) —
same precedent mcp-hub's own `connections` table already set for backend
config (static YAML manifests -> per-tenant Postgres-backed catalog,
2026-09-26). Every tenant's image used to ship the exact same global list,
with no way to enable/disable one for a single tenant without a code change;
tenant-worker and Gateway already each hold a connection to this tenant's own
Postgres (POSTGRES_HOST, both charts' own env), so there's no new cross-
service plumbing needed to make the catalog per-tenant. Gateway's own GET
/skills now queries the same table directly (workflows/internal/gateway/
web/skills.go) instead of hand-syncing a second copy.

Each row's `name` must have a matching Go workflow function in
workflows/internal/workflow/skills/ and a
`w.RegisterWorkflowWithOptions(skillswf.<WorkflowType>, workflow.RegisterOptions{Name: "<name>"})`
line in workflows/cmd/loop-worker/main.go — `name` IS the Temporal workflow
type (turn.go dispatches ExecuteChildWorkflow by this string directly). A row
without a matching registered Go workflow type just fails to dispatch, same
as a stale entry in the old hardcoded list would have.

Hand-authored only, never auto-populated or learned from a transcript
(docs/components/turn-pipeline.md, "Skill recording": "There is none, and
there won't be.") — this module just moved WHERE the hand-authoring lives
(a database row instead of a Python literal), not who authors it or how a new
skill gets registered.
"""

from __future__ import annotations

import json

import asyncpg

# Populated once at tenant-worker startup by init(pool) — empty until then.
# Mutated in place (not reassigned) so a module that grabbed a reference at
# import time (skill_hub.py's `from . import skills`) sees the real contents
# without needing its own init() call ordered just so; still, every real
# consumer's own docs should say it depends on init() having already run.
SKILLS: list[dict] = []


async def init(pool: asyncpg.Pool) -> list[dict]:
    """Loads this tenant's enabled skills from its own `skills` table,
    populates the module-level SKILLS list in place, and returns the same
    entries — callers that need to rebuild their OWN derived state from a
    fresh load (capabilities.load_skills, llm.load_skills) take the return
    value directly rather than re-reading the module global themselves, so
    there's exactly one query per startup regardless of how many consumers
    need the result.

    Call once, at tenant-worker startup, before capabilities.load_skills/
    llm.load_skills/skill_hub.init (tenant_worker.py's own ordering) — a
    ModelCall dispatched before this resolves would see zero skills, the
    same "just isn't offered yet" degrade shell_hub/skill_hub already have
    for their own not-yet-initialized case.
    """
    async with pool.acquire() as conn:
        rows = await conn.fetch(
            "SELECT name, description, input_schema FROM skills WHERE enabled ORDER BY name"
        )
    entries = [
        {
            "name": row["name"],
            "description": row["description"],
            "input_schema": json.loads(row["input_schema"]),
        }
        for row in rows
    ]
    SKILLS[:] = entries
    return entries
