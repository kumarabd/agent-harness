"""Compression — docs/components/context-slot.md, "Resolved: Duties and
Strategies" #3, Three-Level Escalation (Ehrlich & Blackman 2026). Real body
for CompressContext, replacing the no-op stub it used to be.
"""

from __future__ import annotations

import logging

from .assembly import session_messages
from .constants import LEAF_FOLD_THRESHOLD, VERBATIM_WINDOW_MESSAGES, estimate_tokens

logger = logging.getLogger(__name__)


def compression_state(context_tokens: int, soft_threshold: int, hard_threshold: int) -> str:
    """docs/components/context-slot.md, "Resolved: Duties and Strategies" #3
    — two-tier threshold, not one constant. Returns "none"/"soft"/"hard";
    the caller (turn.go) decides what soft (async, don't block) vs hard
    (block until done) actually means — this only classifies."""
    if context_tokens >= hard_threshold:
        return "hard"
    if context_tokens >= soft_threshold:
        return "soft"
    return "none"


async def compact(conn, session_key: str, provider, model: str) -> None:
    """Summarizes the oldest not-yet-covered span of messages (everything
    outside the verbatim window assemble() shows) into a new leaf
    context_summaries row, then folds accumulated leaves into a condensed
    summary once LEAF_FOLD_THRESHOLD of them exist.

    Level 1: LLM summarize, preserve detail. Level 2 (only if Level 1's
    output isn't actually shorter than its input — "compaction failure",
    the real, previously-unnamed failure mode this design names): LLM
    summarize, aggressive bullet points. Level 3 (only if Level 2 also
    fails to shrink): deterministic truncate, no LLM — always shrinks,
    guaranteeing convergence regardless of what either LLM call produces.
    """
    messages = await session_messages(conn, session_key)
    covered_rows = await conn.fetch(
        "SELECT unnest(covers) AS message_id FROM context_summaries "
        "WHERE session_key = $1 AND kind = 'leaf' AND folded_into IS NULL",
        session_key,
    )
    covered_ids = {row["message_id"] for row in covered_rows}
    uncovered = [m for m in messages if m["message_id"] not in covered_ids]

    span = uncovered[:-VERBATIM_WINDOW_MESSAGES] if len(uncovered) > VERBATIM_WINDOW_MESSAGES else []
    if not span:
        logger.info("lcm.compaction.compact[%s]: nothing new to compact", session_key)
        return

    transcript = "\n".join(f"{m['role']}: {m['content']}" for m in span if m["content"])
    content = await _escalating_summarize(provider, model, transcript)

    await conn.execute(
        "INSERT INTO context_summaries (session_key, kind, covers, content, token_count) "
        "VALUES ($1, 'leaf', $2, $3, $4)",
        session_key,
        [m["message_id"] for m in span],
        content,
        estimate_tokens(content),
    )
    logger.info("lcm.compaction.compact[%s]: wrote leaf summary covering %d messages", session_key, len(span))

    await _fold_leaves_if_due(conn, session_key, provider, model)


async def _escalating_summarize(provider, model: str, transcript: str) -> str:
    input_tokens = estimate_tokens(transcript)

    content = await _summarize(provider, model, transcript, aggressive=False)
    if estimate_tokens(content) < input_tokens:
        return content

    content = await _summarize(provider, model, transcript, aggressive=True)
    if estimate_tokens(content) < input_tokens:
        return content

    # Level 3: deterministic, always shrinks — guarantees convergence
    # regardless of what either LLM call produced.
    return transcript[: max(1, len(transcript) // 4)] + " ...[truncated]"


async def _summarize(provider, model: str, transcript: str, aggressive: bool) -> str:
    instruction = (
        "Summarize this conversation span as terse bullet points, maximally "
        "compressed, losing detail if needed to be short."
        if aggressive
        else "Summarize this conversation span, preserving important details."
    )
    result = await provider.summarize_text(
        system_prompt=instruction,
        user_content=transcript,
        model=model,
    )
    return result.content


async def _fold_leaves_if_due(conn, session_key: str, provider, model: str) -> None:
    """Recursively folds the summary DAG once any level crosses
    LEAF_FOLD_THRESHOLD — leaves first, then repeatedly condensed-of-condensed
    while condensed count is still at/above threshold (a leaf-fold can itself
    push condensed count over threshold, and folding condensed rows produces
    one new condensed row, which could — over a long enough session — push
    it over threshold again).

    Found necessary via a real conversation about long-running-session cost,
    not assumed: an earlier version only ever folded 'leaf' rows — condensed
    rows were never re-folded, so `context_summaries` for a session that
    never resets would accumulate an ever-growing flat list of condensed
    rows, and assemble() sums every one of their token_count unconditionally
    into every call's context. That's the opposite of what "hierarchical"
    (LCM §2.1's summary-of-summaries DAG) was supposed to buy — DAG height
    should grow logarithmically with total summary count, not stay flat
    while the row count grows linearly forever."""
    await _fold_kind_if_due(conn, session_key, provider, model, "leaf")
    while await _fold_kind_if_due(conn, session_key, provider, model, "condensed"):
        pass


async def _fold_kind_if_due(conn, session_key: str, provider, model: str, kind: str) -> bool:
    """Folds every not-yet-folded `kind` row into one new 'condensed' row,
    if at least LEAF_FOLD_THRESHOLD of them exist. Returns whether a fold
    happened, so the recursive condensed-of-condensed loop above knows
    whether to keep going.

    Real, live bug found and fixed 2026-08-29, while building lcm.retrieval's
    expand() — folded rows are marked `folded_into` the new condensed row's
    id, NOT deleted (the original design's behavior). A folded leaf's own
    summary_id is still referenced by its new parent condensed row's
    `covers` array — deleting it would leave that reference dangling,
    meaning any future attempt to walk BACK DOWN the DAG (expand() doing
    exactly that: condensed -> its covers, i.e. the leaves that were folded
    into it -> those leaves' own covers, i.e. the real message_ids) would
    hit nothing and silently return an empty result instead of the real
    original content. That's not a hypothetical: any real session that's
    ever crossed LEAF_FOLD_THRESHOLD (5 leaf summaries) already has this
    gap in production today, independent of whether expand() gets called —
    the DELETE already ran, the rows are already gone. This fix only
    prevents it from recurring for future folds; it doesn't (can't)
    retroactively recover already-deleted rows for sessions that folded
    before this fix shipped.

    assemble()'s own query (assembly.py) and this function's own SELECT
    below both now filter `folded_into IS NULL` — a folded row must stop
    counting toward "the current DAG frontier" (what assemble() shows, what
    a later fold pass considers for re-folding) even though it physically
    still exists in the table for expand() to find."""
    rows = await conn.fetch(
        "SELECT summary_id, content FROM context_summaries "
        "WHERE session_key = $1 AND kind = $2 AND folded_into IS NULL ORDER BY created_at",
        session_key,
        kind,
    )
    if len(rows) < LEAF_FOLD_THRESHOLD:
        return False

    combined = "\n".join(row["content"] for row in rows)
    condensed_content = await _escalating_summarize(provider, model, combined)

    async with conn.transaction():
        new_id = await conn.fetchval(
            "INSERT INTO context_summaries (session_key, kind, covers, content, token_count) "
            "VALUES ($1, 'condensed', $2, $3, $4) RETURNING summary_id",
            session_key,
            [row["summary_id"] for row in rows],
            condensed_content,
            estimate_tokens(condensed_content),
        )
        await conn.execute(
            "UPDATE context_summaries SET folded_into = $2 WHERE summary_id = ANY($1::uuid[])",
            [row["summary_id"] for row in rows],
            new_id,
        )
    logger.info(
        "lcm.compaction._fold_kind_if_due[%s]: folded %d %r summaries into condensed summary %s",
        session_key, len(rows), kind, new_id,
    )
    return True
