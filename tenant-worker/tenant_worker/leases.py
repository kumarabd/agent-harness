"""Session-directory leases — coordinates concurrent tool-call access to the
shared session filesystem PV (docs/components/session-filesystem.md,
"Resolved: Lease Renewal, Not Fixed TTL"; docs/components/state-layer.md's
`session_filesystem_leases` table).

Granularity is the session/subagent-*directory* level (not per-file — the
docs' own deliberate starting point, refined later only if real contention
patterns warrant it), so `path` here is exactly a `ids.session_fs_path(...)`
value. A lease is renewed periodically by whoever holds it (paired with the
same heartbeat tick a tool activity already does for cancellation — see
tools.py's `ToolContext.tick()`), not granted for a fixed TTL up front: it
only expires if renewal genuinely stops, mirroring the project's existing
"no ABANDON" cooperative-cancellation philosophy rather than introducing a
second, inconsistent timeout model.

`content_hash`/`last_writer` on the table are intentionally left unused by
this module — they belong to the future subagent merge-back mechanism
(session-filesystem.md's conflict-detection design), not to basic
acquire/renew/release.
"""

from __future__ import annotations

import asyncpg


async def acquire_or_renew(
    pool: asyncpg.Pool,
    session_key: str,
    path: str,
    holder_id: str,
    ttl_seconds: float,
) -> bool:
    """Acquire `path` for `holder_id`, or renew it if already held by the same
    holder. Fails (returns False, no error) if genuinely held by a different,
    non-expired holder — the caller decides whether/how to wait and retry;
    this function never blocks.
    """
    row = await pool.fetchrow(
        """
        INSERT INTO session_filesystem_leases (session_key, path, holder_id, expires_at)
        VALUES ($1, $2, $3, now() + make_interval(secs => $4))
        ON CONFLICT (session_key, path) DO UPDATE
            SET holder_id = EXCLUDED.holder_id,
                acquired_at = CASE WHEN session_filesystem_leases.holder_id = EXCLUDED.holder_id
                                   THEN session_filesystem_leases.acquired_at ELSE now() END,
                expires_at = EXCLUDED.expires_at
            WHERE session_filesystem_leases.holder_id = EXCLUDED.holder_id
               OR session_filesystem_leases.expires_at < now()
        RETURNING holder_id
        """,
        session_key,
        path,
        holder_id,
        ttl_seconds,
    )
    return row is not None and row["holder_id"] == holder_id


async def release(pool: asyncpg.Pool, session_key: str, path: str, holder_id: str) -> None:
    """Release a lease this holder currently holds. A no-op if it was already
    reclaimed by someone else after expiring — never errors on that."""
    await pool.execute(
        "DELETE FROM session_filesystem_leases WHERE session_key = $1 AND path = $2 AND holder_id = $3",
        session_key,
        path,
        holder_id,
    )
