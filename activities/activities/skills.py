"""skills.py — docs/05-architecture-domain-control-loops.md. The per-tenant
registry of authored domain workflows ("skills") this tenant has enabled —
sourced from this tenant's own `skills` table (migration 040_skills.sql), not
a hardcoded list.

2026-09-27: a "skill" is entirely a mode now — journaling and
service_monitoring both migrated onto the session-mode mechanism
(tools.switch_mode), and the one-shot skill dispatch shape they used before
(a nested child workflow, dispatched by name at ModelCall mint time) was
deleted outright rather than kept as a second, redundant mechanism. This
table's own role narrowed to match: a row's `name` must be a key in
activities/activities/llm.py's MODE_TURNS (the fixed, deployment-wide fact of
which names are real turn-shaped Go dispatch targets — workflows/internal/
workflow/mode_journaling.go, mode_service_monitoring.go), and `enabled` is
purely this tenant's own opt-in/opt-out gate on top of that
(llm.load_skills intersects the two into llm.ENABLED_MODES, which
tools.switch_mode reads). Gateway's own GET /skills queries this same table
directly (workflows/internal/gateway/web/skills.go).

Hand-authored only, never auto-populated or learned from a transcript
(docs/components/turn-pipeline.md, "Skill recording": "There is none, and
there won't be.") — this module just moved WHERE the hand-authoring lives
(a database row instead of a Python literal), not who authors it or how a new
mode gets registered.
"""

from __future__ import annotations

import json

import asyncpg

# Populated once at tenant-worker startup by init(pool) — empty until then.
# Mutated in place (not reassigned) so a module that grabbed a reference at
# import time sees the real contents without needing its own init() call
# ordered just so; still, every real consumer's own docs should say it
# depends on init() having already run.
SKILLS: list[dict] = []


async def init(pool: asyncpg.Pool) -> list[dict]:
    """Loads this tenant's enabled skills (mode-gate rows) from its own
    `skills` table, populates the module-level SKILLS list in place, and
    returns the same entries — llm.load_skills takes the return value
    directly rather than re-reading the module global itself, so there's
    exactly one query per startup.

    Call once, at tenant-worker startup, before llm.load_skills
    (tenant_worker.py's own ordering) — a ModelCall dispatched before this
    resolves would see zero enabled modes, the same "just isn't offered yet"
    degrade shell_hub already has for its own not-yet-initialized case.
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
