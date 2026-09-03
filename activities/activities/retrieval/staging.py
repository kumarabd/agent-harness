"""Shared helpers for the `turn_retrieval` staging table.

REVISED 2026-09-02: the key column is `owner_id` — "whichever unit owns this
row". MemoryRetrieve and ToolDiscover run once per TURN and stage under the
current turn's id; SkillDiscover runs once per task-run and stages under the
plan_id (the planning turn's id). Workflows carry only an id reference plus
per-subsystem status — the content lives here.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field


@dataclass
class RetrievalRow:
    kind: str  # 'memory' | 'tool' | 'skill' | 'composed'
    seq: int  # rank within (owner_id, kind)
    content: str
    score: float | None = None
    metadata: dict = field(default_factory=dict)


async def write_rows(pool, owner_id: str, rows: list[RetrievalRow]) -> int:
    """Upsert one subsystem's staged rows. Idempotent per (owner_id, kind, seq)
    so a Temporal activity retry re-writes rather than colliding. Returns the
    number of rows written."""
    if not rows:
        return 0
    await pool.executemany(
        "INSERT INTO turn_retrieval (owner_id, kind, seq, content, score, metadata) "
        "VALUES ($1, $2, $3, $4, $5, $6) "
        "ON CONFLICT (owner_id, kind, seq) DO UPDATE SET "
        "  content = EXCLUDED.content, score = EXCLUDED.score, "
        "  metadata = EXCLUDED.metadata, created_at = now()",
        [
            (owner_id, r.kind, r.seq, r.content, r.score, json.dumps(r.metadata))
            for r in rows
        ],
    )
    return len(rows)


async def read_rows(pool, owner_id: str, kinds: tuple[str, ...]) -> list[RetrievalRow]:
    """Read staged rows for an owner, ordered by (kind, seq)."""
    records = await pool.fetch(
        "SELECT kind, seq, content, score, metadata FROM turn_retrieval "
        "WHERE owner_id = $1 AND kind = ANY($2::text[]) "
        "ORDER BY kind, seq",
        owner_id,
        list(kinds),
    )
    return [
        RetrievalRow(
            kind=r["kind"],
            seq=r["seq"],
            content=r["content"],
            score=r["score"],
            metadata=json.loads(r["metadata"]) if r["metadata"] else {},
        )
        for r in records
    ]
