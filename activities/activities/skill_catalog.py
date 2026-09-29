"""Tenant skill enablement, read from the skills table at worker startup and model calls.

Domain definitions live in the skills package. This module only loads enabled
catalog entries; llm.load_skills intersects them with the registered names for
chat's descriptions. Selection and command validation also check the live tenant
gate. Gateway GET /skills queries the same catalog. Names are user selections,
not top-level workflow dispatch targets.
"""

from __future__ import annotations

import json

import asyncpg

# Populated at tenant-worker startup and refreshed before each real model call.
# Mutated in place (not reassigned) so a module that grabbed a reference at
# import time sees the real contents without needing its own init() call
# ordered just so; still, every real consumer's own docs should say it
# depends on init() having already run.
SKILLS: list[dict] = []


async def init(pool: asyncpg.Pool) -> list[dict]:
    """Prime the tenant's enabled skills before the worker starts polling."""
    async with pool.acquire() as conn:
        return await refresh(conn)


async def refresh(conn: asyncpg.Connection) -> list[dict]:
    """Read the live enablement gate before constructing a model tool schema."""
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
