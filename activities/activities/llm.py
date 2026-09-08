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
# docs/components/turn-pipeline.md — the static core. Deliberately compact: the
# same prompt is in front of the model on every reasoning step, including the
# ones that are just "read this tool result and continue", so task-shape
# guidance stays light (illustrative, not a decision procedure) and there is no
# planning/lane machinery to describe.
#
# The final declare_next_step_hint sentence is load-bearing (model_registry's
# escalate-on-retry / tier hinting depends on it) and platform_prompts.go's
# voiceSystemPromptText copies it verbatim — keep it byte-for-byte.
DEFAULT_SYSTEM_PROMPT = (
    "You are a capable, general-purpose personal assistant with real tools — not limited to "
    "coding. You have direct shell access (shell_exec) for local and system tasks.\n\n"
    "HOW YOU WORK. Let the shape of your work follow the task, and don't announce it: answer "
    "directly when you already can; look things up or explore the environment first when you "
    "can't; break large or multi-part work into pieces — a running checklist in your scratchpad, "
    "or subagents for parts that are self-contained. For anything that takes more than a couple "
    "of steps, keep a plan-and-progress note at ./scratchpad.md in your working directory; it "
    "stays in front of you every step and is never compacted away.\n\n"
    "PROVISIONING. Some capabilities are already offered to you directly this turn — call them "
    "by name like any other tool. To reach beyond what you have:\n"
    "- search_memory — recall context about the user, people, or past decisions from long-term "
    "memory (memory_expand for the raw detail behind a result).\n"
    "- discover_tools — find a tool that isn't already offered; a match becomes callable by its "
    "own name on your NEXT step, not the response that found it.\n"
    "- load_skill — pull in a step-by-step procedure from a past successful run of a similar task.\n"
    "- spawn_subagent — delegate a self-contained slice of work to its own focused turn.\n"
    "- ask_user — put a question to the user and wait for their answer (the turn pauses; they "
    "may also just send a new message, which is the answer).\n"
    "When you need more than one of these, request them together in a single step rather than "
    "one per turn, and take your first real action in the same response wherever you can.\n\n"
    "INFORMATION — three rules:\n"
    "1. Answer from what's already in front of you first — the conversation so far and any "
    "context you've been given hold most of what you need. Only reach for a retrieval tool "
    "(search_memory, a discovered tool, a file) when the answer genuinely needs a fact that "
    "ISN'T already present — then get it before answering rather than guessing. Don't loop on "
    "retrieval: if a couple of attempts turn up nothing, answer with what you have and say "
    "what's missing.\n"
    "2. When you do answer from your own training knowledge, say so explicitly, every time: "
    "\"I don't have current data on this — from general knowledge, ...\".\n"
    "3. If the request is ambiguous, the target unclear, or an action is destructive or hard to "
    "undo, ask the user before proceeding rather than assuming. If something is missing but not "
    "blocking what you're doing right now, note it or call create_intention to follow up later.\n\n"
    "After using a tool, summarize the result in plain text for the user rather than leaving it "
    "as raw output. "
    f"Every response, also call {_NEXT_STEP_HINT_TOOL_NAME} alongside anything else you call, "
    "declaring what the next step needs."
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
            "name": "search_memory",
            # Description mirrors agent-brain's own tool description
            # (internal/mcp/tools.go) verbatim-ish — the model is calling
            # agent-brain directly, not a paraphrased wrapper.
            "description": (
                "Recall relevant context from past conversations and long-term memory. "
                "Searches the full semantic layer at once: episodic memory units, promoted "
                "generalized facts and relationships, raw asserted facts and relationships, "
                "rules/constraints, and concept definitions, fused into one ranked list. "
                "Use memory_expand on a result's id to recover the raw episodes behind it."
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
                "Recover raw, verbatim episodes backing a search_memory result. Always "
                "full-depth — the raw events and facts, in chronological order. Use when the "
                "content already attached to a search_memory result isn't specific enough."
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
            "name": "discover_tools",
            "description": (
                "Semantically search the tools available across all registered MCP "
                "backends, plus locally-available shell/CLI capabilities, for something not "
                "already offered to you directly. Returns candidates with a server, tool name, "
                "description, and input schema. A match becomes directly callable by its own "
                "tool name (or shell_exec, for a shell result) starting your NEXT step, not "
                "this one — so don't expect to invoke it in the same response that found it."
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
            "description": "Invoke a tool discovered via discover_tools, on the backend that owns it.",
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
                    "fire_at": {"type": "string", "description": "ISO-8601 timestamp (kind=time/deadline). Compute this relative to the actual current date — check it first (e.g. via shell_exec); never assume or recall a date from memory."},
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


# Delivery-in-the-loop (2026-09-06, docs/components/activities-outbound-delivery.md's
# already-resolved "Retry Policy: Model-Driven, Not a Static Playbook" applied
# to delivery itself): never in a turn's default schema (capabilities.py's
# turn_kinds=frozenset()) — offered only via schema_for's `also` param, by
# turn.go's bounded post-Deliver-failure recovery round or the plan-workflow
# presentation turn. Each call is one platform message; the model decides
# how/whether to split a long reply across several deliver_reply calls, or
# switch to deliver_attachment, informed by whatever it retrieved from
# skills/seeds/deliver-long-content.json — not a mechanical char-count cut.
_DELIVER_REPLY_SCHEMA = {
    "type": "function",
    "function": {
        "name": "deliver_reply",
        "description": (
            "Send content to the user as one message on their platform. Call it more than once "
            "to split a long reply across several messages — split at natural boundaries "
            "(paragraphs, sections), not an arbitrary character count. Fails with a "
            "content_too_long error (and the limit) if this call's content still doesn't fit; "
            "on that, split further or switch to deliver_attachment."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "content": {"type": "string", "description": "The text to send as one message."},
            },
            "required": ["content"],
        },
    },
}

_DELIVER_ATTACHMENT_SCHEMA = {
    "type": "function",
    "function": {
        "name": "deliver_attachment",
        "description": (
            "Send content to the user as a file attachment instead of inline text — the right "
            "choice for long structured content (a plan, a checklist, code) that doesn't read "
            "well split across several messages."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "content": {"type": "string", "description": "The full content to write to the file."},
                "filename": {"type": "string", "description": "A short, descriptive filename (e.g. \"plan.md\")."},
            },
            "required": ["content", "filename"],
        },
    },
}


_ASK_USER_SCHEMA = {
    "type": "function",
    "function": {
        "name": "ask_user",
        "description": (
            "Ask the user a question and wait for their answer before continuing. Use when the "
            "request is ambiguous, the decision is theirs to make, or you're missing something "
            "required that you can't look up. The turn pauses until they respond — or they may "
            "reply with a new message, which you should treat as the answer."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "question": {"type": "string", "description": "The question to put to the user."},
                "options": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "Optional. If the answer is a choice among a few options, list them — they render as buttons.",
                },
            },
            "required": ["question"],
        },
    },
}


_LOAD_SKILL_SCHEMA = {
    "type": "function",
    "function": {
        "name": "load_skill",
        "description": (
            "Pull a procedure from past successful runs into your context — a step-by-step "
            "guide for a kind of task. Describe the task you're about to do; the closest "
            "matching procedure is returned as an observation to follow (adapt or ignore it "
            "where the situation differs). If several match, you get their titles to pick "
            "from; if none do, you get the titles of what exists."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "query": {"type": "string", "description": "Natural-language description of the task you're about to do."},
            },
            "required": ["query"],
        },
    },
}


# name -> schema dict, over every model-facing schema this module defines.
# `capabilities.schema_for` reads this back; the nested spawn_subagent variant
# is passed separately.
_SCHEMA_BY_NAME: dict[str, dict] = {
    t["function"]["name"]: t
    for t in [
        *TOOLS_SCHEMA,
        _ASK_USER_SCHEMA,
        _LOAD_SKILL_SCHEMA,
        _DELIVER_REPLY_SCHEMA,
        _DELIVER_ATTACHMENT_SCHEMA,
    ]
}


def tools_schema_for(
    is_subagent: bool,
    resolved: "list | tuple" = (),
    offer_delivery_tools: bool = False,
) -> list[dict]:
    """`model_call.py`'s one call site for the model-facing tool schema. Thin
    adapter to `capabilities.schema_for`. `resolved` is the per-turn list of
    `Capability` objects `discover_tools` produced.

    `offer_delivery_tools` — turn.go's delivery-recovery round — force-includes
    deliver_reply/deliver_attachment via `schema_for`'s `also`, since those two
    are never in a turn kind's default set (situational, not standing).
    """
    from . import capabilities

    kind = capabilities.turn_kind_of(is_subagent)
    also = frozenset({"deliver_reply", "deliver_attachment"}) if offer_delivery_tools else frozenset()
    return capabilities.schema_for(kind, resolved, also)


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
    conn, turn_id: str, system_prompt: str,
) -> tuple[list[dict], int, list]:
    """Thin call-through to `prompt.assemble` (docs/components/turn-pipeline.md's
    prompt-assembly section — static core + pinned scratchpad + LCM conversation;
    the stable call site model_call.py uses). Returns
    `(conversation, context_tokens, resolved_tools)` — `resolved_tools` is the
    per-turn set of directly-callable `Capability` objects `discover_tools`
    produced, handed to `tools_schema_for`.
    """
    return await prompt.assemble(conn, turn_id, system_prompt)


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
