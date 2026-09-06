package workflow

import (
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/ids"
	"agent-harness/workflows/internal/types"
)

// WorkKind is what dispatchWork decided to do with an inbound top-level message.
type WorkKind string

const (
	WorkTurn   WorkKind = "turn"   // a plain TurnWorkflow (Lite / conversational)
	WorkPlan   WorkKind = "plan"   // a PlanWorkflow (fresh Deliberate task-run)
	WorkAttach WorkKind = "attach" // forward the message into a running PlanWorkflow
)

// WorkResult is dispatchWork's decision + handle.
type WorkResult struct {
	Kind       WorkKind
	WorkflowID string                       // the started (or, for Attach, the target) workflow id
	Handle     workflow.ChildWorkflowFuture // nil for Attach
}

// dispatchWork is the session's front-door router (docs/components/request-pipeline/
// 08-planning.md — "the coordinator classifies and dispatches", the
// production-standard supervisor/entity pattern). Called by the coordinator at
// the one point it used to start a TurnWorkflow.
//
//  1. InsertMessage (creates the turns row + seq-0 message) — the coordinator
//     owns intake.
//  2. ClassifyRequest — no fallback: a persistent failure returns an error the
//     coordinator surfaces rather than starting anything.
//  3. ResolveOpenPlan — is a Deliberate task-run already in progress for this
//     session, and does this message continue it?
//  4. Branch:
//     - Lite / conversational → a plain TurnWorkflow (PreInserted).
//     - Deliberate, fresh task-run → a PlanWorkflow ("<turn_id>:plan").
//     - Deliberate, continues a running plan → Attach: the caller forwards the
//       message into the already-running PlanWorkflow.
//     - Deliberate, supersedes a running plan → signal it `abandon`, then start
//       a fresh PlanWorkflow.
//
// Top-level only — a subagent still classifies inside turn.go and, if
// Deliberate, opens its own single-loop plan there (a Deliberate subagent
// getting its own PlanWorkflow is 3C-iii, for a recursed checkpoint).
func dispatchWork(ctx workflow.Context, sessionKey, connectionID string, turnSeq int, msg types.Message, initiatedBy string) (WorkResult, error) {
	logger := workflow.GetLogger(ctx)
	turnID := ids.TurnID(sessionKey, turnSeq)
	turnSeqCopy := turnSeq

	tierA := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	bounded := workflow.ActivityOptions{
		StartToCloseTimeout: activityTimeoutTierA,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	}

	// 1. intake
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, tierA), "InsertMessage", types.InsertMessageInput{
		TurnID:      turnID,
		Message:     msg,
		IsTurnStart: true,
		ParentID:    sessionKey,
		ParentType:  "session",
		TurnSeq:     &turnSeqCopy,
		InitiatedBy: initiatedBy,
	}).Get(ctx, nil); err != nil {
		return WorkResult{}, err
	}

	// 2. classify (no fallback)
	var task types.TaskRepresentation
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, bounded), "ClassifyRequest", types.ClassifyRequestInput{TurnID: turnID}).Get(ctx, &task); err != nil {
		return WorkResult{}, err
	}

	// 3. lane decision
	if task.Intent == "conversational" {
		return startPlainTurn(ctx, sessionKey, connectionID, turnID, &turnSeqCopy, msg, initiatedBy, "", nil)
	}

	// Is a Deliberate task already in progress for this session, and does this
	// message continue it? (decision B — no `episodes` table; ResolveOpenPlan
	// checks for a running PlanWorkflow + the classifier's continues_prior.)
	var resolve types.ResolveOpenPlanResult
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, bounded), "ResolveOpenPlan", types.ResolveOpenPlanInput{
		SessionKey: sessionKey,
		TurnID:     turnID,
		Task:       task,
	}).Get(ctx, &resolve); err != nil {
		return WorkResult{}, err
	}
	logger.Info("dispatch: resolved", "turn_id", turnID, "plan_id", resolve.PlanID,
		"continue", resolve.ShouldContinue, "supersede", resolve.Supersede)

	if resolve.ShouldContinue {
		return WorkResult{Kind: WorkAttach, WorkflowID: resolve.PlanID + ":plan"}, nil
	}
	if resolve.Supersede {
		// A new task arrived while an old plan runs — tell it to wrap up.
		_ = workflow.SignalExternalWorkflow(ctx, resolve.PlanID+":plan", "", PlanAbandonSignalName, turnID).Get(ctx, nil)
	}

	// 4. branch on the lane
	if !laneIsDeliberate(task) {
		return startPlainTurn(ctx, sessionKey, connectionID, turnID, &turnSeqCopy, msg, initiatedBy, "", &task)
	}

	// Deliberate, fresh task-run → a PlanWorkflow. plan id == this turn id.
	planWFID := turnID + ":plan"
	cwo := workflow.ChildWorkflowOptions{
		WorkflowID:        planWFID,
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
	}
	h := workflow.ExecuteChildWorkflow(workflow.WithChildOptions(ctx, cwo), PlanWorkflow, types.PlanWorkflowInput{
		PlanID:       turnID,
		SessionKey:   sessionKey,
		ConnectionID: connectionID,
		InitiatedBy:  initiatedBy,
		Task:         task,
	})
	var we workflow.Execution
	if err := h.GetChildWorkflowExecution().Get(ctx, &we); err != nil {
		if temporal.IsWorkflowExecutionAlreadyStartedError(err) {
			logger.Info("plan workflow already running, attaching", "plan_id", planWFID)
		} else {
			return WorkResult{}, err
		}
	}
	return WorkResult{Kind: WorkPlan, WorkflowID: planWFID, Handle: h}, nil
}

// startPlainTurn starts a plain TurnWorkflow (ParentType "session"), PreInserted
// (dispatchWork already did InsertMessage). planID/task are threaded through so
// the turn skips its own ClassifyRequest when the router already resolved it.
func startPlainTurn(ctx workflow.Context, sessionKey, connectionID, turnID string, turnSeq *int, msg types.Message, initiatedBy, planID string, task *types.TaskRepresentation) (WorkResult, error) {
	logger := workflow.GetLogger(ctx)
	in := types.TurnInput{
		SessionKey:     sessionKey,
		TurnID:         turnID,
		ParentType:     "session",
		ParentID:       sessionKey,
		TurnSeq:        turnSeq,
		InitialMessage: msg,
		ConnectionID:   connectionID,
		InitiatedBy:    initiatedBy,
		PreInserted:    true,
		PlanID:         planID,
		Task:           task,
	}
	cwo := workflow.ChildWorkflowOptions{
		WorkflowID:        turnID,
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
	}
	h := workflow.ExecuteChildWorkflow(workflow.WithChildOptions(ctx, cwo), TurnWorkflow, in)
	var we workflow.Execution
	if err := h.GetChildWorkflowExecution().Get(ctx, &we); err != nil {
		if temporal.IsWorkflowExecutionAlreadyStartedError(err) {
			logger.Info("turn already running, attaching", "turn_id", turnID)
		} else {
			return WorkResult{}, err
		}
	}
	return WorkResult{Kind: WorkTurn, WorkflowID: turnID, Handle: h}, nil
}
