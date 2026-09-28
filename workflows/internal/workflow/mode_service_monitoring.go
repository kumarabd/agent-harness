package workflow

import (
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// ServiceMonitoringModeTurn — same 2026-09-27 mode mechanism as
// JournalingModeTurn (mode_journaling.go); see that file's own doc comment
// for the shared rationale. Registered under the workflow type name
// "service_monitoring" (cmd/loop-worker/main.go), replacing the old
// skills/service_monitoring.go one-shot SkillWorkflowInput dispatch, whose
// actual investigation/setup work ran through the same RunReasoningTurn
// scoped-reasoning bridge implicated in journaling's false-"Saved"
// incident.
//
// Unlike journaling, this activity is naturally single-round — the user's
// own framing was "the loop starts, executes by looking at tools and
// produces the result, and at that point the model decides to end the loop
// because it completed the ask" — so llm.SERVICE_MONITORING_SYSTEM_PROMPT
// instructs the model to call switch_mode() back to plain chat as soon as
// that one round concludes (armed, or explained a blocker), rather than
// staying resident across messages the way journaling's own prompt does.
// Same mode mechanism, same runTurn body, only the curated prompt's exit
// timing differs — there is no separate "one-shot mode" shape in Go.
func ServiceMonitoringModeTurn(ctx workflow.Context, input types.TurnInput) (types.TurnResult, error) {
	return runTurn(ctx, input, "service_monitoring")
}
