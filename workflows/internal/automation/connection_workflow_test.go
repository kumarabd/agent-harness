package automation

import (
	"context"
	"errors"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"testing"
)

func TestConnectionWorkflow(t *testing.T) {
	for _, state := range []string{"ready", "needs_authorization", "disconnected", "apply_failed"} {
		t.Run(state, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterActivityWithOptions(func(context.Context, string) error { return nil }, activity.RegisterOptions{Name: "ApplyConnection"})
			env.RegisterActivityWithOptions(func(context.Context, string) (string, error) { return "", nil }, activity.RegisterOptions{Name: "ObserveConnection"})
			env.RegisterActivityWithOptions(func(context.Context, string, bool) error { return nil }, activity.RegisterOptions{Name: "FinishConnection"})
			if state == "apply_failed" {
				env.OnActivity("ApplyConnection", mock.Anything, "operation").Return(errors.New("helm failed")).Times(3)
			} else {
				env.OnActivity("ApplyConnection", mock.Anything, "operation").Return(nil).Once()
				env.OnActivity("ObserveConnection", mock.Anything, "operation").Return("indexing", nil).Once()
				env.OnActivity("ObserveConnection", mock.Anything, "operation").Return(state, nil).Once()
			}
			env.OnActivity("FinishConnection", mock.Anything, "operation", state == "apply_failed").Return(nil).Once()
			env.ExecuteWorkflow(ConnectionProvisionWorkflow, ConnectionProvisionInput{OperationID: "operation", ConnectionID: "connection"})
			require.True(t, env.IsWorkflowCompleted())
			if state == "apply_failed" {
				require.Error(t, env.GetWorkflowError())
			} else {
				require.NoError(t, env.GetWorkflowError())
			}
			env.AssertExpectations(t)
		})
	}
}
