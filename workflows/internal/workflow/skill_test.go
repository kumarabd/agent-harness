package workflow

import (
	"agent-harness/workflows/internal/types"
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"testing"
)

func TestSkillStep_MutationsAreNotRetried(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "write"}[mutation], func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			env.RegisterActivityWithOptions(func(context.Context, types.SkillIteration) (types.SkillDecision, error) {
				return types.SkillDecision{HasCall: true, Mutation: mutation}, nil
			}, activity.RegisterOptions{Name: "SkillReason"})
			attempts := 0
			env.RegisterActivityWithOptions(func(context.Context, types.SkillIteration) (types.SkillObservation, error) {
				attempts++
				return types.SkillObservation{}, errors.New("transport failed")
			}, activity.RegisterOptions{Name: "SkillExecute"})
			env.ExecuteWorkflow(SkillStepWorkflow, types.SkillStepInput{StepID: "call", Kind: "journaling", TenantSlug: "tenant"})
			require.Error(t, env.GetWorkflowError())
			if mutation {
				require.Equal(t, 1, attempts)
			} else {
				require.Equal(t, 3, attempts)
			}
		})
	}
}

func TestSkillStep_BoundedAndRequiresEvidence(t *testing.T) {
	for _, calls := range []bool{false, true} {
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		rounds := 0
		env.RegisterActivityWithOptions(func(context.Context, types.SkillIteration) (types.SkillDecision, error) {
			rounds++
			return types.SkillDecision{HasCall: calls}, nil
		}, activity.RegisterOptions{Name: "SkillReason"})
		env.RegisterActivityWithOptions(func(context.Context, types.SkillIteration) (types.SkillObservation, error) {
			return types.SkillObservation{}, nil
		}, activity.RegisterOptions{Name: "SkillExecute"})
		env.ExecuteWorkflow(SkillStepWorkflow, types.SkillStepInput{StepID: "call", TenantSlug: "tenant"})
		require.Error(t, env.GetWorkflowError())
		if calls {
			require.Equal(t, 12, rounds)
		} else {
			require.Equal(t, 1, rounds)
		}
	}
}
