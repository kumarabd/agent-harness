"""Session-wide context assembly — docs/components/context-slot.md,
"Resolved: Duties and Strategies" #1 (hybrid sliding window + summary DAG).

Session-scoped, not turn-scoped (docs/components/context-slot.md, "Resolved:
Scope Is the Whole Session, Not One Turn") — assemble() takes session_key,
reading across every top-level turn under that session, not one turn_id.
"""

from __future__ import annotations

import json

from .constants import VERBATIM_WINDOW_MESSAGES, estimate_tokens


async def assemble(conn, session_key: str, system_prompt: str) -> tuple[list[dict], int]:
    """Returns (conversation, context_tokens): conversation is OpenAI-shaped,
    ready for llm.call_model; context_tokens is this assembled context's
    estimated size, returned by ModelCall to the workflow for the
    compression-gate check — turn.go can't accumulate this itself across
    separate turn-workflow executions the way it does per-turn budget spend,
    since each top-level turn is a fresh child workflow with no memory of
    the last (docs/components/context-slot.md's "Two concrete consequences").

    Two layers, oldest first: the summary DAG (condensed and leaf summaries
    — nothing here decides what's covered vs not, compaction.compact() owns
    that), then the verbatim window (last VERBATIM_WINDOW_MESSAGES messages
    across the WHOLE session, not just this turn), reconstructed the same
    way the old turn-scoped build_conversation did — an assistant message's
    tool_calls rebuilt from tool_calls rows, tool results interleaved.

    `AND folded_into IS NULL` (2026-08-29, alongside lcm.retrieval's own
    lossless-expand fix) — once a leaf/condensed row has been folded into a
    newer condensed row, it's marked folded_into rather than deleted (see
    compaction.py's own comment for why), so this query has to exclude
    superseded rows explicitly or every call would start double-counting
    (both a folded leaf AND its condensed replacement) into every
    assembled context — a real regression the pre-fix DELETE-based design
    never had to guard against, since a deleted row simply couldn't reappear
    here on its own.
    """
    conversation: list[dict] = [{"role": "system", "content": system_prompt}]
    context_tokens = estimate_tokens(system_prompt)

    summary_rows = await conn.fetch(
        "SELECT kind, content, token_count FROM context_summaries "
        "WHERE session_key = $1 AND folded_into IS NULL ORDER BY created_at",
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

    messages = await session_messages(conn, session_key)
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


async def session_messages(conn, session_key: str) -> list:
    """Session-wide, not turn-scoped — every top-level turn's messages,
    ordered by turn_seq then seq (docs/components/context-slot.md,
    "Resolved: Scope" — the fix that section calls for directly). Shared by
    assemble() (above), compaction.compact() (finds the oldest uncovered
    span), and retrieval.py (grep searches this same real message set)."""
    return await conn.fetch(
        "SELECT m.message_id, m.role, m.content, m.seq "
        "FROM messages m JOIN turns t ON m.parent_id = t.turn_id "
        "WHERE t.parent_id = $1 AND t.parent_type = 'session' "
        "ORDER BY t.turn_seq, m.seq",
        session_key,
    )
