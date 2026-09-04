"""Real LLM integration for ModelCall (activities/activities/model_call.py).

This module owns the provider-NEUTRAL side of the LLM call: the tools
schema every provider advertises, the default system prompt, the
`build_conversation` call-through (full assembly now lives in `prompt.py`
— request-pipeline step 9), and the RealModelResult return shape. The
actual per-provider request/response translation lives in
activities/activities/providers/ (Provider ABC — OpenAI-compatible
covers real OpenAI, DeepSeek, Qwen/DashScope, Groq, OpenRouter, Crusoe,
etc.; Anthropic is its own class). See docs/components/model-registry.md.

Only invoked when no test fixture exists for a given (turn_id, context_seq) —
model_call.py's fixture-first branch is unchanged; this module only fills in
what used to be a `raise RuntimeError("no real model provider configured")`.

`messages` only stores plain role/content rows — there's no OpenAI-shaped
representation of an assistant's prior tool_calls or their results anywhere
in the schema. `lcm.assembly.assemble` (reached via `prompt.assemble`)
reconstructs a valid OpenAI conversation from the existing tables (messages +
tool_calls) with no new columns: an assistant message that minted tool calls
gets its `tool_calls` array rebuilt from the tool_calls rows keyed by
message_id, each immediately followed by a `role: "tool"` message carrying
that call's result, matched by tool_call_id (tool_calls.tool_call_id is
reused verbatim as OpenAI's tool_call_id — any string works, no second ID
scheme needed).

This module owns the raw JSON **schema dicts** (`TOOLS_SCHEMA` + the plan
meta-tool schemas + the nested spawn_subagent variant) — pure data. Which turn
kinds see each, whether it's peeled, its layer and its native-activity timing
all live in `capabilities.py` (docs/components/tool-registry.md, "Resolved:
Three-Layer Tool Taxonomy & Per-Task Resolution"). `tools_schema_for` is now a
thin adapter into `capabilities.schema_for`; `tools.TOOL_REGISTRY` derives its
handler+timing wiring from `capabilities.CAPABILITIES`. Adding a model-facing
tool = one schema dict here + one `Capability` row + one `_HANDLERS` entry in
`tools.py`. `tools.TOOL_REGISTRY`'s `search`/`slow_tool`/`noop_tool` remain
fixture-only stubs and must never be offered to a real model.

**Prompt assembly** — request pipeline step 9
(docs/components/request-pipeline/09-prompt-assembly.md), `prompt.py` — owns
the whole ordered, budget-bounded conversation: `lcm.assemble`'s summary
DAG + verbatim window, then retrieved skills (step 5), the plan ledger
(step 8), discovered tools (step 7), and long-term memory (step 4), each
staged by the request pipeline's retrieval phase and read fresh every
ModelCall. `build_conversation` here is kept only as model_call.py's stable
call site.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from . import prompt
from .types import Usage

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

# docs/components/request-pipeline/08-planning.md (Phase 3C, plan-and-execute) —
# the planning turn's system prompt. This turn does NOT execute anything: it
# reads the task (and any composed skill / retrieved procedures already in the
# prompt), decides the shape of the work, and emits a checkpoint plan via
# propose_plan. PlanWorkflow then runs one checkpoint turn per checkpoint, and
# each of those may re-plan the remainder — so the plan is a first draft, not a
# contract. Keep checkpoints coarse (a handful, each a meaningful unit of
# progress with an observable 'done when'), not a keystroke-level script.
PLANNING_SYSTEM_PROMPT = (
    "You are planning a task, not executing it. Think through what the task requires, draw on any "
    "procedure or skill already shown in your context, and lay out a short ordered list of "
    "checkpoints — each a meaningful unit of progress with an observable condition that means it's "
    "done. Aim for a handful of coarse steps, not a line-by-line script; the agent executing each "
    "checkpoint can re-plan the rest as it learns more. Mark a checkpoint complex=true when it is "
    "itself a multi-step subtask worth its own plan. Call propose_plan with your checkpoints "
    "and nothing else — set needs_approval=true when the work is risky, expensive, or hard to "
    "reverse and the user should see the plan before it runs; leave it off for routine work. "
    f"Also call {_NEXT_STEP_HINT_TOOL_NAME}, declaring what the first checkpoint needs."
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
    # docs/components/proactivity.md — the agent's own standing intentions.
    # Each is an IntentionWorkflow execution (no table); these tools start /
    # signal / cancel / query it via the Temporal client (tools_intention.py).
    {
        "type": "function",
        "function": {
            "name": "create_intention",
            "description": (
                "Arm a standing intention — something you should keep watching for or doing on the "
                "user's behalf, beyond this turn (\"remind me to leave 2h before my flight\", "
                "\"tell me when the deploy goes green\", \"every weekday morning give me my priorities\"). "
                "When it triggers, you get woken with a fresh turn to decide whether and how to act. "
                "The bar is high — arm one only when there's a real, lasting reason to."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "objective": {"type": "string", "description": "What you're committing to, in the user's terms."},
                    "why": {"type": "string", "description": "Optional one line of context carried to the future turn."},
                    "kind": {
                        "type": "string",
                        "enum": ["time", "deadline", "condition", "state", "event", "inactivity", "schedule"],
                        "description": "time/deadline = fire once at fire_at; condition/state/event = poll a probe until it holds; inactivity = fire if the user goes quiet for idle_for_seconds; schedule = recurring, needs cron or every_seconds.",
                    },
                    "fire_at": {"type": "string", "description": "ISO-8601 timestamp (kind=time/deadline)."},
                    "idle_for_seconds": {"type": "number", "description": "Seconds of user silence before firing (kind=inactivity)."},
                    "cron": {"type": "string", "description": "Cron expression, UTC (kind=schedule) — e.g. \"0 9 * * MON-FRI\"."},
                    "every_seconds": {"type": "number", "description": "Fixed interval in seconds (kind=schedule), alternative to cron."},
                    "poll_every_seconds": {"type": "number", "description": "Poll interval (kind=condition/state/event; default 300)."},
                    "expires_at": {"type": "string", "description": "ISO-8601; give up unfired after this (poll kinds)."},
                    "probe": {
                        "type": "object",
                        "description": "What to check each poll (kind=condition/state/event).",
                        "properties": {
                            "tool": {"type": "string", "description": "A call_tool \"server/tool\" to run."},
                            "args": {"type": "object", "description": "Arguments for that tool."},
                            "predicate": {"type": "string", "description": "Natural-language condition to judge against the result."},
                        },
                        "required": ["tool", "predicate"],
                    },
                },
                "required": ["objective", "kind"],
            },
        },
    },
    # docs/components/tool-registry.md, "Resolved: Three-Layer Tool Taxonomy"
    # — the 5 CRUD operations on an armed intention (everything but create)
    # collapsed into one dispatcher tool. These are operations on one
    # construct, not 5 distinct intents, unlike e.g. memory_search vs.
    # lcm_grep (different substrates, deliberately left separate).
    {
        "type": "function",
        "function": {
            "name": "manage_intention",
            "description": (
                "List, inspect, revise, snooze, or cancel your armed intentions. "
                "list needs nothing else. inspect/revise/snooze/cancel need intention_id."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "action": {"type": "string", "enum": ["list", "inspect", "revise", "snooze", "cancel"]},
                    "intention_id": {"type": "string", "description": "Required for every action except list."},
                    "objective": {"type": "string", "description": "revise: the new objective."},
                    "why": {"type": "string", "description": "revise: the new one-line context."},
                    "fire_at": {"type": "string", "description": "revise: new ISO-8601 fire time."},
                    "poll_every_seconds": {"type": "number", "description": "revise: new poll interval."},
                    "by_seconds": {"type": "number", "description": "snooze: push the next fire out by this many seconds."},
                },
                "required": ["action"],
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
# subagent-only at the schema level (excluded from a main-agent turn's schema
# entirely, not listed-and-rejected). That rule now lives as data:
# `capabilities.CAPABILITIES` gives lcm_expand `turn_kinds={SUBAGENT}` while
# lcm_grep / lcm_describe carry the full non-planning set.

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


# docs/components/request-pipeline/08-planning.md (Phase 3C) — the checkpoint
# turn's completion report. A meta-tool like declare_next_step_hint: it rides
# the response's existing round-trip, carries no work of its own, and ModelCall
# peels it to apply against PLAN.md rather than minting a tool_calls row. Only
# offered to a checkpoint turn (the seed message names the checkpoint).
_CHECKPOINT_DONE_SCHEMA = {
    "type": "function",
    "function": {
        "name": "checkpoint_done",
        "description": (
            "You are executing one checkpoint of a plan. Call this once the checkpoint's "
            "'done when' condition is met: status \"done\", or \"skipped\" if you deliberately "
            "bypassed it, or \"revised\" (with a note) if the task diverged from what the step "
            "assumed. If what you found means the REST of the plan should change, pass "
            "revised_tail — an ordered list of the remaining checkpoints, which replaces every "
            "still-pending step after this one."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "checkpoint_id": {
                    "type": "string",
                    "description": "The cp id from the seed message (e.g. \"cp2\").",
                },
                "status": {"type": "string", "enum": ["done", "skipped", "revised"]},
                "note": {
                    "type": "string",
                    "description": "Why the step was revised or skipped, or anything the next checkpoint needs to know.",
                },
                "revised_tail": {
                    "type": "array",
                    "description": "Optional. The remaining plan, re-planned: replaces all still-pending checkpoints after this one.",
                    "items": {
                        "type": "object",
                        "properties": {
                            "intent": {"type": "string", "description": "What this step accomplishes."},
                            "done_when": {"type": "string", "description": "The observable condition that means it's complete."},
                        },
                        "required": ["intent"],
                    },
                },
            },
            "required": ["checkpoint_id", "status"],
        },
    },
}


_PROPOSE_PLAN_SCHEMA = {
    "type": "function",
    "function": {
        "name": "propose_plan",
        "description": (
            "Emit the checkpoint plan for this task: an ordered list of coarse steps, each with an "
            "intent and an observable 'done_when'. This is the only tool you call on a planning turn."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "checkpoints": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "properties": {
                            "intent": {"type": "string", "description": "What this step accomplishes."},
                            "done_when": {"type": "string", "description": "The observable condition that means it's complete."},
                            "complex": {
                                "type": "boolean",
                                "description": "True if this step is itself a multi-step subtask that deserves its own plan (it will be run as a nested planning+execution pass). Leave off for ordinary steps.",
                            },
                        },
                        "required": ["intent"],
                    },
                },
                "needs_approval": {
                    "type": "boolean",
                    "description": "True if the plan should be shown to the user for approval before execution begins.",
                },
            },
            "required": ["checkpoints"],
        },
    },
}


# name -> schema dict, over every model-facing schema this module defines
# (the base list plus the two plan meta-tools). `capabilities.schema_for`
# reads this back; the nested spawn_subagent variant is passed separately.
_SCHEMA_BY_NAME: dict[str, dict] = {
    t["function"]["name"]: t
    for t in [*TOOLS_SCHEMA, _PROPOSE_PLAN_SCHEMA, _CHECKPOINT_DONE_SCHEMA]
}


def tools_schema_for(
    is_subagent: bool,
    planning: bool = False,
    plan_handling: bool = False,
    checkpoint: bool = False,
    resolved: "list | tuple" = (),
) -> list[dict]:
    """`model_call.py`'s one call site for the model-facing tool schema.

    The turn-kind rules — lcm_expand being subagent-only, the spawn_subagent
    nested-variant swap, which turns see propose_plan / checkpoint_done — now
    live as data in `capabilities.CAPABILITIES` (tool-registry.md, "Resolved:
    Three-Layer Tool Taxonomy"). This is a thin adapter from the historical
    boolean flags to `capabilities.schema_for`. `resolved` is the per-turn
    list of `Capability` objects `ToolDiscover` produced (empty until Phase 3).
    """
    from . import capabilities

    kind = capabilities.turn_kind_of(is_subagent, planning, plan_handling, checkpoint)
    return capabilities.schema_for(kind, resolved)


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


async def build_conversation(
    conn, turn_id: str, plan_id: str, system_prompt: str, context_window: int = 0, *, planning: bool = False,
) -> tuple[list[dict], int, list]:
    """Thin call-through to `prompt.assemble` — request pipeline step 9
    (docs/components/request-pipeline/09-prompt-assembly.md) owns the section
    model, ordering, and budget arbitration; this stays the stable call site
    model_call.py already uses. `plan_id` (docs/components/episode-lifecycle.md)
    keys the enrichment sections; empty for a conversational fast-path turn.
    `context_window` (0 if unknown, e.g. the fixture path) bounds how much of it
    enrichment may consume before `prompt.assemble` starts shedding sections.

    Returns `(conversation, context_tokens, resolved_tools)` — `resolved_tools`
    (docs/components/tool-registry.md, "Resolved: Three-Layer Tool Taxonomy &
    Per-Task Resolution") is the per-task set of directly-callable `Capability`
    objects `model_call.py` hands to `tools_schema_for`; always empty when
    `planning=True` (that turn gets a reference catalog in-prompt instead).
    """
    return await prompt.assemble(conn, turn_id, plan_id, system_prompt, context_window, planning=planning)


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
