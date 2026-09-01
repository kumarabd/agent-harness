"""Shared helper for the reconciliation pass — request pipeline step 8
(docs/components/request-pipeline/08-planning.md, "Reconciliation trigger").

When a user correction lands mid-turn, `RoutingWorkflow` re-runs in
`mode="reconcile"`: memory + skill discovery only (no tools, no plan re-seed),
re-keyed on the correction and replacing the stale bundle. The correction text
is read here, in-activity, so no message content crosses the workflow boundary.
"""

from __future__ import annotations


async def reconcile_query(pool, turn_id: str, base_query: str) -> str:
    """The original `retrieval_query` joined with the turn's latest user message
    — the correction that triggered reconciliation. Falls back to `base_query`
    alone when there's no distinct newer message."""
    base = (base_query or "").strip()
    row = await pool.fetchrow(
        "SELECT content FROM messages WHERE parent_id = $1 AND role = 'user' ORDER BY seq DESC LIMIT 1",
        turn_id,
    )
    latest = (row["content"].strip() if row and row["content"] else "")
    if not latest or latest == base:
        return base
    return f"{base} / {latest}".strip(" /") if base else latest
