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
    speaker_id: str = ""
    # docs/components/gateway/mobile.md — the client-generated id a mobile
    # device stamps on a message it sends, so its optimistic echo can dedupe
    # when the message comes back over the fan-out stream. Empty for anything
    # this process synthesizes.
    client_msg_id: str = ""


@dataclass
class ModelCallInput:
    turn_id: str = ""
    context_seq: int = 0
    # docs/components/turn-pipeline.md, Phase 7 — the PREVIOUS step's
    # report_status tier hint for THIS step, threaded through opaquely by the
    # workflow. Empty on the very first call of a turn —
    # model_registry.default_hint() supplies {language, medium}.
    hint_modality: str = ""
    hint_tier: str = ""
    # Delivery-in-the-loop — offers deliver_reply/deliver_attachment for this
    # one call: turn.go's bounded post-Deliver-failure recovery round.
    offer_delivery_tools: bool = False


@dataclass
class ToolCallRef:
    """One tool call minted by ModelCall — name/ID/dispatch-kind only, no
    arguments. The workflow uses this to decide Activity-vs-child-workflow
    dispatch; it never sees the arguments themselves."""

    tool_call_id: str = ""
    tool_name: str = ""
    is_subagent: bool = False
    # docs/components/turn-pipeline.md — the model called `ask_user`; turn.go
    # dispatches a UserInputRequestWorkflow child and parks the loop. Minted
    # like is_subagent. Never true alongside is_subagent.
    is_ask_user: bool = False
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
class NextStep:
    """docs/components/turn-pipeline.md — the model's advisory next-step hint,
    authored via the peeled `report_status` meta-tool. `tier` picks the next
    ModelCall's model; `est_remaining_steps` raises turn.go's iteration ceiling;
    `note` is carried into the next step's context. `modality` is always
    "language" (not model-authored)."""

    note: str = ""
    modality: str = ""
    tier: str = ""
    est_remaining_steps: int = 0


@dataclass
class ModelCallOutput:
    # docs/components/turn-pipeline.md — the model's declared turn lifecycle:
    # "working" (loop continues), "done" (deliver + terminate), "blocked"
    # (parked — a later phase). The one field turn.go branches on for
    # termination. Synthesized from tool-call presence until the model authors
    # it directly.
    status: str = "working"
    tool_calls: list[ToolCallRef] = field(default_factory=list)
    usage: Usage = field(default_factory=Usage)
    # This step's message had non-whitespace text — turn.go's no-progress
    # guard uses it (a "working" step with no content and no tool calls,
    # twice, is a stuck model).
    has_content: bool = False
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
    # docs/components/turn-pipeline.md — the model's note-to-self + self-selected
    # model for the next step. turn.go copies modality/tier into the next
    # ModelCallInput. None is valid.
    next_step: NextStep | None = None


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
    # Provenance for the turns row, set only on the is_turn_start call
    # (docs/components/proactivity.md): "" / "user" (default), "intn:<id>".
    initiated_by: str = ""


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


# --- docs/components/proactivity.md — intentions ---


@dataclass
class ProbeSpec:
    tool: str = ""
    args: dict[str, Any] = field(default_factory=dict)
    predicate: str = ""


@dataclass
class FireIntentionInput:
    """FireIntention SignalWithStarts the session coordinator's Wake handler."""

    intention_id: str = ""
    session_key: str = ""
    objective: str = ""
    why: str = ""


@dataclass
class CheckConditionInput:
    intention_id: str = ""
    probe: ProbeSpec = field(default_factory=ProbeSpec)


@dataclass
class CheckConditionResult:
    fired: bool = False
    note: str = ""
