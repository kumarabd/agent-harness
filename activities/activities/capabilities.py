"""The capability table — docs/components/tool-registry.md, "Resolved:
Three-Layer Tool Taxonomy & Per-Task Resolution" and its "Implementation shape".

Everything the model can emit in a response falls into one of three layers:

  - INTERFACE  — open-ended external reach: shell_exec, search_tools, and the
                 per-task *resolved* tools (built per turn from ToolDiscover's
                 rows, not listed here).
  - COGNITION  — reading the agent's own substrate: memory_search / memory_expand
                 (agent-brain), lcm_grep / lcm_describe / lcm_expand (this
                 session's history + compaction DAG).
  - CONTROL    — steering the constructs the agent lives inside: declare_next_step_hint
                 (tier), propose_plan / checkpoint_done (plan), spawn_subagent
                 (subagent tree), the intention tools.

This module is the single declarative source for *which* capabilities exist,
*which turn kinds* expose each one, whether it is *peeled* (a control signal the
harness applies rather than dispatches), its native-activity timing, and its
handler wiring key. It replaces four things that were kept in sync by hand:
`llm.py`'s `tools_schema_for` branch cascade, its `_SUBAGENT_ONLY_TOOL_NAMES`
set, the split between `TOOLS_SCHEMA` and `tools.py`'s `TOOL_REGISTRY`, and the
per-tool timing scattered through `TOOL_REGISTRY`.

Kept pure at module load (no imports from `llm` / `tools`) so `tools.py` can
build `TOOL_REGISTRY` from `CAPABILITIES` without a cycle; `schema_for` reaches
back into `llm.py` for the raw schema dicts lazily.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from enum import Enum


class Layer(str, Enum):
    INTERFACE = "interface"
    COGNITION = "cognition"
    CONTROL = "control"


class TurnKind(str, Enum):
    REASONING = "reasoning"          # a plain / Lite reason-act iteration
    PLANNING = "planning"            # the planning turn — invokes nothing
    CHECKPOINT = "checkpoint"        # executing one plan checkpoint
    PLAN_HANDLING = "plan_handling"  # a mid-plan follow-up turn
    SUBAGENT = "subagent"            # a subagent turn — some schemas swap


@dataclass(frozen=True)
class TimingProfile:
    """The heartbeat/timeout tuning a native `ToolCall` activity needs — the
    shape `tools.py`'s `ToolSpec` carries, minus the handler."""

    heartbeat_interval_seconds: float
    heartbeat_timeout_seconds: float
    start_to_close_timeout_seconds: float


# The three profiles actually in use, named for what distinguishes them (see the
# per-tool comments that used to live in TOOL_REGISTRY):
HEAVY = TimingProfile(3.0, 10.0, 300.0)     # shell_exec / merge_subagent_output — cancellable subprocess / large PV merge
NETWORK = TimingProfile(5.0, 15.0, 30.0)    # agent-brain / mcp-hub / Temporal-client round-trips
LOCAL = TimingProfile(5.0, 15.0, 15.0)      # lcm_* — a Postgres read against this tenant's own DB


@dataclass(frozen=True)
class Capability:
    name: str
    layer: Layer
    turn_kinds: frozenset[TurnKind]
    peel: bool = False
    # key into tools.py's handler map; None for a peeled control signal or for
    # spawn_subagent (dispatched as a child workflow by turn.go, not an activity)
    handler_ref: str | None = None
    timing: TimingProfile = NETWORK
    # spawn_subagent's schema is replaced by the nested variant on a subagent turn
    has_subagent_variant: bool = False
    # set per turn for resolved (ToolDiscover) tools; None for the static table
    schema: dict | None = field(default=None, compare=False)
    # {server, tool} for a resolved mcp-hub tool — carried onto types.ToolCall
    # so turn.go dispatches it through the generic mcp-hub-tier proxy
    resolved_target: tuple[str, str] | None = field(default=None, compare=False)


_ALL = frozenset(TurnKind)
_NONPLAN = frozenset({TurnKind.REASONING, TurnKind.CHECKPOINT, TurnKind.PLAN_HANDLING, TurnKind.SUBAGENT})

# Order mirrors the historical TOOLS_SCHEMA; the plan meta-tools and the
# next-step hint sit at the end. Turn-kind sets reproduce today's
# `tools_schema_for` exactly (the only per-kind filter that ever mattered was
# lcm_expand being subagent-only); the one deliberate normalisation is that
# `propose_plan` / `checkpoint_done` now sit in list position rather than always
# being appended last — order is not load-bearing for any provider.
CAPABILITIES: list[Capability] = [
    Capability("shell_exec", Layer.INTERFACE, _NONPLAN, handler_ref="shell_exec", timing=HEAVY),
    Capability("merge_subagent_output", Layer.CONTROL, _NONPLAN, handler_ref="merge_subagent_output", timing=HEAVY),
    Capability("memory_search", Layer.COGNITION, _NONPLAN, handler_ref="memory_search"),
    Capability("memory_expand", Layer.COGNITION, _NONPLAN, handler_ref="memory_expand"),
    Capability("search_tools", Layer.INTERFACE, _NONPLAN, handler_ref="search_tools"),
    # call_tool is internal-only since the 2026-09-04 per-task-resolution
    # revision (tool-registry.md, "Resolved: Three-Layer Tool Taxonomy") —
    # turn_kinds=() means schema_for never offers it to the model. It keeps a
    # handler_ref so TOOL_REGISTRY still carries its timing profile: `mint_resolved`
    # below hands out resolved tools under their OWN name, and ToolCall (Phase
    # 3, tool_call.py) proxies a resolved dispatch through `tools.call_tool`
    # directly using that profile, not a schema-driven model call.
    Capability("call_tool", Layer.INTERFACE, frozenset(), handler_ref="call_tool"),
    Capability("spawn_subagent", Layer.CONTROL, _NONPLAN, has_subagent_variant=True),
    Capability("create_intention", Layer.CONTROL, _NONPLAN, handler_ref="create_intention"),
    # 5 CRUD ops -> 1 dispatcher (list/inspect/revise/snooze/cancel) —
    # tool-registry.md, "Resolved: Three-Layer Tool Taxonomy".
    Capability("manage_intention", Layer.CONTROL, _NONPLAN, handler_ref="manage_intention"),
    Capability("lcm_grep", Layer.COGNITION, _NONPLAN, handler_ref="lcm_grep", timing=LOCAL),
    Capability("lcm_describe", Layer.COGNITION, _NONPLAN, handler_ref="lcm_describe", timing=LOCAL),
    Capability("lcm_expand", Layer.COGNITION, frozenset({TurnKind.SUBAGENT}), handler_ref="lcm_expand", timing=LOCAL),
    Capability("propose_plan", Layer.CONTROL, frozenset({TurnKind.PLANNING, TurnKind.PLAN_HANDLING}), peel=True),
    Capability("checkpoint_done", Layer.CONTROL, frozenset({TurnKind.CHECKPOINT}), peel=True),
    Capability("declare_next_step_hint", Layer.CONTROL, _ALL, peel=True),
]

BY_NAME: dict[str, Capability] = {c.name: c for c in CAPABILITIES}

# Names with a real activity handler — what tools.py builds TOOL_REGISTRY from.
HANDLER_REFS: dict[str, str] = {c.name: c.handler_ref for c in CAPABILITIES if c.handler_ref}


def turn_kind_of(is_subagent: bool, planning: bool, plan_handling: bool, checkpoint: bool) -> TurnKind:
    """Map `model_call.py`'s existing boolean flags to a single `TurnKind`.
    Mutually exclusive by construction — `model_call.py` computes `is_checkpoint`
    as false whenever `is_subagent`/`planning_mode`/`plan_handling` is set."""
    if planning:
        return TurnKind.PLANNING
    if is_subagent:
        return TurnKind.SUBAGENT
    if plan_handling:
        return TurnKind.PLAN_HANDLING
    if checkpoint:
        return TurnKind.CHECKPOINT
    return TurnKind.REASONING


def schema_for(kind: TurnKind, resolved: "list[Capability] | tuple[Capability, ...]" = ()) -> list[dict]:
    """The model-facing tool schema for a turn: the static capabilities whose
    `turn_kinds` include `kind`, then any per-turn resolved tools appended.
    `spawn_subagent` swaps to its nested variant on a subagent turn."""
    from .llm import _SCHEMA_BY_NAME, _SPAWN_SUBAGENT_NESTED_SCHEMA  # lazy — avoids an import cycle

    out: list[dict] = []
    for c in CAPABILITIES:
        if kind not in c.turn_kinds:
            continue
        if kind is TurnKind.SUBAGENT and c.has_subagent_variant:
            out.append(_SPAWN_SUBAGENT_NESTED_SCHEMA)
        else:
            out.append(_SCHEMA_BY_NAME[c.name])
    out.extend(r.schema for r in resolved if r.schema)
    return out


# Per-task tool resolution (tool-registry.md, "Resolved: Three-Layer Tool
# Taxonomy & Per-Task Resolution") — ToolDiscover's staged rows become
# directly-callable schemas instead of a prompt hint. Capped conservatively:
# some mcp-hub input_schema blobs are large enough that binding all of
# ToolDiscover's top_k=10 would cost more than the old hint block did.
MAX_RESOLVED = 5

_NAME_RE = re.compile(r"[^a-zA-Z0-9_-]")


def _mint_name(server: str, tool: str, taken: set[str]) -> str:
    """A valid, turn-unique OpenAI function name for a resolved tool. Prefers
    the bare tool name (mcp-hub is expected to keep tool identifiers sane and
    to own collision handling upstream); falls back to a server-qualified name
    only if two resolved tools this turn happen to share one."""
    candidate = _NAME_RE.sub("_", tool)[:64] or "tool"
    if candidate not in taken:
        return candidate
    qualified = _NAME_RE.sub("_", f"{server}_{tool}")[:64] or "tool"
    return qualified if qualified not in taken else f"{qualified[:60]}_{len(taken)}"


def mint_resolved(rows: "list[tuple[str, dict | None]]") -> list[Capability]:
    """Turn ToolDiscover's staged `(content, metadata)` rows — `content` =
    "{server}/{tool} — {description}", `metadata` = {server, tool,
    input_schema} — into up to MAX_RESOLVED directly-callable `Capability`
    objects. A row missing a usable `{server, tool, input_schema}` is skipped,
    same "advisory, not restrictive" posture as the rest of discovery: it just
    isn't offered directly, the model still has `search_tools`.

    Keeps the LAST `MAX_RESOLVED`, not the first: `rows` is seq-ordered, and a
    mid-turn `search_tools` call (`tools._persist_discovered`) appends after
    ToolDiscover's pre-turn scan — so when there's more than fits, the
    model's own deliberate follow-up discovery outranks the initial guess,
    not the reverse."""
    out: list[Capability] = []
    taken: set[str] = set()
    for content, metadata in rows:
        if not isinstance(metadata, dict):
            continue
        server, tool, schema = metadata.get("server"), metadata.get("tool"), metadata.get("input_schema")
        if not server or not tool or not isinstance(schema, dict):
            continue
        name = _mint_name(server, tool, taken)
        taken.add(name)
        description = content.split(" — ", 1)[1].strip() if " — " in content else content
        out.append(Capability(
            name=name,
            layer=Layer.INTERFACE,
            turn_kinds=frozenset(),  # not looked up by schema_for's static loop; appended directly
            schema={"type": "function", "function": {"name": name, "description": description, "parameters": schema}},
            resolved_target=(server, tool),
        ))
    return out[-MAX_RESOLVED:]


def route(tool_call_names: "list[str]", active: dict[str, Capability]) -> tuple[list[str], list[str]]:
    """Split a response's tool-call names into (peeled control signals,
    dispatchable). `active` = the static `BY_NAME` table plus any per-turn
    resolved tools.

    NOT yet wired (Phase 3). Today `model_call.py` peels the plan meta-tools
    inline (`plan.split_propose_plan` / `plan.split_checkpoint_done`) and the
    providers strip `declare_next_step_hint` while parsing its tier value; this
    is the shape those converge on once resolved tools are in play.
    """
    peeled: list[str] = []
    dispatch: list[str] = []
    for name in tool_call_names:
        cap = active.get(name)
        (peeled if (cap is not None and cap.peel) else dispatch).append(name)
    return peeled, dispatch
