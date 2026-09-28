package workflow

import (
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// JournalingModeTurn — docs/05-architecture-domain-control-loops.md.
// Registered under the workflow type name "journaling" (cmd/loop-worker/
// main.go) — the same string tools.switch_mode's own `mode` argument names,
// which is what coordinator.go's turnWorkflowTypeName resolves to when a
// session's mode is "journaling": the coordinator dispatches this directly
// for the session's next message, the same way it dispatches TurnWorkflow by
// default, not via the model calling a "journaling" tool.
//
// 2026-09-27: replaces the earlier design entirely — a skill dispatched as a
// nested child workflow from within one ordinary turn's own tool-call step,
// running a scoped RunReasoningTurn reasoning bridge with a fixed
// entry_text argument. That design's reasoning turn returned "ok" having
// made zero tool calls at all in a real production incident, and there was
// no way for the activity to persist across more than one message without
// inventing new machinery (a resident workflow, its own idle timer, its own
// signal channel) duplicating what a real turn already has.
//
// This is the exact same body TurnWorkflow itself runs (runTurn, turn.go) —
// same ModelCall/ToolCall dispatch, same approval gating, same tier control,
// same message-forwarding/cancellation/progress-watchdog machinery, same
// LCM-assembled conversational continuity — parameterized only by mode
// "journaling", which model_call.py maps to a curated system prompt
// (llm.JOURNALING_SYSTEM_PROMPT) instead of the session's own stored
// prompt. There is no bespoke Go-side state here at all: staying in this
// mode across many messages, confirming an entry before writing it,
// verifying the write by fetching the page back, and leaving the mode once
// the user is done are all curated PROMPT instructions the model follows
// with its ordinary tools (ask_user, discover_tools/call_tool,
// switch_mode) — not native workflow states, per docs/05-architecture-
// domain-control-loops.md's "agentic loop with deterministic and model-
// driven steps wherever appropriate."
func JournalingModeTurn(ctx workflow.Context, input types.TurnInput) (types.TurnResult, error) {
	return runTurn(ctx, input, "journaling")
}
