// Package skills holds every domain-workflow "skill"
// (docs/05-architecture-domain-control-loops.md, docs/components/
// turn-pipeline.md "Skills") — kept separate from the workflow package
// (turn.go's own dispatch machinery) on purpose: turn.go dispatches a skill
// by its registered type-name STRING (types.ToolCallRef.ResolvedWorkflowType)
// via workflow.ExecuteChildWorkflow, so it never needs to import this
// package or know any individual skill exists. The dependency runs one way —
// this package imports workflow (for the already-exported
// UserInputRequestWorkflow primitive, reused as the approval-gate building
// block, and RunReasonActLoop, the exact same reason-act loop TurnWorkflow
// itself runs, reused for a skill's own scoped reasoning turn below), workflow
// never imports this package. That asymmetry is the architectural point:
// skills are independent of, and unknown to, the core turn loop (docs/
// 05-architecture-domain-control-loops.md, "Skill Workflows Are Independent
// of Subagents").
package skills

import (
	"time"

	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

// activityTimeoutTierA — this package's own Tier A timeout, matching
// workflow.activityTimeoutTierA's value (turn.go) but deliberately not
// imported from there: a skill author's activity calls (ReadSkillCallArguments,
// CloseSkillCall, and whatever a specific skill's own activities need) are a
// different concern from turn.go's internal dispatch timing, even though
// they happen to agree on the same value today.
const activityTimeoutTierA = 30 * time.Second

// closeSkillCall is the one place every skill workflow's exit paths funnel
// through — writes the real result/reason/side_effect to Postgres via the
// CloseSkillCall activity, on a disconnected context so it still runs even
// if ctx itself is already cancelled (same reasoning as
// UserInputRequestWorkflow's own CloseUserInput/DenyToolCall calls —
// user_input.go), and returns the thin status a skill workflow's own return
// value carries. Best-effort, same tolerance every other end-of-turn
// bookkeeping call in this codebase gets.
func closeSkillCall(ctx workflow.Context, toolCallID string, status string, result map[string]any, reason string, sideEffect string) string {
	bg, cancelBg := workflow.NewDisconnectedContext(ctx)
	defer cancelBg()
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(bg, ao)
	_ = workflow.ExecuteActivity(actx, "CloseSkillCall", toolCallID, status, result, reason, sideEffect).Get(actx, nil)
	return status
}

// RunReasoningTurn — docs/05-architecture-domain-control-loops.md. Every
// skill's entry point for delegating interpretive judgment (parsing messy
// external results, deciding whether a write actually succeeded) to a real
// model call, by reusing wf.RunReasonActLoop verbatim — the exact same
// ModelCall dispatch, the exact same status/tool_calls stop condition, the
// exact same real RequiresApproval -> UserInputRequestWorkflow path. That
// last part means ask_user already durably delivers to and waits on the
// user's real connection from inside a nested reasoning turn (confirmed
// against user_input.go's dispatchInterimDelivery) — no new plumbing needed.
//
// The turn this creates is a real turns/messages row (parent_type "skill",
// migration 038), seeded by objective — authored by the calling skill's own
// code, not a real user message or a subagent's tool_calls.arguments (see
// insert_message.py). No signal listener of its own: a scoped reasoning turn
// is never signaled directly, only cascade-cancelled via ctx when the outer
// turn is interrupted, already handled structurally.
//
// idSuffix keeps each of a skill invocation's own reasoning turns distinct
// if it ever needs more than one (":reason" is the common case).
func RunReasoningTurn(ctx workflow.Context, input types.SkillWorkflowInput, idSuffix, objective string) (types.ReasoningTurnOutcome, error) {
	reasoningTurnID := input.ToolCallID + ":" + idSuffix

	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)
	insertInput := types.InsertMessageInput{
		TurnID:      reasoningTurnID,
		Message:     types.Message{Role: "user", Content: objective},
		IsTurnStart: true,
		ParentID:    input.TurnID,
		ParentType:  "skill",
	}
	if err := workflow.ExecuteActivity(actx, "InsertMessage", insertInput).Get(actx, nil); err != nil {
		return types.ReasoningTurnOutcome{Status: "error"}, err
	}

	// Never fed — a scoped reasoning turn has no signal listener of its own
	// (wf.RunReasonActLoopInput's own doc comment on PendingMessages/CancelRequested).
	pendingMessages := []types.SignalPayload{}
	cancelRequested := false
	loopResult, err := wf.RunReasonActLoop(ctx, wf.RunReasonActLoopInput{
		TurnID:             reasoningTurnID,
		SessionKey:         input.SessionKey,
		ConnectionID:       input.ConnectionID,
		ParentType:         "skill",
		OfferDeliveryTools: false,
		PendingMessages:    &pendingMessages,
		CancelRequested:    &cancelRequested,
		Interrupts:         nil,
		ProgressGen:        new(int),
	})
	if err != nil {
		return types.ReasoningTurnOutcome{Status: "error"}, err
	}

	var outcome types.ReasoningTurnOutcome
	sao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	sactx := workflow.WithActivityOptions(ctx, sao)
	if err := workflow.ExecuteActivity(sactx, "SummarizeReasoningTurn", map[string]any{
		"reasoning_turn_id": reasoningTurnID,
		"stop_reason":       loopResult.StopReason,
	}).Get(sactx, &outcome); err != nil {
		return types.ReasoningTurnOutcome{Status: "error"}, err
	}
	return outcome, nil
}
