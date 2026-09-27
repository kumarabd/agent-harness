"""The capability table — docs/components/tool-registry.md, "Resolved:
Three-Layer Tool Taxonomy & Per-Task Resolution" and its "Implementation shape".

Everything the model can emit in a response falls into one of three layers:

  - INTERFACE  — open-ended external reach: shell_exec, discover_tools, and the
                 per-task *resolved* tools (built per turn from discover_tools's
                 rows, not listed here).
  - COGNITION  — reading the agent's own substrate: recall / reflect
                 (agent-brain), lcm_grep /
                 lcm_describe / lcm_expand (this session's history + compaction DAG).
  - CONTROL    — steering the constructs the agent lives inside: report_status
                 (status + next_step), spawn_subagent (subagent tree), the intention tools.

This module is the single declarative source for *which* capabilities exist,
*which turn kinds* (REASONING / SUBAGENT) expose each one, whether it is *peeled*
(a control signal the harness applies rather than dispatches), its
native-activity timing, and its handler wiring key. It replaces the hand-synced
`tools_schema_for` branch cascade, the subagent-only tool set, the
`TOOLS_SCHEMA` / `TOOL_REGISTRY` split, and the scattered per-tool timing.

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
    REASONING = "reasoning"  # a top-level reason-act iteration
    SUBAGENT = "subagent"    # a subagent turn — some schemas swap (nested spawn_subagent, lcm_expand)


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
    # docs/components/turn-pipeline.md — a provisioning action (recall,
    # discover_tools, spawn_subagent, ask_user). Flagged so later
    # phases can exclude these from approval gating / the user-visible stream /
    # the iteration budget. No behavioural effect yet.
    meta: bool = False
    # key into tools.py's handler map; None for a peeled control signal or for
    # spawn_subagent (dispatched as a child workflow by turn.go, not an activity)
    handler_ref: str | None = None
    timing: TimingProfile = NETWORK
    # spawn_subagent's schema is replaced by the nested variant on a subagent turn
    has_subagent_variant: bool = False
    # set per turn for resolved (discover_tools) tools; None for the static table
    schema: dict | None = field(default=None, compare=False)
    # {server, tool} for a resolved mcp-hub tool — carried onto types.ToolCall
    # so turn.go dispatches it through the generic mcp-hub-tier proxy
    resolved_target: tuple[str, str] | None = field(default=None, compare=False)
    # docs/05-architecture-domain-control-loops.md — the Go workflow type name
    # for a resolved skill, mutually exclusive with resolved_target: turn.go
    # dispatches this one as a child workflow (workflow.ExecuteChildWorkflow
    # by type-name string), never through the call_tool/TOOL_REGISTRY path a
    # resolved_target capability uses.
    resolved_workflow_type: str | None = field(default=None, compare=False)


# Both turn kinds see every capability by default; SUBAGENT additionally sees
# lcm_expand and gets the nested spawn_subagent variant (schema_for handles both).
_MAIN = frozenset(TurnKind)

_STATIC_CAPABILITIES: list[Capability] = [
    Capability("shell_exec", Layer.INTERFACE, _MAIN, handler_ref="shell_exec", timing=HEAVY),
    Capability("merge_subagent_output", Layer.CONTROL, _MAIN, handler_ref="merge_subagent_output", timing=HEAVY),
    # Renamed 2026-09-16 (from search_memory/reflect_on_entity) to match the
    # real Hindsight product's own tool naming — docs/components/memory-slot.md's
    # Notes Log.
    Capability("recall", Layer.COGNITION, _MAIN, handler_ref="recall", meta=True),
    Capability("reflect", Layer.COGNITION, _MAIN, handler_ref="reflect"),
    Capability("discover_tools", Layer.INTERFACE, _MAIN, handler_ref="discover_tools", meta=True),
    # docs/05-architecture-domain-control-loops.md — every registered skill is
    # already directly callable by its own name (see the skill entries
    # appended below CAPABILITIES's literal list) — nothing here needs
    # minting. This is an optional detail/search step: exact input_schema, or
    # semantic search once there are more skills than comfortably fit as
    # individually-listed tools.
    Capability("discover_skills", Layer.INTERFACE, _MAIN, handler_ref="discover_skills", meta=True),
    # call_tool is internal-only since the 2026-09-04 per-task-resolution
    # revision (tool-registry.md, "Resolved: Three-Layer Tool Taxonomy") —
    # turn_kinds=() means schema_for never offers it to the model. It keeps a
    # handler_ref so TOOL_REGISTRY still carries its timing profile: `mint_resolved`
    # below hands out resolved tools under their OWN name, and ToolCall (Phase
    # 3, tool_call.py) proxies a resolved dispatch through `tools.call_tool`
    # directly using that profile, not a schema-driven model call.
    Capability("call_tool", Layer.INTERFACE, frozenset(), handler_ref="call_tool"),
    Capability("spawn_subagent", Layer.CONTROL, _MAIN, has_subagent_variant=True, meta=True),
    # No handler_ref — turn.go dispatches a UserInputRequestWorkflow child and
    # parks the loop on it, same as spawn_subagent is a child workflow.
    Capability("ask_user", Layer.CONTROL, _MAIN, meta=True),
    Capability("create_intention", Layer.CONTROL, _MAIN, handler_ref="create_intention"),
    # 5 CRUD ops -> 1 dispatcher (list/inspect/revise/snooze/cancel) —
    # tool-registry.md, "Resolved: Three-Layer Tool Taxonomy".
    Capability("manage_intention", Layer.CONTROL, _MAIN, handler_ref="manage_intention"),
    Capability("lcm_grep", Layer.COGNITION, _MAIN, handler_ref="lcm_grep", timing=LOCAL),
    Capability("lcm_describe", Layer.COGNITION, _MAIN, handler_ref="lcm_describe", timing=LOCAL),
    Capability("lcm_expand", Layer.COGNITION, frozenset({TurnKind.SUBAGENT}), handler_ref="lcm_expand", timing=LOCAL),
    Capability("report_status", Layer.CONTROL, _MAIN, peel=True),
    # Delivery-in-the-loop (2026-09-06): content that won't fit in one platform
    # message is the model's own judgment call (split at natural boundaries vs.
    # attach as a file), not a mechanical Go length-check. turn_kinds=frozenset()
    # — never in any turn's default schema; offered only via schema_for's `also`
    # param, by turn.go's bounded recovery round after an automatic Deliver
    # fails. No handler_ref: dispatched by turn.go straight to the owning gateway
    # connection's own embedded worker (same routing as Deliver/DeliverChunk/
    # DeliverInterim), never through the generic tenant-worker ToolCall path —
    # same shape as spawn_subagent being dispatched as a child workflow.
    Capability("deliver_reply", Layer.CONTROL, frozenset()),
    Capability("deliver_attachment", Layer.CONTROL, frozenset()),
]

# CAPABILITIES/BY_NAME/HANDLER_REFS all include this tenant's own enabled
# skills (docs/05-architecture-domain-control-loops.md — a skill is a static,
# always-on capability, exactly like ask_user/spawn_subagent above, not a
# per-turn-resolved one: there are few of them, hand-authored, so there's no
# reason to gate them behind discovery), rebuilt by load_skills below rather
# than baked in at import time — 2026-09-27, skills moved from a hardcoded
# process-wide list (skills.py) to this tenant's own Postgres `skills` table,
# so they aren't known until tenant_worker.py's own startup query resolves.
# Empty (base capabilities only) until load_skills runs, same "just isn't
# offered yet" shape shell_hub/skill_hub already have before their own init().
CAPABILITIES: list[Capability] = []
BY_NAME: dict[str, Capability] = {}
HANDLER_REFS: dict[str, str] = {}


def load_skills(entries: list[dict]) -> None:
    """Rebuilds CAPABILITIES/BY_NAME/HANDLER_REFS from this tenant's freshly
    loaded skills (skills.init's return value) — tenant_worker.py calls this
    once at startup, after skills.init(pool) and before the Temporal worker
    starts polling, so every real ModelCall sees the tenant's actual enabled
    set. Idempotent (safe to call again if skills are ever reloaded without a
    process restart, though nothing does that today).

    No handler_ref — turn.go dispatches a child workflow of
    resolved_workflow_type, same as ask_user has no handler_ref. The raw
    schema itself is rebuilt into llm.TOOLS_SCHEMA by llm.load_skills (called
    alongside this, same entries), so schema_for's existing
    _SCHEMA_BY_NAME[c.name] lookup finds it with no changes needed there.
    """
    skill_capabilities = [
        Capability(e["name"], Layer.CONTROL, _MAIN, resolved_workflow_type=e["name"]) for e in entries
    ]
    CAPABILITIES[:] = _STATIC_CAPABILITIES + skill_capabilities
    BY_NAME.clear()
    BY_NAME.update({c.name: c for c in CAPABILITIES})
    HANDLER_REFS.clear()
    HANDLER_REFS.update({c.name: c.handler_ref for c in CAPABILITIES if c.handler_ref})


load_skills([])


def turn_kind_of(is_subagent: bool) -> TurnKind:
    return TurnKind.SUBAGENT if is_subagent else TurnKind.REASONING


def schema_for(
    kind: TurnKind,
    resolved: "list[Capability] | tuple[Capability, ...]" = (),
    also: frozenset[str] = frozenset(),
    exclude_skill_name: "str | None" = None,
) -> list[dict]:
    """The model-facing tool schema for a turn: the static capabilities whose
    `turn_kinds` include `kind`, then any per-turn resolved tools appended.
    `spawn_subagent` swaps to its nested variant on a subagent turn.

    `also` force-includes named capabilities regardless of `turn_kinds` —
    for capabilities like `deliver_reply`/`deliver_attachment` that are never
    part of any turn kind's default set, only offered situationally by the
    caller (turn.go's delivery-recovery round, the plan-presentation turn).

    `exclude_skill_name` — model_call.py passes the currently-running
    skill's own name when the CALLING turn's own `parent_type` is "skill" (a
    skill's own scoped reasoning turn, RunReasoningTurn/support.go), so that
    one skill is omitted from that turn's own schema. Found 2026-09-27
    debugging a real stuck production turn: skills default to
    `turn_kinds=_MAIN` like any other always-on capability, so a skill's own
    internal reasoning turn — which reuses this exact function — otherwise
    offers that same skill (itself) as a callable tool, with nothing
    stopping the model from invoking it recursively on itself; that
    recursion has no legitimate case to weigh against (a skill's own
    workflow already owns its retries/approval gate/finish condition).
    Deliberately narrow — only the one skill currently running is excluded,
    not every registered skill: a skill legitimately composing a
    DIFFERENT skill from its own reasoning turn is a real, intentionally
    unforeclosed case, unlike calling itself again mid-flight."""
    from .llm import _SCHEMA_BY_NAME, _SPAWN_SUBAGENT_NESTED_SCHEMA  # lazy — avoids an import cycle

    out: list[dict] = []
    for c in CAPABILITIES:
        if exclude_skill_name is not None and c.name == exclude_skill_name:
            continue
        if kind not in c.turn_kinds and c.name not in also:
            continue
        if kind is TurnKind.SUBAGENT and c.has_subagent_variant:
            out.append(_SPAWN_SUBAGENT_NESTED_SCHEMA)
        else:
            out.append(_SCHEMA_BY_NAME[c.name])
    out.extend(r.schema for r in resolved if r.schema)
    return out


# Per-task tool resolution (tool-registry.md, "Resolved: Three-Layer Tool
# Taxonomy & Per-Task Resolution") — discover_tools's staged rows become
# directly-callable schemas instead of a prompt hint. Capped conservatively:
# some mcp-hub input_schema blobs are large enough that binding all of
# discover_tools's top_k=10 would cost more than the old hint block did.
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
    """Turn discover_tools's staged `(content, metadata)` rows — `content` =
    "{server}/{tool} — {description}", `metadata` = {server, tool,
    input_schema} — into up to MAX_RESOLVED directly-callable `Capability`
    objects. A row missing a usable `{server, tool, input_schema}` is skipped,
    same "advisory, not restrictive" posture as the rest of discovery: it just
    isn't offered directly, the model still has `search_tools`.

    Keeps the LAST `MAX_RESOLVED`, not the first: `rows` is seq-ordered, and a
    mid-turn `search_tools` call (`tools._persist_discovered`) appends after
    discover_tools's pre-turn scan — so when there's more than fits, the
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
