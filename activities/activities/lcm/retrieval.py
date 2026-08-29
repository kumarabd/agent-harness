"""Memory-Access Tools — the LCM paper's (Ehrlich & Blackman 2026) Appendix
C.1, `lcm_grep`/`lcm_describe`/`lcm_expand`. Scoped deliberately narrower
than the paper's own "files or summaries" framing: agent-harness's large-tool-
output storage (claim_check.py) already has a full, independent recovery
path via shell_exec (cat/head/tail/grep against a real path on the session
filesystem) — building a second route to the same content here would be
redundant plumbing. These three functions act ONLY on the context DAG
(context_summaries) and the raw conversational record (messages); they never
touch claim-check files.

Every function here is session-scoped — takes session_key (grep, indirectly
via the id it's asked to describe/expand belonging to a session's own DAG for
describe/expand), never reaches across sessions. That boundary is memory-slot.md's
job (agent-brain), not this package's.
"""

from __future__ import annotations

import uuid

from .assembly import session_messages
from .constants import GREP_DEFAULT_LIMIT


class LCMNotFoundError(ValueError):
    """Raised when an id passed to describe()/expand() doesn't resolve to
    anything — a plain ValueError subclass, not a new exception hierarchy,
    since tool_call.py's existing exception-to-status='error' handling
    already turns any handler exception into a clear message for the model;
    no special-casing needed at the dispatch layer for this to work."""


def _parse_id(id_str: str, tool_name: str) -> uuid.UUID:
    try:
        return uuid.UUID(id_str)
    except (ValueError, AttributeError, TypeError) as exc:
        raise LCMNotFoundError(f"{tool_name}: {id_str!r} is not a valid id") from exc


async def grep(conn, session_key: str, pattern: str, mode: str = "pattern", limit: int | None = None) -> dict:
    """docs/components/context-slot.md's Memory-Access Tools — `lcm_grep`.

    mode="pattern" (default): real Postgres regex (~), literal pattern
    matching — matches this tool's own name honestly, not a rebrand of
    keyword search. mode="fulltext": the messages table's own real,
    already-indexed search_vector column (a GIN-indexed tsvector, built and
    live since 001_initial_schema.sql, unused by any code until this) —
    stemmed English keyword search, genuinely different semantics from
    regex (tolerant of word forms, ranked by relevance), for a natural-
    language query pattern doesn't fit. NOT true semantic/embedding search
    — messages has no vector embedding column, and adding one is real new
    infrastructure (a migration, an embed-on-write call) out of scope here;
    named directly so "fulltext" isn't mistaken for "semantic."

    limit: no system-enforced hard ceiling — the model can ask for as many
    results as it has real reason to want. GREP_DEFAULT_LIMIT (20) applies
    only when the caller omits the argument entirely, so an unscoped query
    doesn't accidentally flood context by default (the real, stated reason
    the source paper gives for pagination) without the model ever having
    decided that's what it wanted.

    Results report which summary node (if any) NEAREST covers each matched
    message — the leaf that directly covers it, or, if that leaf has since
    been folded, the leaf's immediate parent (never chased further up even
    if that parent has itself since been folded again — see
    _active_covering_summary_ids's own docstring for why walking to the
    topmost surviving ancestor was tried first and found to throw away
    exactly the region-level specificity this tool exists to surface). A
    message not (yet) covered by anything is still sitting in assemble()'s
    own raw verbatim window or the not-yet-compacted tail beyond it —
    reports covered_by_summary_id: null, meaning "already visible in
    context as-is, nothing to expand."
    """
    if mode not in ("pattern", "fulltext"):
        raise ValueError(f"lcm_grep: mode must be 'pattern' or 'fulltext', got {mode!r}")
    effective_limit = limit if limit is not None else GREP_DEFAULT_LIMIT

    if mode == "pattern":
        match_clause = "m.content ~ $2"
        order_clause = "t.turn_seq, m.seq"
    else:
        match_clause = "m.search_vector @@ plainto_tsquery('english', $2)"
        order_clause = "ts_rank(m.search_vector, plainto_tsquery('english', $2)) DESC"

    # LIMIT $3+1: fetch one extra row purely to detect "there were more
    # matches than the limit" without a separate COUNT(*) query.
    rows = await conn.fetch(
        f"""
        SELECT m.message_id, m.role, m.content, t.turn_seq
        FROM turns t JOIN messages m ON m.parent_id = t.turn_id
        WHERE t.parent_id = $1 AND t.parent_type = 'session' AND {match_clause}
        ORDER BY {order_clause}
        LIMIT $3
        """,
        session_key,
        pattern,
        effective_limit + 1,
    )
    truncated = len(rows) > effective_limit
    rows = rows[:effective_limit]

    covering = await _active_covering_summary_ids(conn, session_key, [r["message_id"] for r in rows])

    return {
        "results": [
            {
                "message_id": str(r["message_id"]),
                "role": r["role"],
                "content": r["content"],
                "turn_seq": r["turn_seq"],
                "covered_by_summary_id": covering.get(r["message_id"]),
            }
            for r in rows
        ],
        "truncated": truncated,
    }


async def describe(conn, id: str) -> dict:
    """docs/components/context-slot.md's Memory-Access Tools — `lcm_describe`.
    Auto-detects whether id names a raw message or a summary node — the two
    tables' primary keys are both plain uuids with no overlapping namespace,
    so a single lookup-then-lookup (message first, summary second) resolves
    it unambiguously without the caller needing to know which kind of thing
    they have."""
    parsed = _parse_id(id, "lcm_describe")

    message_row = await conn.fetchrow(
        "SELECT m.message_id, m.role, m.content, t.turn_id, t.turn_seq, t.parent_id AS session_key "
        "FROM messages m JOIN turns t ON m.parent_id = t.turn_id WHERE m.message_id = $1",
        parsed,
    )
    if message_row:
        covering = await _active_covering_summary_ids(conn, message_row["session_key"], [parsed])
        return {
            "kind": "message",
            "message_id": str(message_row["message_id"]),
            "role": message_row["role"],
            "content": message_row["content"],
            "turn_id": message_row["turn_id"],
            "turn_seq": message_row["turn_seq"],
            "session_key": message_row["session_key"],
            "covered_by_summary_id": covering.get(parsed),
        }

    summary_row = await conn.fetchrow(
        "SELECT summary_id, session_key, kind, covers, content, token_count, created_at, folded_into "
        "FROM context_summaries WHERE summary_id = $1",
        parsed,
    )
    if summary_row:
        return {
            "kind": "summary",
            "summary_id": str(summary_row["summary_id"]),
            "session_key": summary_row["session_key"],
            "summary_kind": summary_row["kind"],  # 'leaf' | 'condensed'
            "covers": [str(c) for c in summary_row["covers"]],
            "content": summary_row["content"],
            "token_count": summary_row["token_count"],
            "created_at": summary_row["created_at"].isoformat(),
            "folded_into": str(summary_row["folded_into"]) if summary_row["folded_into"] else None,
        }

    raise LCMNotFoundError(f"lcm_describe: no message or summary found with id {id!r}")


async def expand(conn, summary_id: str) -> dict:
    """docs/components/context-slot.md's Memory-Access Tools — `lcm_expand`.
    Recovers the full, real original messages a summary node (leaf OR
    condensed) ultimately represents — this is what makes compaction
    genuinely reversible rather than a one-way, lossy operation. A leaf's
    own `covers` is already message_ids, resolved directly; a condensed
    row's `covers` is child summary_ids (which may themselves be leaf or
    condensed), walked recursively down to messages.

    Correctness of this recursive walk depends entirely on compaction.py's
    own fix (2026-08-29): folded rows are marked `folded_into`, never
    physically deleted — a condensed row's children have to still exist for
    this to find them. Restricted to subagents only at the tool-schema
    level (llm.py), not enforced here — matches the paper's own reasoning:
    letting the main agent freely re-expand every summary back to full text
    would fight directly against the reason compaction exists in the first
    place.
    """
    parsed = _parse_id(summary_id, "lcm_expand")

    root = await conn.fetchrow(
        "SELECT summary_id, kind, covers FROM context_summaries WHERE summary_id = $1", parsed
    )
    if not root:
        raise LCMNotFoundError(f"lcm_expand: no summary found with id {summary_id!r}")

    message_ids = await _resolve_to_message_ids(conn, root)
    if not message_ids:
        return {"summary_id": str(parsed), "messages": []}

    rows = await conn.fetch(
        "SELECT m.message_id, m.role, m.content "
        "FROM messages m JOIN turns t ON m.parent_id = t.turn_id "
        "WHERE m.message_id = ANY($1::uuid[]) ORDER BY t.turn_seq, m.seq",
        message_ids,
    )
    return {
        "summary_id": str(parsed),
        "messages": [{"role": r["role"], "content": r["content"]} for r in rows],
    }


async def _resolve_to_message_ids(conn, summary_row, _seen: set | None = None) -> list:
    """Recursively resolves a summary row down to the raw message_ids it
    ultimately covers. `_seen` is a defensive cycle guard only — the DAG is
    acyclic by construction (a condensed row is always strictly newer than,
    and only ever points at, rows that already existed before it), so this
    should never actually trigger; kept because a recursive walk over
    caller-suppliable-adjacent data with no guard at all is a real footgun
    to leave for later, not because a cycle is expected."""
    _seen = _seen if _seen is not None else set()
    if summary_row["summary_id"] in _seen:
        return []
    _seen.add(summary_row["summary_id"])

    if summary_row["kind"] == "leaf":
        return list(summary_row["covers"])

    # kind == "condensed": covers holds child summary_ids, not message_ids.
    # These rows are still physically present (folded_into set, not
    # deleted) — see compaction.py's own fix — which is exactly what makes
    # this lookup succeed instead of silently finding nothing.
    if not summary_row["covers"]:
        return []
    child_rows = await conn.fetch(
        "SELECT summary_id, kind, covers FROM context_summaries WHERE summary_id = ANY($1::uuid[])",
        summary_row["covers"],
    )
    message_ids: list = []
    for child in child_rows:
        message_ids.extend(await _resolve_to_message_ids(conn, child, _seen))
    return message_ids


async def _active_covering_summary_ids(conn, session_key: str, message_ids: list) -> dict:
    """For each id in message_ids, resolves which CURRENTLY-ACTIVE
    (or, if that leaf has since been folded, its immediate parent) covers
    it — the NEAREST summary node whose direct span includes the message,
    not the topmost surviving ancestor.

    Real bug found and fixed 2026-08-29, after building this exact walk to
    go all the way to the topmost `folded_into IS NULL` ancestor: once a
    session has folded deep enough that a message's leaf has itself been
    folded into a condensed node, and THAT condensed node has itself been
    folded again (leaf -> S3 -> S7, say), walking to the top reports S7 for
    every message under it — collapsing "GQA came up in region S3" and "GQA
    came up in region S4" into "GQA came up somewhere under S7," which
    could mean anywhere in the whole session once the DAG is tall enough.
    That's the opposite of useful: it discards exactly the region-level
    specificity grep exists to surface, and gets worse the more folding has
    happened, i.e. exactly when it matters most. Fixed to stop one hop up
    from the message's own leaf — report the leaf's direct `folded_into`
    target (S3), never chase further even if S3 has itself since been
    folded into S7. Still fully actionable even though S3 itself may no
    longer be part of the live `assemble()`-shown frontier: lcm_describe/
    lcm_expand don't check folded_into at all, they just read `covers`,
    so a "stale" nearest node is still a real, directly usable id.

    A leaf that hasn't been folded at all yet reports itself (there's
    nothing to hop to). A message not covered by anything yet (still in
    the raw verbatim window / the not-yet-compacted tail) is absent from
    the returned dict.

    Two bulk queries, not one per message_id — this is called from grep()
    (up to GREP_DEFAULT_LIMIT+1 messages per call) and describe() (a single
    message), so avoiding real N+1 query fan-out matters more for the
    former than the latter, and doing it uniformly is simpler than two
    different implementations."""
    if not message_ids:
        return {}

    leaf_rows = await conn.fetch(
        "SELECT summary_id, covers, folded_into FROM context_summaries "
        "WHERE session_key = $1 AND kind = 'leaf'",
        session_key,
    )
    message_to_leaf: dict = {}
    leaf_folded_into: dict = {}
    for row in leaf_rows:
        leaf_folded_into[row["summary_id"]] = row["folded_into"]
        for mid in row["covers"]:
            message_to_leaf[mid] = row["summary_id"]

    result = {}
    for mid in message_ids:
        leaf_id = message_to_leaf.get(mid)
        if leaf_id is None:
            continue
        parent = leaf_folded_into.get(leaf_id)
        result[mid] = str(parent) if parent is not None else str(leaf_id)
    return result
