"""Tenant skill enablement, read from the skills table at worker startup.

Domain definitions live in the skills package. This module only loads enabled
catalog entries; llm.load_skills intersects them with the registered names for
chat's descriptions. Selection and command validation also check the live tenant
gate. Gateway GET /skills queries the same catalog. Names are user selections,
not top-level workflow dispatch targets.
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
