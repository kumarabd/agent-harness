"""JSON-serializable shapes shared with the Go workflow layer.

Mirrors workflows/internal/types/types.go by hand (not code-generated, in this
slice). Field names match the JSON tags on the Go side exactly, since Temporal's
default data converter is plain JSON — these dataclasses only need to round-trip
through dict/JSON, they don't need to be identical Python objects.

Reshaped 2026-08-14 for the reference-passing contract
(docs/components/temporal-workflow.md, "Resolved: Reference-Passing Contract"
and "Resolved: Reference/ID Schema"): every activity input/output that used to
carry message content, tool arguments, or tool results now carries only IDs
and control-flow metadata. Content-bearing types (the old ModelResponse,
ToolCall.arguments, ToolResult.result/reason/side_effect as workflow-visible
fields) are gone from this file — that data now lives exclusively in Postgres,
read/written by the activity implementations directly (see db.py).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class Usage:
    input_tokens: int = 0
    output_tokens: int = 0


@dataclass
class Message:
    role: str = ""
    content: str = ""


@dataclass
class ModelCallInput:
    turn_id: str = ""
    context_seq: int = 0
    # docs/components/model-registry.md, "Resolved: Selection Mechanism" —
    # the PREVIOUS step's self-declared hint for THIS step, threaded through
    # opaquely by the workflow (it never interprets these, just passes
    # ModelCallOutput's hint fields into the next ModelCallInput). Empty
    # string on the very first call of a turn — model_registry.default_hint()
    # is what actually supplies {language, medium} in that case, not a
    # literal default here, so the workflow doesn't need to know the
    # registry's own bootstrap value.
    hint_modality: str = ""
    hint_tier: str = ""
    # docs/components/request-pipeline/02-request-understanding.md — step 2's
    # complexity estimate, threaded through so ModelCall can bootstrap the
    # turn's FIRST tier from it (empty hint_tier only). Empty for subagents
    # and when step 2 fell back.
    complexity: str = ""


@dataclass
class ClassifyRequestInput:
    """ClassifyRequest's only input — docs/components/request-pipeline/
    02-request-understanding.md. The activity reads the turn's seed user
    message (and a little recent context) from Postgres itself and returns a
    TaskRepresentation."""

    turn_id: str = ""


@dataclass
class TaskRepresentation:
    """Step 2's output. Small derived routing signals only — the two routing
    scalars (intent/complexity), the classifier's confidence, a distilled
    retrieval query, and a few named entities. Carried by the workflow the
    same way ModelCallOutput's next_hint_tier and ToolCallRef's {server,tool}
    are: routing metadata derived from the message, not the message content
    itself (which stays Postgres-side). retrieval_query/entities are passed
    straight into the step-4/5/7 retrieval activities by RetrievalWorkflow.
    confidence == 0.0 marks a fallback / un-classified turn."""

    intent: str = ""
    complexity: str = ""
    confidence: float = 0.0
    retrieval_query: str = ""
    entities: list[str] = field(default_factory=list)


@dataclass
class MemoryRetrieveInput:
    """MemoryRetrieve's input — docs/components/request-pipeline/
    04-memory-retrieval.md. retrieval_query is the distilled query from step
    2's TaskRepresentation, a small derived signal passed straight in by
    RoutingWorkflow (not read from Postgres)."""

    turn_id: str = ""
    retrieval_query: str = ""


@dataclass
class ToolDiscoverInput:
    """ToolDiscover's input — docs/components/request-pipeline/
    07-tool-discovery.md."""

    turn_id: str = ""
    retrieval_query: str = ""
    entities: list[str] = field(default_factory=list)


@dataclass
class SkillDiscoverInput:
    """SkillDiscover's input — docs/components/request-pipeline/
    05-skill-discovery.md."""

    turn_id: str = ""
    retrieval_query: str = ""


@dataclass
class ComposeSkillInput:
    """ComposeSkill's input — docs/components/request-pipeline/
    06-skill-composition.md. Reads the staged memory / tool / skill rows from
    turn_retrieval by turn_id itself."""

    turn_id: str = ""


@dataclass
class RecordSkillOutcomeInput:
    """RecordSkillOutcome's input — docs/components/skill-subsystem.md,
    "Recording". The activity reads the turn's transcript, tool calls, and
    staged skill rows from Postgres itself; the workflow only supplies the
    turn_id and the loop's stop reason (which it can't persist cleanly)."""

    turn_id: str = ""
    stop_reason: str = ""


@dataclass
class SkillSynthesizeInput:
    """SkillSynthesize's input — docs/components/skill-subsystem.md,
    "Synthesis". The activity processes the whole un-synthesized candidate
    queue; this carries only the triggering turn for logging."""

    trigger_turn_id: str = ""


@dataclass
class SubsystemResult:
    """What each retrieval-phase activity returns to RoutingWorkflow — a
    status and the count of rows it staged to turn_retrieval. No content: the
    rows are read from turn_retrieval by later steps. status is
    "ok" | "empty" | "error" as returned by the activity; RoutingWorkflow may
    additionally record "timed_out" / "skipped" in the same shape."""

    status: str = "empty"
    count: int = 0


@dataclass
class ToolCallRef:
    """One tool call minted by ModelCall — name/ID/dispatch-kind only, no
    arguments. The workflow uses this to decide Activity-vs-child-workflow
    dispatch; it never sees the arguments themselves."""

    tool_call_id: str = ""
    tool_name: str = ""
    is_subagent: bool = False
    # docs/components/user-input.md — computed here, at mint time, since
    # this is the one place with the real arguments in memory (workflow code
    # never has them). Never true alongside is_subagent in this first pass.
    requires_approval: bool = False
    # Resolved {server, tool} identity behind this call — dispatch/routing
    # metadata crossing the reference-passing boundary the same way
    # tool_name already does, not the call's actual arguments. Only set
    # when requires_approval is True.
    server: str = ""
    tool: str = ""


@dataclass
class ModelCallOutput:
    has_tool_calls: bool = False
    tool_calls: list[ToolCallRef] = field(default_factory=list)
    usage: Usage = field(default_factory=Usage)
    # docs/components/context-slot.md — the assembled context's estimated
    # size (lcm.py's estimate_tokens), computed fresh in Python each call
    # since the Go workflow can't accumulate this itself across separate
    # turn-workflow executions (see lcm.assemble's own docstring).
    context_tokens: int = 0
    # docs/components/context-slot.md, "Responsibilities" — the model
    # actually used this call's real context window (model_registry.py),
    # so the workflow can size the compression threshold as a fraction of
    # it instead of a fixed constant.
    context_window: int = 0
    # docs/components/model-registry.md, "Resolved: Selection Mechanism" —
    # this step's self-declared hint for the NEXT step. Threaded back into
    # the next ModelCallInput unmodified by the workflow.
    next_hint_modality: str = "language"
    next_hint_tier: str = "medium"


@dataclass
class ToolCallInput:
    """ToolCall's only input — it reads its own arguments from Postgres via
    this ID (docs/components/temporal-workflow.md)."""

    tool_call_id: str = ""


@dataclass
class ToolCallOutput:
    """ToolCall's only output — status, not result/reason/side_effect. Those
    stay in Postgres; the workflow only needs to know ok/error/cancelled to
    decide retry-count bookkeeping."""

    tool_call_id: str = ""
    status: str = "ok"  # "ok" | "error" | "cancelled"


@dataclass
class InsertMessageInput:
    """Input for the message-insert activity — the one place content still
    crosses an activity input boundary, since it's the literal handoff from
    the coordinator's signal payload (already durable via Temporal signal
    history) into Postgres. messages.seq is computed by the activity itself
    (MAX(seq)+1 within its own turn), not passed in.

    is_turn_start marks the one call per turn that also creates the turns
    row. On that call, if parent_type == "turn" (a subagent), `message` is
    ignored — the activity derives the subagent's own inbound content from
    its *own* tool_calls.arguments row instead, since the workflow never has
    that content to pass along."""

    turn_id: str = ""
    message: Message = field(default_factory=Message)
    is_turn_start: bool = False
    parent_id: str = ""
    parent_type: str = ""
    turn_seq: int | None = None


@dataclass
class SignalPayload:
    """What SignalWithStart / a follow-up signal carries into the Session
    Coordinator. scripted_model_responses is gone — test fixtures are written
    directly to _test_scripted_responses by the starter CLI, never passed
    through the workflow (see workflows/cmd/starter)."""

    message: Message = field(default_factory=Message)


@dataclass
class UserInputOption:
    id: str = ""
    label: str = ""


@dataclass
class UserInputRequest:
    """docs/components/user-input.md — kind-agnostic; context is opaque here,
    interpreted only by whichever consumer built the request (permission
    gating, a future decision-request consumer)."""

    request_id: str = ""
    turn_id: str = ""
    kind: str = ""
    prompt: str = ""
    options: list[UserInputOption] = field(default_factory=list)
    allow_free_text: bool = False
    context: dict = field(default_factory=dict)


@dataclass
class UserInputResponse:
    request_id: str = ""
    selected_option_id: str | None = None
    free_text: str | None = None
