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
(docs/components/memory-slot.md) and `search_tools`/`call_tool`
(docs/components/tool-registry.md) alongside `shell_exec` — real,
model-offerable tools. tools.TOOL_REGISTRY's `search`/`slow_tool`/
`noop_tool` entries are still fixture-only stubs (docs/components/
activities-outbound-delivery.md's demo tools) and must never be offered to
a real model. Not read dynamically off TOOL_REGISTRY, which has no
LLM-schema metadata yet — a generic schema-registry abstraction for exactly
five tools would be premature; add future real tools here by hand alongside
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

from . import agent_brain, ids, lcm, model_registry, sentence_segmenter
from .types import Usage

logger = logging.getLogger(__name__)

# docs/components/model-registry.md, "Resolved: Selection Mechanism" — not a
# judgment-call nudge (contrast the reverted search_tools-before-shell_exec
# system-prompt rule): the model has no structural way to know this protocol
# exists at all without being told, unlike that case where the necessary
# information was already available another way.
_NEXT_STEP_HINT_TOOL_NAME = "declare_next_step_hint"

DEFAULT_SYSTEM_PROMPT = (
    "You are an autonomous coding assistant. You have access to a shell_exec "
    "tool to run shell commands in your working directory. Use it when needed "
    "to complete the user's request, then summarize the result in plain text. "
    f"Every response, also call {_NEXT_STEP_HINT_TOOL_NAME} alongside anything "
    "else you call, declaring what the next step needs."
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
    {
        "type": "function",
        "function": {
            "name": "search_tools",
            # Description mirrors mcp-hub's own real search_tools tool
            # description, plus a note about the shell-hub fan-out (this
            # project's own addition, not mcp-hub's).
            "description": (
                "Semantically search the tools available across all registered MCP "
                "backends, plus locally-available shell/CLI capabilities. Returns "
                "candidates with a server, tool name, description, and input schema. "
                "Use call_tool to invoke an mcp-hub result (server != \"shell\"), or "
                "shell_exec directly to invoke a shell result (server == \"shell\")."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "query": {"type": "string", "description": "Natural language description of what you need."},
                    "top_k": {"type": "number", "description": "Max results to return (default 5)."},
                },
                "required": ["query"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "call_tool",
            "description": "Invoke a tool discovered via search_tools, on the backend that owns it.",
            "parameters": {
                "type": "object",
                "properties": {
                    "server": {"type": "string", "description": "The server field from a search_tools result."},
                    "tool": {"type": "string", "description": "The tool field from a search_tools result."},
                    "arguments": {
                        "type": "object",
                        "description": "Arguments matching that result's input_schema.",
                    },
                },
                "required": ["server", "tool", "arguments"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": _NEXT_STEP_HINT_TOOL_NAME,
            # docs/components/model-registry.md, "Resolved: Selection
            # Mechanism" — included alongside whatever other tool_calls a
            # response already has (OpenAI's function-calling API supports
            # multiple tool_calls per response), so this rides on the same
            # API call rather than costing a separate round trip.
            "description": (
                "Always include this alongside your response, every step, declaring what kind "
                "of model the NEXT step needs. tier=fast for simple/mechanical next steps "
                "(e.g. running a command and reporting its output), tier=expert for next steps "
                "needing careful multi-step reasoning or judgment, tier=medium otherwise."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "modality": {"type": "string", "description": "Always \"language\" for now."},
                    "tier": {"type": "string", "enum": ["fast", "medium", "expert"]},
                },
                "required": ["modality", "tier"],
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
    # docs/components/model-registry.md — this step's self-declared hint for
    # the next step, defaulted to model_registry.default_hint() if the model
    # didn't include declare_next_step_hint in its response (real models
    # aren't guaranteed to comply with an instruction every single call —
    # degrade to the bootstrap default rather than erroring).
    next_hint_modality: str = "language"
    next_hint_tier: str = "medium"


async def build_conversation(conn, turn_id: str, system_prompt: str) -> tuple[list[dict], int]:
    """Session-wide context assembly (docs/components/context-slot.md) —
    delegates to lcm.assemble, which reads every top-level turn under this
    turn's session, not just this one (the fix that doc's "Resolved: Scope"
    section calls for — cross-turn session memory used to be a real gap,
    now closed). This function's own remaining job is narrower: detect
    session-start (still a turn_id-scoped question — "is this the first
    ModelCall of the first top-level turn" — genuinely different from lcm's
    assembly concern, so it stays here) and splice in the memory-slot
    retrieval block lcm.assemble has no reason to know about.

    Returns (conversation, context_tokens) — context_tokens is threaded
    back through ModelCallOutput to the workflow for the compression-gate
    check (turn.go can't accumulate this itself across separate
    turn-workflow executions — see lcm.assemble's own docstring).
    """
    turn_row = await conn.fetchrow("SELECT parent_type, turn_seq FROM turns WHERE turn_id = $1", turn_id)
    session_key = ids.session_key_of(turn_id)

    conversation, context_tokens = await lcm.assemble(conn, session_key, system_prompt)

    if turn_row is not None and turn_row["parent_type"] == "session" and turn_row["turn_seq"] == 1:
        session_messages = await conn.fetch(
            "SELECT role, content FROM messages WHERE parent_id = $1 ORDER BY seq", turn_id
        )
        # docs/components/memory-slot.md, "Resolved: Two Retrieval Triggers"
        # — session-start is unconditional, fires once, on this turn's first
        # ModelCall only (only InsertMessage's own start-of-turn row exists
        # yet, no assistant response). Deliberately a direct message-count
        # check, not inferred from len(conversation) — lcm.assemble's output
        # length isn't a reliable proxy (it could vary with future changes
        # to what gets prepended, e.g. summary rows).
        is_session_start = len(session_messages) == 1 and session_messages[0]["role"] == "user"
        if is_session_start:
            first_message = session_messages[0]
            memory_block = await _session_start_memory_block(turn_id, first_message["content"])
            if memory_block is not None:
                # Placed right after the system prompt — before the summary
                # DAG / verbatim window lcm.assemble already appended,
                # matching memory-slot.md's "Resolved: Staleness" placement
                # (retrieved background before live session content).
                conversation.insert(1, memory_block)
                context_tokens += lcm.estimate_tokens(memory_block["content"])

    return conversation, context_tokens


async def call_model(client: AsyncOpenAI, conversation: list[dict], model: str) -> RealModelResult:
    """model is resolved by the caller (model_call.py, via model_registry.py)
    from this step's hint — not read from PIONEER_MODEL directly here
    anymore, docs/components/model-registry.md's whole point being that the
    model isn't fixed for the process's lifetime."""
    if not model:
        raise RuntimeError(
            "No model resolved for this step - model_registry.py's LANGUAGE_<TIER>_MODEL/"
            "PIONEER_MODEL are both unset. Set at least PIONEER_MODEL."
        )
    max_tokens = int(os.environ.get("PIONEER_MAX_TOKENS", "4096"))

    response = await client.chat.completions.create(
        model=model,
        messages=conversation,
        tools=TOOLS_SCHEMA,
        max_tokens=max_tokens,
    )
    message = response.choices[0].message

    raw_tool_calls = []
    next_hint_modality, next_hint_tier = model_registry.default_hint()
    for tc in message.tool_calls or []:
        if tc.function.name == _NEXT_STEP_HINT_TOOL_NAME:
            # Not a real, dispatchable tool call — pulled out here so
            # tool_call.py/turn.go never see it as one. Malformed hint
            # arguments degrade to the bootstrap default rather than
            # failing the whole response.
            try:
                hint_args = json.loads(tc.function.arguments)
                next_hint_modality = hint_args.get("modality", next_hint_modality)
                next_hint_tier = hint_args.get("tier", next_hint_tier)
            except (json.JSONDecodeError, AttributeError):
                logger.warning("call_model: malformed %s arguments, using default hint", _NEXT_STEP_HINT_TOOL_NAME)
            continue
        raw_tool_calls.append(
            {"name": tc.function.name, "arguments": json.loads(tc.function.arguments), "is_subagent": False}
        )

    usage = Usage(
        input_tokens=response.usage.prompt_tokens if response.usage else 0,
        output_tokens=response.usage.completion_tokens if response.usage else 0,
    )
    # messages.content is NOT NULL - the API can return content=None when the
    # response is tool-calls-only.
    return RealModelResult(
        content=message.content or "",
        raw_tool_calls=raw_tool_calls,
        usage=usage,
        next_hint_modality=next_hint_modality,
        next_hint_tier=next_hint_tier,
    )


async def call_model_streaming(client: AsyncOpenAI, conversation: list[dict], model: str, on_chunk) -> RealModelResult:
    """Streaming counterpart to call_model — docs/components/gateway.md's
    "Resolved: ModelCall Streaming — Shared Infra, Text-First Rollout".
    Used only for a turn's first ModelCall call (model_call.py gates this
    on context_seq == 0, the "single-shot turns only" scope decided
    directly): whether a turn turns out to need tool calls is only known
    once this finishes, but chunks have to be delivered live, during the
    call, for streaming to have any latency benefit at all — the rare case
    where a turn streams content and then calls a tool anyway is accepted
    as a known, bounded edge case (two messages instead of one), not
    something this function tries to detect or prevent.

    Returns the exact same RealModelResult shape as call_model, so every
    caller past this function (model_call.py's own downstream processing)
    is unaffected by which path produced it.

    on_chunk is an async callable, invoked once per completed sentence
    boundary (sentence_segmenter.find_boundary) with the CUMULATIVE content
    delivered so far, not just the new increment — Discord's edit API
    replaces a message's whole content, so the delivery side needs the
    running total, not a diff. Called once more at the very end with
    whatever trailing text never hit a sentence boundary (e.g. a response
    that doesn't end in .!?), so the final delivered text always exactly
    matches the complete response, never cut short by segmentation.

    2026-08-26: also used for Discord voice (model_call.py's own platform
    gate), which needs each chunk's own NEW sentence(s) for TTS, not the
    running total — deliberately left as this function's caller's problem
    (turn_deliveries.content stays cumulative for every platform, and
    deliver_voice_chunk.go computes the delta itself from consecutive rows)
    rather than changing this function's contract, since Discord text is
    still the only real consumer of the cumulative shape and there's no
    reason to make it aware voice exists at all.
    """
    if not model:
        raise RuntimeError(
            "No model resolved for this step - model_registry.py's LANGUAGE_<TIER>_MODEL/"
            "PIONEER_MODEL are both unset. Set at least PIONEER_MODEL."
        )
    max_tokens = int(os.environ.get("PIONEER_MAX_TOKENS", "4096"))

    stream = await client.chat.completions.create(
        model=model,
        messages=conversation,
        tools=TOOLS_SCHEMA,
        max_tokens=max_tokens,
        stream=True,
        stream_options={"include_usage": True},
    )

    content_buffer = ""
    unflushed = ""
    # Tool calls accumulate by index — OpenAI's streaming shape sends each
    # tool call's name/arguments as fragments across multiple chunks,
    # matched by the delta's own index, never assumed to arrive whole.
    tool_call_frags: dict[int, dict] = {}
    usage = Usage()

    async for chunk in stream:
        if chunk.usage is not None:
            usage = Usage(
                input_tokens=chunk.usage.prompt_tokens or 0,
                output_tokens=chunk.usage.completion_tokens or 0,
            )
        if not chunk.choices:
            continue
        delta = chunk.choices[0].delta
        if delta.content:
            content_buffer += delta.content
            unflushed += delta.content
            while True:
                boundary = sentence_segmenter.find_boundary(unflushed)
                if boundary is None:
                    break
                unflushed = unflushed[boundary:]
                await on_chunk(content_buffer[: len(content_buffer) - len(unflushed)])
        for tc in delta.tool_calls or []:
            frag = tool_call_frags.setdefault(tc.index, {"name": "", "arguments": ""})
            if tc.function and tc.function.name:
                frag["name"] += tc.function.name
            if tc.function and tc.function.arguments:
                frag["arguments"] += tc.function.arguments

    if unflushed:
        # Final forced flush — see docstring: the last delivered chunk must
        # always equal the complete response, regardless of whether it
        # ends on a real sentence boundary.
        await on_chunk(content_buffer)

    raw_tool_calls = []
    next_hint_modality, next_hint_tier = model_registry.default_hint()
    for idx in sorted(tool_call_frags):
        frag = tool_call_frags[idx]
        if frag["name"] == _NEXT_STEP_HINT_TOOL_NAME:
            try:
                hint_args = json.loads(frag["arguments"])
                next_hint_modality = hint_args.get("modality", next_hint_modality)
                next_hint_tier = hint_args.get("tier", next_hint_tier)
            except (json.JSONDecodeError, AttributeError):
                logger.warning(
                    "call_model_streaming: malformed %s arguments, using default hint", _NEXT_STEP_HINT_TOOL_NAME
                )
            continue
        try:
            arguments = json.loads(frag["arguments"]) if frag["arguments"] else {}
        except json.JSONDecodeError:
            logger.warning(
                "call_model_streaming: malformed tool call arguments for %s, treating as empty", frag["name"]
            )
            arguments = {}
        raw_tool_calls.append({"name": frag["name"], "arguments": arguments, "is_subagent": False})

    return RealModelResult(
        content=content_buffer,
        raw_tool_calls=raw_tool_calls,
        usage=usage,
        next_hint_modality=next_hint_modality,
        next_hint_tier=next_hint_tier,
    )
