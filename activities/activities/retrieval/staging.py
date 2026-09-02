"""Shared helpers for the `turn_retrieval` staging table
(docs/components/request-pipeline/03-routing.md), episode-scoped
(docs/components/episode-lifecycle.md — the `turn_id` key column was renamed
to `episode_id`; the table name was kept).

The retrieval subsystems write their ranked results here, keyed by
`episode_id` (== the anchor turn_id); `ComposeSkill` and later the planner /
prompt assembly read them. One row set per episode — every turn of the
episode reads the same bundle, and only the reconcile pass (a continuation
turn, or a mid-turn correction) replaces it. Workflows carry only an
`episode_id` reference plus per-subsystem status — the content lives here.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field


@dataclass
class RetrievalRow:
    kind: str  # 'memory' | 'tool' | 'skill' | 'composed'
    seq: int  # rank within (episode_id, kind)
    content: str
    score: float | None = None
    metadata: dict = field(default_factory=dict)


async def write_rows(pool, episode_id: str, rows: list[RetrievalRow]) -> int:
    """Upsert one subsystem's staged rows. Idempotent per (episode_id, kind,
    seq) so a Temporal activity retry re-writes rather than colliding.
    Returns the number of rows written."""
    if not rows:
        return 0
    await pool.executemany(
        "INSERT INTO turn_retrieval (episode_id, kind, seq, content, score, metadata) "
        "VALUES ($1, $2, $3, $4, $5, $6) "
        "ON CONFLICT (episode_id, kind, seq) DO UPDATE SET "
        "  content = EXCLUDED.content, score = EXCLUDED.score, "
        "  metadata = EXCLUDED.metadata, created_at = now()",
        [
            (episode_id, r.kind, r.seq, r.content, r.score, json.dumps(r.metadata))
            for r in rows
        ],
    )
    return len(rows)


async def replace_rows(pool, episode_id: str, kind: str, rows: list[RetrievalRow]) -> int:
    """Atomically swap ALL rows of one kind for an episode — delete then re-write
    in a single transaction. Used by the reconciliation pass (episode-lifecycle.md
    / request-pipeline/08-planning.md): re-keyed retrieval replaces the stale
    bundle rather than appending to it, so the rendered block stays bounded
    across repeated corrections and continuation turns. Returns the number of
    rows written."""
    async with pool.acquire() as conn:
        async with conn.transaction():
            await conn.execute("DELETE FROM turn_retrieval WHERE episode_id = $1 AND kind = $2", episode_id, kind)
            if rows:
                await conn.executemany(
                    "INSERT INTO turn_retrieval (episode_id, kind, seq, content, score, metadata) "
                    "VALUES ($1, $2, $3, $4, $5, $6)",
                    [(episode_id, r.kind, r.seq, r.content, r.score, json.dumps(r.metadata)) for r in rows],
                )
    return len(rows)


async def read_rows(pool, episode_id: str, kinds: tuple[str, ...]) -> list[RetrievalRow]:
    """Read staged rows for an episode, ordered by (kind, seq)."""
    records = await pool.fetch(
        "SELECT kind, seq, content, score, metadata FROM turn_retrieval "
        "WHERE episode_id = $1 AND kind = ANY($2::text[]) "
        "ORDER BY kind, seq",
        episode_id,
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
