package workflow

import (
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/ids"
	"agent-harness/workflows/internal/types"
)

// startTurn is the session's front door: write the inbound message (creating the
// turns row), then start a TurnWorkflow for it. The turn does its own
// classification, retrieval, and reason-act loop internally — the coordinator
// only decides "is a turn already active" (forward the message into it) vs "start
// a new one" (call this).
//
// Returns the child-workflow future (always non-nil on success — the coordinator
// awaits it for completion) and the turn id.
func startTurn(ctx workflow.Context, sessionKey, connectionID string, turnSeq int, msg types.Message, initiatedBy string) (workflow.ChildWorkflowFuture, string, error) {
	logger := workflow.GetLogger(ctx)
	turnID := ids.TurnID(sessionKey, turnSeq)
	turnSeqCopy := turnSeq

	tierA := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, tierA), "InsertMessage", types.InsertMessageInput{
		TurnID:      turnID,
		Message:     msg,
		IsTurnStart: true,
		ParentID:    sessionKey,
		ParentType:  "session",
		TurnSeq:     &turnSeqCopy,
		InitiatedBy: initiatedBy,
	}).Get(ctx, nil); err != nil {
		return nil, "", err
	}

	in := types.TurnInput{
		SessionKey:     sessionKey,
		TurnID:         turnID,
		ParentType:     "session",
		ParentID:       sessionKey,
		TurnSeq:        &turnSeqCopy,
		InitialMessage: msg,
		ConnectionID:   connectionID,
		InitiatedBy:    initiatedBy,
		PreInserted:    true,
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
			return nil, "", err
		}
	}
	return h, turnID, nil
}
