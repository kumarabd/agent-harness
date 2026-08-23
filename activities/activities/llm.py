"""Real LLM integration for ModelCall (activities/activities/model_call.py) —
provider is Pioneer, an OpenAI-API-compatible endpoint (the openai SDK's
base_url override, no separate client library needed — see worker.py's
AsyncOpenAI construction). No provider is specified anywhere in docs/; this
was a free choice.

Only invoked when no test fixture exists for a given (turn_id, context_seq) —
model_call.py's fixture-first branch is unchanged; this module only fills in
what used to be a `raise RuntimeError("no real model provider configured")`.

`messages` only stores plain role/content rows — there's no OpenAI-shaped
representation of an assistant's prior tool_calls or their results anywhere
in the schema. build_conversation reconstructs a valid OpenAI conversation
from the existing tables (messages + tool_calls) with no new columns: an
assistant message that minted tool calls gets its `tool_calls` array
rebuilt from the tool_calls rows keyed by message_id, each immediately
followed by a `role: "tool"` message carrying that call's result, matched by
tool_call_id (tool_calls.tool_call_id is reused verbatim as OpenAI's
tool_call_id — any string works, no second ID scheme needed).

TOOLS_SCHEMA now also includes `memory_search`/`memory_expand`
(docs/components/memory-slot.md) alongside `shell_exec` — real,
model-offerable tools. tools.TOOL_REGISTRY's `search`/`slow_tool`/
`noop_tool` entries are still fixture-only stubs (docs/components/
activities-outbound-delivery.md's demo tools) and must never be offered to
a real model. Not read dynamically off TOOL_REGISTRY, which has no
LLM-schema metadata yet — a generic schema-registry abstraction for exactly
three tools would be premature; add future real tools here by hand alongside
their TOOL_REGISTRY entry in tools.py.

**Session-start memory retrieval** (docs/components/memory-slot.md,
"Resolved: Two Retrieval Triggers" + "Resolved: Failure Handling"):
build_conversation calls agent-brain's memory_search once, unconditionally,
the first time a session's first turn builds its conversation (turn_seq==1,
parent_type=='session', and no assistant message yet for this turn — i.e.
the very first ModelCall of a brand-new session, inferred from message
count rather than threading context_seq through, since InsertMessage's
start-of-turn write is the only row present at that point). Bounded retry
(3 attempts, matching ToolCall's MaximumAttempts), then degrades to no
retrieved background rather than failing the turn — this is a plain
in-process retry, not a separate Temporal activity, since it's a step
*inside* the already-activity-tracked ModelCall. Results are rendered into
one labeled system-role block placed *before* the live conversation
(docs/components/memory-slot.md's "Resolved: Staleness Is Handled by
Placement") — not memory-slot.md's originally-designed typed
{content, harness_type} normalization: that round-trips a
`attributes.harness_type` tag stamped at write time, but agent-brain's real,
current memory_write tool schema (internal/mcp/tools_events.go, verified
directly) has no generic `attributes` input field to stamp it through in
the first place — a design/implementation gap discovered while building
this, not fixed here (agent-brain's own repo, out of scope for this
project). Skipped entirely rather than half-implemented against a shape the
real tool doesn't support; the raw fused results are rendered as-is
instead.
"""

from __future__ import annotations

import json
import logging
import os
from dataclasses import dataclass, field

from openai import AsyncOpenAI

from . import agent_brain
from .types import Usage

logger = logging.getLogger(__name__)

DEFAULT_SYSTEM_PROMPT = (
    "You are an autonomous coding assistant. You have access to a shell_exec "
    "tool to run shell commands in your working directory. Use it when needed "
    "to complete the user's request, then summarize the result in plain text."
)

TOOLS_SCHEMA = [
    {
        "type": "function",
        "function": {
            "name": "shell_exec",
            "description": "Execute a shell command in the session's working directory.",
            "parameters": {
                "type": "object",
                "properties": {
                    "command": {"type": "string", "description": "The shell command to run."},
                },
                "required": ["command"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "memory_search",
            # Description mirrors agent-brain's own tool description
            # (internal/mcp/tools.go) verbatim-ish — the model is calling
            # agent-brain directly, not a paraphrased wrapper.
            "description": (
                "Search across the full semantic layer at once: episodic memory units, "
                "promoted generalized facts and relationships, raw asserted facts and "
                "relationships, rules/constraints, and concept definitions. Results are "
                "fused into one ranked list. Use memory_expand on a result's id to recover "
                "the raw episodes behind it."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "query": {"type": "string", "description": "Natural language recall query."},
                    "limit": {"type": "number", "description": "Max results to return (default 10)."},
                },
                "required": ["query"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "memory_expand",
            "description": (
                "Recover raw, verbatim episodes backing a memory_search result. Always "
                "full-depth — the raw events and facts, in chronological order. Use when the "
                "content already attached to a memory_search result isn't specific enough."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "node_id": {"type": "string", "description": "id returned by memory_search."},
                    "node_type": {
                        "type": "string",
                        "description": '"emu" (default) or "semantic" — which kind node_id is.',
                    },
                    "limit": {"type": "number", "description": "Max episodes to return (default 20, max 50)."},
                },
                "required": ["node_id"],
            },
        },
    },
]

_MEMORY_SEARCH_RETRY_ATTEMPTS = 3  # matches ToolCall's MaximumAttempts (docs/components/temporal-workflow.md)


def _render_memory_results(results: list[dict]) -> str:
    """Best-effort text rendering of memory_search's fused results — see
    module docstring on why this isn't the typed {content, harness_type}
    normalization memory-slot.md originally specified. Each source shape
    genuinely differs (internal/recall/fusion.go's FusedResult), so this
    picks the most relevant text field per source rather than assuming one
    uniform "content" field exists."""
    lines = []
    for r in results:
        source = r.get("source", "?")
        if r.get("statement"):
            text = r["statement"]
        elif r.get("term") and r.get("definition"):
            text = f"{r['term']}: {r['definition']}"
        elif r.get("emu", {}).get("semantic_fact"):
            sf = r["emu"]["semantic_fact"]
            text = f"predicate={sf.get('predicate')} object={sf.get('object_value')}"
        else:
            text = f"(id={r.get('id')} — use memory_expand for full content)"
        lines.append(f"- [{source}] {text}")
    return "\n".join(lines)


async def _session_start_memory_block(turn_id: str, query: str) -> dict | None:
    """Returns a system-role message with retrieved background, or None if
    agent-brain isn't configured for this deployment or every retry attempt
    failed (degrade gracefully, per module docstring's Failure Handling
    note — never raises)."""
    result = None
    for attempt in range(1, _MEMORY_SEARCH_RETRY_ATTEMPTS + 1):
        try:
            result = await agent_brain.call_tool("memory_search", {"query": query, "limit": 10})
            break
        except agent_brain.AgentBrainNotConfiguredError:
            return None
        except Exception:  # noqa: BLE001 - real network/protocol failure, bounded retry then degrade
            logger.warning("session-start memory_search failed (attempt %d/%d) for turn %s",
                            attempt, _MEMORY_SEARCH_RETRY_ATTEMPTS, turn_id, exc_info=True)
            if attempt == _MEMORY_SEARCH_RETRY_ATTEMPTS:
                return None

    results = result.get("results", [])
    if not results:
        return None
    rendered = _render_memory_results(results)
    return {
        "role": "system",
        "content": (
            "The following is background from prior sessions, possibly stale — weigh it "
            "against what this conversation has already established:\n" + rendered
        ),
    }


@dataclass
class RealModelResult:
    content: str
    raw_tool_calls: list[dict] = field(default_factory=list)
    usage: Usage = field(default_factory=Usage)


async def build_conversation(conn, turn_id: str, system_prompt: str) -> list[dict]:
    """Reconstructs this turn's conversation so far, OpenAI-shaped. Scoped to
    this turn only (messages.parent_id = turn_id), matching exactly how the
    fixture path already scopes context (_test_scripted_responses WHERE
    turn_id = ...) — cross-turn session memory is a separate, larger gap
    (future-work.md's session-consolidation item), not this pass.
    """
    conversation: list[dict] = [{"role": "system", "content": system_prompt}]

    messages = await conn.fetch(
        "SELECT message_id, role, content FROM messages WHERE parent_id = $1 ORDER BY seq",
        turn_id,
    )

    # docs/components/memory-slot.md, "Resolved: Two Retrieval Triggers" —
    # session-start is unconditional, fires once, on this turn's first
    # ModelCall only (message count == 1: just InsertMessage's own
    # start-of-turn row, no assistant response yet — see module docstring).
    turn_row = await conn.fetchrow("SELECT parent_type, turn_seq FROM turns WHERE turn_id = $1", turn_id)
    is_session_start = (
        turn_row is not None
        and turn_row["parent_type"] == "session"
        and turn_row["turn_seq"] == 1
        and len(messages) == 1
        and messages[0]["role"] == "user"
    )
    if is_session_start:
        memory_block = await _session_start_memory_block(turn_id, messages[0]["content"])
        if memory_block is not None:
            conversation.append(memory_block)

    for msg in messages:
        if msg["role"] != "assistant":
            conversation.append({"role": msg["role"], "content": msg["content"]})
            continue

        tool_call_rows = await conn.fetch(
            "SELECT tool_call_id, tool_name, arguments, status, result "
            "FROM tool_calls WHERE message_id = $1 ORDER BY started_at",
            msg["message_id"],
        )
        if not tool_call_rows:
            conversation.append({"role": "assistant", "content": msg["content"]})
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
        for row in tool_call_rows:
            if row["status"] == "ok":
                result_content = row["result"] or "{}"
            elif row["status"] == "cancelled":
                result_content = json.dumps({"error": "cancelled: interrupted by a new message"})
            else:
                # "error" (real failure) or the unexpected "pending" case (the
                # workflow always waits for a step's tool calls to fully
                # resolve before looping back to ModelCall, so this shouldn't
                # occur in practice - treated as a genuine error observation
                # rather than built out as a real, expected state).
                result_content = row["result"] or json.dumps({"error": f"tool call did not complete (status={row['status']})"})
            conversation.append(
                {"role": "tool", "tool_call_id": row["tool_call_id"], "content": result_content}
            )

    return conversation


async def call_model(client: AsyncOpenAI, conversation: list[dict]) -> RealModelResult:
    model = os.environ.get("PIONEER_MODEL")
    if not model:
        raise RuntimeError(
            "PIONEER_MODEL is not set - Pioneer's model catalog isn't known ahead of time, "
            "so there's no safe default to guess. Set it to a real model name Pioneer serves."
        )
    max_tokens = int(os.environ.get("PIONEER_MAX_TOKENS", "4096"))

    response = await client.chat.completions.create(
        model=model,
        messages=conversation,
        tools=TOOLS_SCHEMA,
        max_tokens=max_tokens,
    )
    message = response.choices[0].message

    raw_tool_calls = [
        {"name": tc.function.name, "arguments": json.loads(tc.function.arguments), "is_subagent": False}
        for tc in (message.tool_calls or [])
    ]
    usage = Usage(
        input_tokens=response.usage.prompt_tokens if response.usage else 0,
        output_tokens=response.usage.completion_tokens if response.usage else 0,
    )
    # messages.content is NOT NULL - the API can return content=None when the
    # response is tool-calls-only.
    return RealModelResult(content=message.content or "", raw_tool_calls=raw_tool_calls, usage=usage)
