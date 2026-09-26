// Package automation contains durable control-plane workflows. Connection
// provisioning is deliberately separate from agent-loop: it holds cluster
// deployment authority and must never run on a tenant task queue.
package automation

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const ConnectionProvisionWorkflowName = "ConnectionProvisionWorkflow"

type ConnectionProvisionInput struct {
	OperationID  string
	ConnectionID string
}

// ConnectionProvisionWorkflow delegates all external state to activities.
// Activity retries make the operation idempotent; the durable operation row
// is the user-visible source of truth, not Temporal history.
func ConnectionProvisionWorkflow(ctx workflow.Context, in ConnectionProvisionInput) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:    20 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	err := workflow.ExecuteActivity(ctx, "ApplyConnection", in.OperationID).Get(ctx, nil)
	if err == nil {
		observe := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 20 * time.Second, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 3}})
		deadline := workflow.Now(ctx).Add(10 * time.Minute)
		for {
			var state string
			err = workflow.ExecuteActivity(observe, "ObserveConnection", in.OperationID).Get(observe, &state)
			if err == nil && (state == "ready" || state == "needs_authorization" || state == "disconnected") {
				break
			}
			if !workflow.Now(ctx).Before(deadline) {
				err = fmt.Errorf("hub did not become available before deadline")
				break
			}
			if e := workflow.Sleep(ctx, 5*time.Second); e != nil {
				err = e
				break
			}
		}
	}
	// A disconnected context also records failures after workflow cancellation.
	finish, _ := workflow.NewDisconnectedContext(ctx)
	finish = workflow.WithActivityOptions(finish, workflow.ActivityOptions{StartToCloseTimeout: 30 * time.Second, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 5}})
	if e := workflow.ExecuteActivity(finish, "FinishConnection", in.OperationID, err != nil).Get(finish, nil); e != nil {
		return e
	}
	return err
}
