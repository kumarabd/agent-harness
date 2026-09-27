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

// TestJournalingSkill_DeclinedConfirmationNeverStartsReasoning proves the
// first native state has real control ownership: even if the parent model
// invoked journaling implicitly, a declined confirmation closes the skill
// before the legacy Notion reasoning bridge can start.
func TestJournalingSkill_DeclinedConfirmationNeverStartsReasoning(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (map[string]any, error) {
			return map[string]any{"entry_text": "I felt hopeful after the walk."}, nil
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

	env.ExecuteWorkflow(JournalingSkill, types.SkillWorkflowInput{
		ToolCallID: "call-1",
		TurnID:     "turn-1",
		SessionKey: "user:web",
		TenantSlug: "tenant-a",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, "cancelled", closedStatus)
	require.Equal(t, "entry_not_confirmed", closedReason)
}

func strPtr(s string) *string { return &s }
