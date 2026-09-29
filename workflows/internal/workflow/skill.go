package workflow

import (
	"time"

	"agent-harness/workflows/internal/types"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const conversationStateQuery = "ConversationState"
const skillCommandUpdate = "SkillCommand"

// SkillStepWorkflow executes a bounded domain step from an explicitly confirmed
// content reference. Its skill owns prompt and tool policy. Only chat delivers
// to the user; no raw NewMessage channel exists in this workflow.
func SkillStepWorkflow(ctx workflow.Context, in types.SkillStepInput) (types.SkillObservation, error) {
	ctx = WithTenantTaskQueue(ctx, in.TenantSlug)
	for i := 0; i < 12; i++ {
		iteration := types.SkillIteration{StepID: in.StepID, ContentID: in.ContentID, Kind: in.Kind, Index: i}
		ao := workflow.ActivityOptions{StartToCloseTimeout: 90 * time.Second,
			RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 3}}
		var decision types.SkillDecision
		if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, ao), "SkillReason", iteration).Get(ctx, &decision); err != nil {
			return types.SkillObservation{}, err
		}
		if !decision.HasCall {
			return types.SkillObservation{}, temporal.NewNonRetryableApplicationError("skill stopped before its completion criterion was met", "SkillIncomplete", nil)
		}
		// Without provider idempotency support, an ambiguous external write
		// must be inspected rather than automatically retried.
		if decision.Mutation {
			ao.RetryPolicy.MaximumAttempts = 1
		}
		ao.WaitForCancellation = true
		ao.HeartbeatTimeout = 10 * time.Second
		var observation types.SkillObservation
		if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, ao), "SkillExecute", iteration).Get(ctx, &observation); err != nil {
			return types.SkillObservation{}, err
		}
		if observation.Complete {
			return observation, nil
		}
	}
	return types.SkillObservation{}, temporal.NewNonRetryableApplicationError("skill exceeded twelve tool rounds", "SkillStepLimit", nil)
}
