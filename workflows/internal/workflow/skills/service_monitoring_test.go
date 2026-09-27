package skills

import (
	"context"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

// TestServiceMonitoringSkill_DeclinedSetupNeverStartsReasoning makes the
// setup consent a real control boundary. It covers implicit invocations too:
// an agent may propose monitoring, but it cannot inspect Grafana or arm a
// commitment after the user declines.
func TestServiceMonitoringSkill_DeclinedSetupNeverStartsReasoning(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (map[string]any, error) {
			return map[string]any{
				"service":     "checkout",
				"namespace":   "shop",
				"notify_when": "the service is unavailable",
			}, nil
		},
		activity.RegisterOptions{Name: "ReadSkillCallArguments"},
	)
	env.RegisterWorkflow(wf.UserInputRequestWorkflow)
	env.OnWorkflow(wf.UserInputRequestWorkflow, mock.Anything, mock.Anything).Return(
		types.UserInputRequestWorkflowOutput{Response: types.UserInputResponse{SelectedOptionID: strPtr("decline")}}, nil,
	)

	var closedStatus, closedReason string
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string, status string, _ map[string]any, reason string, _ string) error {
			closedStatus, closedReason = status, reason
			return nil
		},
		activity.RegisterOptions{Name: "CloseSkillCall"},
	)

	env.ExecuteWorkflow(ServiceMonitoringSkill, types.SkillWorkflowInput{
		ToolCallID: "call-2",
		TurnID:     "turn-2",
		SessionKey: "user:web",
		TenantSlug: "tenant-a",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, "cancelled", closedStatus)
	require.Equal(t, "monitoring_not_confirmed", closedReason)
}
