"""Real LLM integration for ModelCall (activities/activities/model_call.py).

This module owns the provider-NEUTRAL side of the LLM call: the tools
schema every provider advertises, the default system prompt, the
`build_conversation` context-assembly helper (session-start memory
retrieval + LCM assembly), and the RealModelResult return shape. The
actual per-provider request/response translation lives in
activities/activities/providers/ (Provider ABC — OpenAI-compatible
covers real OpenAI, DeepSeek, Qwen/DashScope, Groq, OpenRouter, Crusoe,
etc.; Anthropic is its own class). See docs/components/model-registry.md.

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

import logging
from dataclasses import dataclass, field

from . import agent_brain, ids, lcm, model_registry
from .types import Usage

logger = logging.getLogger(__name__)

# docs/components/model-registry.md, "Resolved: Selection Mechanism" — not a
# judgment-call nudge (contrast the reverted search_tools-before-shell_exec
# system-prompt rule): the model has no structural way to know this protocol
# exists at all without being told, unlike that case where the necessary
# information was already available another way.
_NEXT_STEP_HINT_TOOL_NAME = "declare_next_step_hint"

# docs/components/temporal-workflow.md's recursion-termination guard (LCM/
# Volt's Task tool, Ehrlich & Blackman 2026) — real, model-facing subagent
# spawning, added 2026-08-29. Previously is_subagent was hardcoded False on
# every real parsed tool call in both providers (openai_provider.py/
# anthropic_provider.py) — a real model had no way to spawn a subagent at
# all; only test fixtures (workflows/scenarios/subagent-spawn.json) ever
# set is_subagent=true. Named to match that fixture's own existing
# convention (tool name "spawn_subagent", arguments.prompt), not the
# paper's literal "Task" — consistent with this codebase's own vocabulary
# (is_subagent, subagent_turn_id, merge_subagent_output already use
# "subagent" throughout).
_SPAWN_SUBAGENT_TOOL_NAME = "spawn_subagent"

# Rewritten 2026-08-29 — the original ("autonomous coding assistant... use
# shell_exec") was a scaffolding-era placeholder from before search_tools/
# call_tool/memory_search/memory_expand existed at all, and it actively
# misdescribed what this deployment actually is: a general-purpose personal
# assistant with a discoverable-tool surface (real, per-tenant third-party
# APIs via mcp-hub — maps, notes, health, finance, code hosting, and
# whatever else a given tenant has registered — not just a shell), not a
# coding-only tool. gateway/discord-voice.md's own Notes Log already
# flagged this exact framing as wrong when building the voice-specific
# prompt override — "that default's 'coding assistant' framing doesn't fit
# a spoken conversation regardless of formatting" — but only worked around
# it for voice at the time (a genuinely separate prompt, not a patch on
# this one), rather than fixing the shared default itself. This closes that
# the rest of the way, for every platform without its own override (Web,
# Discord text).
# Deliberately does NOT hardcode any tenant-specific backend name (no
# "maps-engine"/"notion"/"github" mentioned) — this constant is shared
# across every tenant regardless of which backends they've registered;
# search_tools' own real-time discovery is what surfaces the actual
# available set, per tool-registry.md's already-resolved design.
# (2026-08-29, same day: an earlier version of this rewrite also added
# search_skills/get_skill tools + a matching prompt sentence — components/
# skills.md's mcp-hub-document-store design. Reverted the same day, at the
# user's direction: skills are being reconsidered as a memory-layer concept
# (components/memory-slot.md) rather than a separate mcp-hub-backed
# document store — see skills.md's own "Superseded" section. This constant
# no longer mentions either tool.)
# The final sentence is unchanged in substance from the original —
# declare_next_step_hint being called every response is a real,
# functionally-required mechanism (model_registry's escalate-on-retry /
# tier hinting depends on it), and platform_prompts.go's own
# voiceSystemPromptText copies this exact sentence verbatim, so it has to
# keep matching byte-for-byte.
DEFAULT_SYSTEM_PROMPT = (
    "You are a helpful, general-purpose personal assistant with real tools — not limited to "
    "coding tasks. You have direct shell access (shell_exec) for local/system tasks, and "
    "search_tools/call_tool to discover and invoke whatever broader real capabilities this "
    "deployment has registered — third-party APIs and services beyond the shell, specific to "
    "this environment. Use memory_search (and memory_expand for full detail) to recall relevant "
    "context from past conversations when it's genuinely useful, not on every turn. After using "
    "a tool, summarize the result in plain text for the user rather than leaving it as raw "
    "output. "
    f"Every response, also call {_NEXT_STEP_HINT_TOOL_NAME} alongside anything "
    "else you call, declaring what the next step needs."
)

TOOLS_SCHEMA = [
    {
        "type": "function",
        "function": {
            "name": "shell_exec",
            "description": (
                "Execute a shell command in the session's working directory. "
                "stdout and stderr are each returned as either {\"inline\": <text>} "
                "(small output) or a claim-check reference "
                "{\"claim_check_path\": <path>, \"size_bytes\": N, \"head\": ..., "
                "\"tail\": ..., \"exploration_summary\": {...}, \"note\": ...} "
                "(large output). exploration_summary is type-aware: for JSON it "
                "describes the schema/shape (keys, value types, array lengths); "
                "for CSV it lists columns + row count + a few sample rows; for "
                "unstructured text it includes a short natural-language summary "
                "plus line/word counts. For a claim-check reference, use another "
                "shell_exec call (cat, head, tail, grep) against claim_check_path "
                "— which is relative to the session's working directory — to read "
                "whichever specific part of the full output you need beyond what "
                "the summary already tells you."
            ),
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
            "name": "merge_subagent_output",
            # docs/components/session-filesystem.md, "Resolved: Subagent
            # Merge-Back Mechanics" — explicit, model-driven (never
            # automatic). Files argument is optional: omitting it merges the
            # whole subtree; providing a subset merges just those relative
            # paths. Conflict rule (per the doc): a destination written
            # after the subagent's start time is skipped and reported, not
            # silently overwritten.
            "description": (
                "Merge a completed subagent's file changes into this turn's working directory. "
                "Read the subagent's manifest (in its tool-call result) first to see what it "
                "changed. Files newer in the destination than the subagent's start time are "
                "skipped and returned in skipped_conflicts, not silently overwritten. "
                "Destinations you wrote before the subagent started that the subagent then "
                "modified are overwritten and returned in overwrote_parent_earlier."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "subagent_turn_id": {
                        "type": "string",
                        "description": "The subagent's turn_id (also its tool_call_id).",
                    },
                    "files": {
                        "type": "array",
                        "items": {"type": "string"},
                        "description": "Optional subset of the manifest's relative paths to merge. Omit to merge everything.",
                    },
                },
                "required": ["subagent_turn_id"],
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
            "name": _SPAWN_SUBAGENT_TOOL_NAME,
            # docs/components/temporal-workflow.md — turn.go's own
            # tool-call fan-out already dispatches multiple sibling
            # subagent child workflows concurrently within one reasoning
            # step (LCM/Volt's "Tasks" parallel shape), so no separate
            # array-taking tool is needed here: calling this once per
            # desired subagent in the same response already gets that for
            # free. This is the ROOT-turn variant (no delegated_scope/
            # kept_work — see tools_schema_for's substitution for the
            # subagent-issued variant, which requires them).
            "description": (
                "Delegate a self-contained slice of work to a subagent — a fresh turn with its own "
                "isolated working directory (see merge_subagent_output to bring its file changes "
                "back), reasoning independently and returning a summary when done. Use for work "
                "substantial enough to warrant its own focused context, especially when it can run "
                "in parallel with other work — call this multiple times in one response to spawn "
                "several subagents concurrently."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "prompt": {"type": "string", "description": "The self-contained task for the subagent to perform."},
                },
                "required": ["prompt"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "lcm_grep",
            # docs/components/context-slot.md's Memory-Access Tools —
            # named and scoped to match the tool's real behavior: acts only
            # on this session's own conversation history and context-DAG
            # summaries (lcm/ package), never on shell_exec's claim-check
            # files (those already have their own recovery route — cat/
            # head/tail/grep via shell_exec directly).
            "description": (
                "Search this session's own conversation history for a pattern. "
                "mode=\"pattern\" (default) is literal regex matching; mode=\"fulltext\" is "
                "stemmed English keyword search (tolerant of word forms, ranked by relevance) "
                "— neither is semantic/embedding search. Results are grouped by which context "
                "summary, if any, currently covers each match: covered_by_summary_id is null "
                "when the message is already visible in context as-is (nothing to expand), or "
                "a summary_id to pass to lcm_expand when it's been compressed out of view."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "pattern": {"type": "string", "description": "Regex (mode=pattern) or query text (mode=fulltext)."},
                    "mode": {"type": "string", "enum": ["pattern", "fulltext"], "description": "Default \"pattern\"."},
                    "limit": {
                        "type": "number",
                        "description": "Max results to return (default 20). No hard ceiling — raise it if you have real reason to want more.",
                    },
                },
                "required": ["pattern"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "lcm_describe",
            "description": (
                "Look up a single message or context-summary node by id (auto-detects which "
                "kind it is) and return its full detail — role/content/turn info for a "
                "message, or kind/covers/content/token_count/folded_into for a summary. Use "
                "this to inspect what an id from lcm_grep actually is before deciding whether "
                "to call lcm_expand on it."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "id": {"type": "string", "description": "A message_id or summary_id."},
                },
                "required": ["id"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "lcm_expand",
            "description": (
                "Recover the full original messages a context-summary node represents, "
                "walking down through the compression DAG (condensed summaries fold earlier "
                "leaf/condensed summaries) as needed. Reserved for deep investigation into "
                "history that's been compressed out of view — freely re-expanding every "
                "summary back to full text would fight against the reason compaction exists."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "summary_id": {"type": "string", "description": "A summary_id (from lcm_grep or lcm_describe)."},
                },
                "required": ["summary_id"],
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

# docs/components/context-slot.md's Memory-Access Tools — lcm_expand is
# subagent-only at the schema level: excluded from the list entirely for a
# main-agent (top-level) turn rather than listed and rejected at runtime if
# called anyway, per the explicit design decision behind this. lcm_grep/
# lcm_describe carry no such restriction — same "unrestricted" treatment as
# memory_search/memory_expand above.
_SUBAGENT_ONLY_TOOL_NAMES = {"lcm_expand"}

# docs/components/temporal-workflow.md's recursion-termination guard — the
# variant of spawn_subagent offered to a subagent (as opposed to the root
# turn's TOOLS_SCHEMA entry above), requiring delegated_scope/kept_work.
# Schema-level substitution, not a runtime-only check: a subagent-issued
# call literally cannot omit these fields and still validate against its
# own tool schema, matching the paper's own framing ("when a sub-agent, as
# opposed to the root agent, invokes Task, it must provide..."). The actual
# non-empty/genuine-narrowing check still happens in model_call.py at mint
# time (a rejection has to short-circuit dispatch before it becomes a real
# child workflow, which schema validation alone can't guarantee a
# real-world model actually honors) — this substitution is the first line
# of defense, not the only one.
_SPAWN_SUBAGENT_NESTED_SCHEMA = {
    "type": "function",
    "function": {
        "name": _SPAWN_SUBAGENT_TOOL_NAME,
        "description": (
            "Delegate a self-contained slice of YOUR OWN work to a further subagent. Since you "
            "are already a subagent, you must show genuine narrowing of responsibility: "
            "'delegated_scope' names the specific slice being handed off, 'kept_work' names what "
            "you are keeping for yourself. A call that can't articulate real kept_work — i.e. one "
            "that would hand off your entire responsibility — is rejected; perform the work "
            "directly instead of delegating it further."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "prompt": {"type": "string", "description": "The self-contained task for the subagent to perform."},
                "delegated_scope": {
                    "type": "string",
                    "description": "The specific slice of your own work being handed off.",
                },
                "kept_work": {
                    "type": "string",
                    "description": "The work you are keeping for yourself, not delegating.",
                },
            },
            "required": ["prompt", "delegated_scope", "kept_work"],
        },
    },
}


def tools_schema_for(is_subagent: bool) -> list[dict]:
    """model_call.py's one call site for what used to be the flat
    TOOLS_SCHEMA constant — every ModelCall now goes through this so both
    the lcm_expand exclusion and the spawn_subagent variant substitution
    above are enforced uniformly on both the streaming and non-streaming
    call paths, not duplicated at each site."""
    if is_subagent:
        return [
            _SPAWN_SUBAGENT_NESTED_SCHEMA if tool["function"]["name"] == _SPAWN_SUBAGENT_TOOL_NAME else tool
            for tool in TOOLS_SCHEMA
        ]
    return [tool for tool in TOOLS_SCHEMA if tool["function"]["name"] not in _SUBAGENT_ONLY_TOOL_NAMES]


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


# call_model / call_model_streaming moved to
# activities/activities/providers/openai_provider.py (2026-08-28, third
# revision) — each provider owns its own request/response translation
# now, dispatched via Provider ABC (providers/base.py) rather than
# inlined here. Callers (model_call.py, compress_context, exploration_summary)
# use provider.call_model / provider.call_model_streaming /
# provider.summarize_text via the Provider they got from
# llm_client.get_provider(config). This module now only owns the
# provider-neutral pieces: TOOLS_SCHEMA, DEFAULT_SYSTEM_PROMPT,
# build_conversation, RealModelResult.
