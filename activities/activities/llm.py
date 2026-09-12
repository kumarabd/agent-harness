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

# docs/components/turn-pipeline.md, "Model I/O schema" — the model authors
# `status` + `next_step` every step via this peeled meta-tool. The model has
# no structural way to know this protocol exists without being told, so the
# prompt sentence below is load-bearing, not a nudge. Duplicated as a literal
# (not imported from providers/base.py) for the same reason the providers
# duplicate it back — llm.py ↔ providers is a load-order cycle; kept in sync
# by convention with providers.base.REPORT_STATUS_TOOL_NAME.
_REPORT_STATUS_TOOL_NAME = "report_status"

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
# The final report_status sentence is load-bearing — it's how the model
# authors turn-pipeline.md's `status` + `next_step` output (model_registry's
# escalate-on-retry / tier hinting depends on the tier; turn.go's loop
# termination depends on the status). prompts.go's voiceSystemPromptText
# carries its own equivalent sentence — keep the two in step.
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
    f"Every response, also call {_REPORT_STATUS_TOOL_NAME} alongside anything else you call: "
    "status=working while there is more to do, status=done when the task is complete and your "
    "message IS the answer, status=blocked when you cannot proceed without the user (pair it "
    "with ask_user). Set est_remaining_steps to your honest estimate of reasoning steps left, "
    "and note anything the next step needs to remember."
)

# VOICE_SYSTEM_PROMPT — docs/components/gateway/first-party-plan.md,
# "Voice/text mode". Generalized from prompts.go's voiceSystemPromptText
# (Discord voice's own, session-genesis-frozen prompt) into a per-turn
# selection here: model_call.py picks this over DEFAULT_SYSTEM_PROMPT/the
# session's stored prompt when the turn's own triggering message carries
# mode="voice" — Discord voice keeps working exactly as before (it never
# sends per-message mode, so this branch never touches it; its own copy in
# prompts.go stays the source of truth for that session-genesis path).
# A genuinely standalone prompt, not a diff against DEFAULT_SYSTEM_PROMPT —
# same reasoning prompts.go's own copy documents: voice's formatting
# constraints apply regardless of what the base framing says.
#
# Adds one rule prompts.go's version doesn't have: the incoming message may
# itself be speech-to-text output (missing punctuation, mistranscribed
# words, filler), tailoring the REQUEST side of "voice mode", not just the
# response.
#
# The final report_status sentence must stay in step with
# DEFAULT_SYSTEM_PROMPT's own (see that constant's comment) — it's how the
# model authors turn-pipeline.md's status/next_step output, not a style
# choice.
VOICE_SYSTEM_PROMPT = (
    "You are a helpful, friendly voice assistant. The user is speaking to you out loud, and "
    "your response will be read aloud by a text-to-speech system, not displayed as text — write "
    "accordingly:\n\n"
    "- Never use markdown formatting: no asterisks, no bullet points, no headers, no bold or "
    "italics.\n"
    "- Never use emoji.\n"
    "- Write numbers, times, dates, and abbreviations the way you would actually say them out "
    "loud (for example, \"three thirty,\" not \"3:30\").\n"
    "- Keep responses conversational and reasonably brief — this is a spoken conversation, not a "
    "document. If you have several points to make, say them as connected sentences rather than a "
    "list.\n"
    "- Sound natural and warm, the way a person would speak, not like a formal written answer.\n"
    "- After you use a tool or finish a task, always say the answer or outcome out loud in a "
    "sentence or two — tell the user what you found or what you did. Never end your turn "
    "silently: if you have a result, speak it.\n"
    "- The user's message may be speech-to-text output, not typed text: it can be missing "
    "punctuation, contain mistranscribed words, or include filler (\"um,\" \"like,\" false "
    "starts). Read it charitably — infer the likely intended meaning rather than treating "
    "transcription artifacts as literal or asking the user to repeat themselves unless the "
    "request is genuinely unrecoverable.\n"
    "- If the user asks whether you can speak, whether you have a voice, or anything about how "
    "you're talking to them right now: yes, you are speaking to them — your words are being "
    "converted to speech and played aloud in real time this very moment. Never say you are "
    "text-based, that you don't have a voice, or that you can't speak; that is false in this "
    "conversation and confusing to hear out loud.\n\n"
    f"Every response, also call {_REPORT_STATUS_TOOL_NAME} alongside anything else you call: "
    "status=working while there is more to do, status=done when the task is complete and your "
    "spoken reply is the answer, status=blocked when you cannot proceed without the user (pair it "
    "with ask_user). Set est_remaining_steps to your honest estimate of reasoning steps left, "
    "and note anything the next step needs to remember."
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
            "name": _REPORT_STATUS_TOOL_NAME,
            # docs/components/turn-pipeline.md, "Model I/O schema" — the model's
            # `status` + `next_step` output, transported as a peeled tool call
            # (no provider guarantees native structured output). Rides on the
            # same API call as any other tool_calls, never a separate round trip;
            # the harness peels it in the provider and never dispatches it.
            "description": (
                "Always include this alongside your response, every step. status: \"working\" "
                "while there is more to do, \"done\" when the task is complete and your message "
                "is the answer, \"blocked\" when you cannot proceed without the user (also call "
                "ask_user). tier picks the model for the NEXT step: \"fast\" for simple/"
                "mechanical steps (run a command, report output), \"expert\" for careful "
                "multi-step reasoning or judgment, \"medium\" otherwise. est_remaining_steps is "
                "your honest estimate of reasoning steps still needed. note is a short "
                "note-to-self carried into the next step."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "status": {"type": "string", "enum": ["working", "done", "blocked"]},
                    "tier": {"type": "string", "enum": ["fast", "medium", "expert"]},
                    "est_remaining_steps": {"type": "integer"},
                    "note": {"type": "string"},
                },
                "required": ["status"],
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


# name -> schema dict, over every model-facing schema this module defines.
# `capabilities.schema_for` reads this back; the nested spawn_subagent variant
# is passed separately.
_SCHEMA_BY_NAME: dict[str, dict] = {
    t["function"]["name"]: t
    for t in [
        *TOOLS_SCHEMA,
        _ASK_USER_SCHEMA,
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
    # docs/components/turn-pipeline.md — the model authors these via the
    # peeled `report_status` meta-tool (providers/base.py). `status` empty ⇒
    # the model didn't call report_status this step; model_call.py then
    # synthesizes status from tool-call presence (the Phase-2 fallback,
    # removed in Phase 9). tier empty ⇒ keep the current tier.
    status: str = ""
    next_hint_tier: str = ""
    next_step_note: str = ""
    est_remaining_steps: int = 0
    # Always "language" for now — not model-authored (report_status dropped
    # the modality field). model_registry.resolve still takes a modality arg.
    next_hint_modality: str = "language"


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
