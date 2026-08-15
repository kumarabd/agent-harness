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


@dataclass
class ToolCallRef:
    """One tool call minted by ModelCall — name/ID/dispatch-kind only, no
    arguments. The workflow uses this to decide Activity-vs-child-workflow
    dispatch; it never sees the arguments themselves."""

    tool_call_id: str = ""
    tool_name: str = ""
    is_subagent: bool = False


@dataclass
class ModelCallOutput:
    has_tool_calls: bool = False
    tool_calls: list[ToolCallRef] = field(default_factory=list)
    usage: Usage = field(default_factory=Usage)


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
