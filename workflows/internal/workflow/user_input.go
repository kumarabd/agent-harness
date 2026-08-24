package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// UserInputResponseSignalName is the signal a UserInputRequestWorkflow waits
// on. Distinct from NewMessageSignalName — a response to a specific pending
// request is never folded into a turn's next reasoning step the way an
// ordinary chat message is; it's routed by SignalWorkflow directly against
// this workflow's own execution, correlated by UserInputRequest.RequestID
// (docs/components/user-input.md, "Resolved: Core Mechanism").
const UserInputResponseSignalName = "UserInputResponse"

// UserInputRequestTimeout — docs/components/user-input.md. A pending
// request waits at most this long before being treated as expired. For
// permission gating specifically this is fail-closed: an unanswered
// approval is treated as denied, never as approved by default. A concrete
// value given directly, not guessed — contrast every other numeric
// threshold in this project (compression thresholds, heartbeat intervals),
// deliberately left unsized pending real usage data.
const UserInputRequestTimeout = 1 * time.Hour

// UserInputRequestWorkflow is the reusable primitive docs/components/user-input.md
// resolves: durably wait for a human's response to a request, however long
// that takes (up to UserInputRequestTimeout), and resume exactly there — a
// workflow-level signal wait, not an activity-level block, since an activity
// can't durably block for potentially an hour (see that doc's "Resolved: Why
// an Activity Can't Do This, a Workflow Can"). Kind-agnostic in general;
// ApprovalGatedCall on the input is permission gating's own opt-in use of
// it — an approval request IS a user input request, not a separate workflow
// type layered on top.
//
// Three ways this wait ends, all handled explicitly:
//  1. The response signal arrives — the intended path.
//  2. UserInputRequestTimeout elapses — expired, fail-closed for permission
//     gating (treated as denied).
//  3. This workflow is cancelled (e.g. the parent turn was interrupted by an
//     unrelated new message while this call was still pending — a real,
//     deliberate resolution of a previously-open question: an unrelated
//     message DOES cancel a pending request here, same as it already
//     cancels any other in-flight tool call). Found and fixed alongside this
//     rewrite: an earlier version of this workflow logged this case but
//     never actually updated the pending Postgres row — left it stuck at
//     'pending' forever. Fixed by routing all three exits through the same
//     CloseUserInput activity call.
func UserInputRequestWorkflow(ctx workflow.Context, input types.UserInputRequestWorkflowInput) (types.UserInputRequestWorkflowOutput, error) {
	logger := workflow.GetLogger(ctx)
	workflowID := workflow.GetInfo(ctx).WorkflowExecution.ID
	req := input.Request

	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)
	if err := workflow.ExecuteActivity(actx, "RequestUserInput", req, workflowID).Get(actx, nil); err != nil {
		return types.UserInputRequestWorkflowOutput{}, err
	}

	signalChan := workflow.GetSignalChannel(ctx, UserInputResponseSignalName)
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	timer := workflow.NewTimer(timerCtx, UserInputRequestTimeout)

	var response types.UserInputResponse
	expired := false

	sel := workflow.NewSelector(ctx)
	sel.AddReceive(signalChan, func(c workflow.ReceiveChannel, more bool) {
		c.Receive(ctx, &response)
	})
	sel.AddFuture(timer, func(f workflow.Future) {
		expired = true
		logger.Info("UserInputRequestWorkflow expired waiting for a response", "request_id", req.RequestID)
	})
	sel.AddReceive(ctx.Done(), func(c workflow.ReceiveChannel, more bool) {
		logger.Info("UserInputRequestWorkflow cancelled while waiting", "request_id", req.RequestID)
	})
	sel.Select(ctx)
	cancelTimer()

	cancelled := ctx.Err() != nil

	// Best-effort Postgres close-out — same tolerance as every other
	// end-of-turn bookkeeping call in this codebase (Persist/Deliver are
	// also fire-and-forget on their own errors). Uses a fresh, non-cancelled
	// activity context deliberately: if this workflow itself was cancelled,
	// ctx is already done, and an activity dispatched under it would never
	// even get scheduled.
	closeStatus := "answered"
	if expired || cancelled {
		closeStatus = "cancelled"
	}
	bg, cancelBg := workflow.NewDisconnectedContext(ctx)
	defer cancelBg()
	cao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	cactx := workflow.WithActivityOptions(bg, cao)
	_ = workflow.ExecuteActivity(cactx, "CloseUserInput", req.RequestID, closeStatus, response.SelectedOptionID, response.FreeText).Get(cactx, nil)

	if cancelled {
		// Found via live testing: an earlier version returned here directly,
		// which correctly closed out user_input_requests (the call above)
		// but skipped markDenied entirely — leaving tool_calls stuck at
		// 'pending' forever for this specific path, the same class of bug
		// just moved one table over. Uses bg (disconnected from the
		// already-cancelled ctx) for the same reason CloseUserInput does.
		if input.ApprovalGatedCall != nil {
			_ = markDenied(bg, input.ApprovalGatedCall.ToolCallID, "cancelled")
		}
		return types.UserInputRequestWorkflowOutput{}, ctx.Err()
	}

	out := types.UserInputRequestWorkflowOutput{Response: response}

	if input.ApprovalGatedCall == nil {
		return out, nil
	}

	approved := !expired && response.SelectedOptionID != nil && *response.SelectedOptionID == "approve"
	if !approved {
		reason := "denied_by_user"
		if expired {
			reason = "expired"
		}
		_ = markDenied(bg, input.ApprovalGatedCall.ToolCallID, reason)
		toolOut := types.ToolCallOutput{ToolCallID: input.ApprovalGatedCall.ToolCallID, Status: "cancelled"}
		out.ToolCallOutput = &toolOut
		return out, nil
	}

	// Approved — dispatch the real ToolCall activity, same timing/retry
	// policy any other call to this tool_name would get (tool_tiers.go).
	timing := toolTimingFor(input.ApprovalGatedCall.ToolName)
	tao := workflow.ActivityOptions{
		ActivityID:          input.ApprovalGatedCall.ToolCallID,
		StartToCloseTimeout: timing.StartToCloseTimeout,
		HeartbeatTimeout:    timing.HeartbeatTimeout,
		WaitForCancellation: true,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	}
	tactx := workflow.WithActivityOptions(ctx, tao)
	var toolOut types.ToolCallOutput
	if err := workflow.ExecuteActivity(tactx, "ToolCall", types.ToolCallInput{ToolCallID: input.ApprovalGatedCall.ToolCallID}).Get(tactx, &toolOut); err != nil {
		toolOut = types.ToolCallOutput{ToolCallID: input.ApprovalGatedCall.ToolCallID, Status: statusFromError(err)}
	}
	out.ToolCallOutput = &toolOut
	return out, nil
}

// markDenied is a small, best-effort helper — the tool_calls row was minted
// by ModelCall with status='pending' (see tool_calls schema note on why that
// default exists); if this call never reaches the real ToolCall activity at
// all (denied, expired, or cancelled before approval), nothing else will
// ever transition it out of 'pending' otherwise.
func markDenied(ctx workflow.Context, toolCallID string, reason string) error {
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)
	return workflow.ExecuteActivity(actx, "DenyToolCall", toolCallID, reason).Get(actx, nil)
}
