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

TOOLS_SCHEMA is hardcoded to exactly `shell_exec` — the only real,
model-offerable tool right now. tools.TOOL_REGISTRY's `search`/`slow_tool`/
`noop_tool` entries are fixture-only stubs (docs/components/
activities-outbound-delivery.md's demo tools) and must never be offered to
a real model. Not read dynamically off TOOL_REGISTRY, which has no
LLM-schema metadata yet — a generic schema-registry abstraction for exactly
one tool would be premature; add future real tools here by hand alongside
their TOOL_REGISTRY entry in tools.py.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field

from openai import AsyncOpenAI

from .types import Usage

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
]


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
