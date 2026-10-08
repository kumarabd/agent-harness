package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"agent-harness/shared/types"
)

// mockWake registers a Go stand-in for the Python WakeSession activity,
// recording each call's input. Same shape the old IntentionWorkflow test used
// for FireIntention.
func mockWake(env *testsuite.TestWorkflowEnvironment, sink *[]types.WakeSessionInput, err error) {
	env.RegisterActivityWithOptions(
		func(_ context.Context, in types.WakeSessionInput) error {
			*sink = append(*sink, in)
			return err
		},
		activity.RegisterOptions{Name: "WakeSession"},
	)
}

// A wake is one activity call and a completion — no triggers, no signals, no
// state carried between firings. The Schedule owns the cadence.
func TestWakeWorkflow_WakesOnceAndCompletes(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var wakes []types.WakeSessionInput
	mockWake(env, &wakes, nil)

	env.ExecuteWorkflow(WakeWorkflow, types.WakeInput{
		WakeID:     "acct:web:weekly-review",
		SessionKey: "acct:web",
		TenantSlug: "acct",
		Objective:  "Review the week and say what to change.",
		Why:        "You asked for this on Sunday evenings.",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, wakes, 1)
	require.Equal(t, "acct:web:weekly-review", wakes[0].WakeID)
	require.Equal(t, "acct:web", wakes[0].SessionKey)
	require.Equal(t, "Review the week and say what to change.", wakes[0].Objective)
	require.Equal(t, "You asked for this on Sunday evenings.", wakes[0].Why)
}

// A wake that cannot reach its session must fail loudly. There is no fallback
// delivery path and no silent success — the Schedule's next tick is the retry,
// and a schedule that keeps failing shows up in its own action history.
func TestWakeWorkflow_PropagatesFailure(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var wakes []types.WakeSessionInput
	mockWake(env, &wakes, errors.New("coordinator unreachable"))

	env.ExecuteWorkflow(WakeWorkflow, types.WakeInput{
		WakeID:     "acct:web:x",
		SessionKey: "acct:web",
		TenantSlug: "acct",
		Objective:  "x",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
}
