"""LCM (Lossless Context Management) — docs/components/context-slot.md,
"Resolved: LCM as the Concrete Mechanism" (Ehrlich & Blackman 2026). Owns
session-scoped context assembly and compression; nothing else touches
context_summaries or does session-wide message assembly directly —
llm.py's build_conversation and compress_context.py both call into this
module rather than duplicating logic (clean separation, one place to read
to understand the whole mechanism).

Session-scoped, not turn-scoped (docs/components/context-slot.md,
"Resolved: Scope Is the Whole Session, Not One Turn") — every function here
takes session_key, reading/writing across every top-level turn under that
session, not one turn_id.

Token counting throughout is a plain character-based heuristic
(len(text)//4), not a real tokenizer — the actual model varies per tenant
(Pioneer, Crusoe/DeepSeek, ...) and there's no single correct tokenizer to
target without knowing which; a threshold check needs a reasonable proxy,
not an exact count, matching this project's existing tolerance for
approximate constants in this exact area (turn.go's own budgetTokens is a
"high placeholder ceiling", not derived from anything precise either).
"""

from __future__ import annotations

import json
import logging

logger = logging.getLogger(__name__)

# All placeholder-simple, matching this project's standing practice of
# deferring precise numeric tuning until real usage data exists (same shape
# as turn.go's own budgetTokens/softCompressionThreshold/hardCompressionThreshold).
VERBATIM_WINDOW_MESSAGES = 20  # last K reasoning steps stay verbatim, uncompressed
LEAF_FOLD_THRESHOLD = 5  # fold this many uncombined leaf summaries into one condensed summary


def estimate_tokens(text: str) -> int:
    return max(1, len(text or "") // 4)


async def assemble(conn, session_key: str, system_prompt: str) -> tuple[list[dict], int]:
    """Session-wide context assembly (docs/components/context-slot.md,
    "Resolved: Duties and Strategies" #1 — hybrid sliding window + summary
    DAG). Returns (conversation, context_tokens): conversation is
    OpenAI-shaped, ready for llm.call_model; context_tokens is this
    assembled context's estimated size, returned by ModelCall to the
    workflow for the compression-gate check — turn.go can't accumulate this
    itself across separate turn-workflow executions the way it does
    per-turn budget spend, since each top-level turn is a fresh child
    workflow with no memory of the last (docs/components/context-slot.md's
    "Two concrete consequences").

    Two layers, oldest first: the summary DAG (condensed and leaf summaries
    — nothing here decides what's covered vs not, compact() below owns
    that), then the verbatim window (last VERBATIM_WINDOW_MESSAGES messages
    across the WHOLE session, not just this turn), reconstructed the same
    way the old turn-scoped build_conversation did — an assistant message's
    tool_calls rebuilt from tool_calls rows, tool results interleaved.
    """
    conversation: list[dict] = [{"role": "system", "content": system_prompt}]
    context_tokens = estimate_tokens(system_prompt)

    summary_rows = await conn.fetch(
        "SELECT kind, content, token_count FROM context_summaries WHERE session_key = $1 ORDER BY created_at",
        session_key,
    )
    if summary_rows:
        rendered = "\n".join(f"- [{row['kind']}] {row['content']}" for row in summary_rows)
        conversation.append(
            {
                "role": "system",
                "content": "Summary of earlier parts of this session (oldest first, possibly compressed):\n"
                + rendered,
            }
        )
        context_tokens += sum(row["token_count"] for row in summary_rows)

    messages = await _session_messages(conn, session_key)
    window = messages[-VERBATIM_WINDOW_MESSAGES:]

    for msg in window:
        if msg["role"] != "assistant":
            conversation.append({"role": msg["role"], "content": msg["content"]})
            context_tokens += estimate_tokens(msg["content"])
            continue

        tool_call_rows = await conn.fetch(
            "SELECT tool_call_id, tool_name, arguments, status, result "
            "FROM tool_calls WHERE message_id = $1 ORDER BY started_at",
            msg["message_id"],
        )
        if not tool_call_rows:
            conversation.append({"role": "assistant", "content": msg["content"]})
            context_tokens += estimate_tokens(msg["content"])
            continue

        conversation.append(
            {
                "role": "assistant",
                "content": msg["content"] or None,
                "tool_calls": [
                    {
                        "id": row["tool_call_id"],
                        "type": "function",
                        "function": {"name": row["tool_name"], "arguments": row["arguments"]},
                    }
                    for row in tool_call_rows
                ],
            }
        )
        context_tokens += estimate_tokens(msg["content"])
        for row in tool_call_rows:
            if row["status"] == "ok":
                result_content = row["result"] or "{}"
            elif row["status"] == "cancelled":
                result_content = json.dumps({"error": "cancelled: interrupted by a new message"})
            else:
                result_content = row["result"] or json.dumps(
                    {"error": f"tool call did not complete (status={row['status']})"}
                )
            conversation.append({"role": "tool", "tool_call_id": row["tool_call_id"], "content": result_content})
            context_tokens += estimate_tokens(result_content)

    return conversation, context_tokens


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


async def compact(conn, session_key: str, openai_client, model: str) -> None:
    """Three-Level Escalation (docs/components/context-slot.md, "Resolved:
    Duties and Strategies" #3) — real body for CompressContext, replacing
    the no-op stub. Summarizes the oldest not-yet-covered span of messages
    (everything outside the verbatim window assemble() shows) into a new
    leaf context_summaries row, then folds accumulated leaves into a
    condensed summary once LEAF_FOLD_THRESHOLD of them exist.

    Level 1: LLM summarize, preserve detail. Level 2 (only if Level 1's
    output isn't actually shorter than its input — "compaction failure",
    the real, previously-unnamed failure mode this design names): LLM
    summarize, aggressive bullet points. Level 3 (only if Level 2 also
    fails to shrink): deterministic truncate, no LLM — always shrinks,
    guaranteeing convergence regardless of what either LLM call produces.
    """
    messages = await _session_messages(conn, session_key)
    covered_rows = await conn.fetch(
        "SELECT unnest(covers) AS message_id FROM context_summaries WHERE session_key = $1 AND kind = 'leaf'",
        session_key,
    )
    covered_ids = {row["message_id"] for row in covered_rows}
    uncovered = [m for m in messages if m["message_id"] not in covered_ids]

    span = uncovered[:-VERBATIM_WINDOW_MESSAGES] if len(uncovered) > VERBATIM_WINDOW_MESSAGES else []
    if not span:
        logger.info("lcm.compact[%s]: nothing new to compact", session_key)
        return

    transcript = "\n".join(f"{m['role']}: {m['content']}" for m in span if m["content"])
    content = await _escalating_summarize(openai_client, model, transcript)

    await conn.execute(
        "INSERT INTO context_summaries (session_key, kind, covers, content, token_count) "
        "VALUES ($1, 'leaf', $2, $3, $4)",
        session_key,
        [m["message_id"] for m in span],
        content,
        estimate_tokens(content),
    )
    logger.info("lcm.compact[%s]: wrote leaf summary covering %d messages", session_key, len(span))

    await _fold_leaves_if_due(conn, session_key, openai_client, model)


async def _session_messages(conn, session_key: str) -> list:
    """Session-wide, not turn-scoped — every top-level turn's messages,
    ordered by turn_seq then seq (docs/components/context-slot.md,
    "Resolved: Scope" — the fix that section calls for directly)."""
    return await conn.fetch(
        "SELECT m.message_id, m.role, m.content, m.seq "
        "FROM messages m JOIN turns t ON m.parent_id = t.turn_id "
        "WHERE t.parent_id = $1 AND t.parent_type = 'session' "
        "ORDER BY t.turn_seq, m.seq",
        session_key,
    )


async def _escalating_summarize(openai_client, model: str, transcript: str) -> str:
    input_tokens = estimate_tokens(transcript)

    content = await _summarize(openai_client, model, transcript, aggressive=False)
    if estimate_tokens(content) < input_tokens:
        return content

    content = await _summarize(openai_client, model, transcript, aggressive=True)
    if estimate_tokens(content) < input_tokens:
        return content

    # Level 3: deterministic, always shrinks — guarantees convergence
    # regardless of what either LLM call produced.
    return transcript[: max(1, len(transcript) // 4)] + " ...[truncated]"


async def _summarize(openai_client, model: str, transcript: str, aggressive: bool) -> str:
    instruction = (
        "Summarize this conversation span as terse bullet points, maximally "
        "compressed, losing detail if needed to be short."
        if aggressive
        else "Summarize this conversation span, preserving important details."
    )
    response = await openai_client.chat.completions.create(
        model=model,
        messages=[
            {"role": "system", "content": instruction},
            {"role": "user", "content": transcript},
        ],
    )
    return response.choices[0].message.content or ""


async def _fold_leaves_if_due(conn, session_key: str, openai_client, model: str) -> None:
    """Recursively folds the summary DAG once any level crosses
    LEAF_FOLD_THRESHOLD — leaves first, then repeatedly condensed-of-condensed
    while condensed count is still at/above threshold (a leaf-fold can itself
    push condensed count over threshold, and folding condensed rows produces
    one new condensed row, which could — over a long enough session — push
    it over threshold again).

    Found necessary via a real conversation about long-running-session cost,
    not assumed: the previous version only ever folded 'leaf' rows — condensed
    rows were never re-folded, so `context_summaries` for a session that
    never resets would accumulate an ever-growing flat list of condensed
    rows, and assemble() sums every one of their token_count unconditionally
    into every call's context. That's the opposite of what "hierarchical"
    (LCM §2.1's summary-of-summaries DAG) was supposed to buy — DAG height
    should grow logarithmically with total summary count, not stay flat
    while the row count grows linearly forever. No schema change needed:
    `context_summaries.covers` was already documented as "condensed: child
    summary_ids", i.e. a condensed row folding other condensed rows was
    always the intended shape, just never implemented."""
    await _fold_kind_if_due(conn, session_key, openai_client, model, "leaf")
    while await _fold_kind_if_due(conn, session_key, openai_client, model, "condensed"):
        pass


async def _fold_kind_if_due(conn, session_key: str, openai_client, model: str, kind: str) -> bool:
    """Folds every `kind` row into one new 'condensed' row, if at least
    LEAF_FOLD_THRESHOLD of them exist. Returns whether a fold happened, so
    the recursive condensed-of-condensed loop above knows whether to keep
    going."""
    rows = await conn.fetch(
        "SELECT summary_id, content FROM context_summaries "
        "WHERE session_key = $1 AND kind = $2 ORDER BY created_at",
        session_key,
        kind,
    )
    if len(rows) < LEAF_FOLD_THRESHOLD:
        return False

    combined = "\n".join(row["content"] for row in rows)
    condensed_content = await _escalating_summarize(openai_client, model, combined)

    async with conn.transaction():
        await conn.execute(
            "INSERT INTO context_summaries (session_key, kind, covers, content, token_count) "
            "VALUES ($1, 'condensed', $2, $3, $4)",
            session_key,
            [row["summary_id"] for row in rows],
            condensed_content,
            estimate_tokens(condensed_content),
        )
        await conn.execute(
            "DELETE FROM context_summaries WHERE summary_id = ANY($1::uuid[])",
            [row["summary_id"] for row in rows],
        )
    logger.info("lcm._fold_kind_if_due[%s]: folded %d %r summaries into one condensed summary", session_key, len(rows), kind)
    return True
