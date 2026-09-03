package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"agent-harness/workflows/internal/types"
)

// mockFire registers a Go stand-in for the Python FireIntention activity,
// recording each call's input.
func mockFire(env *testsuite.TestWorkflowEnvironment, sink *[]types.FireIntentionInput) {
	env.RegisterActivityWithOptions(
		func(_ context.Context, in types.FireIntentionInput) error {
			*sink = append(*sink, in)
			return nil
		},
		activity.RegisterOptions{Name: "FireIntention"},
	)
}

func TestIntentionWorkflow_TimeFiresOnce(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var fires []types.FireIntentionInput
	mockFire(env, &fires)

	env.ExecuteWorkflow(IntentionWorkflow, types.IntentionInput{
		IntentionID: "intn:u:leave-for-airport",
		SessionKey:  "u:web",
		Objective:   "Remind the user to leave for the airport.",
		Kind:        "time",
		FireAt:      env.Now().Add(2 * time.Hour),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, fires, 1)
	require.Equal(t, "intn:u:leave-for-airport", fires[0].IntentionID)
	require.Equal(t, "u:web", fires[0].SessionKey)
}

func TestIntentionWorkflow_SnoozeDelaysFire(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var fires []types.FireIntentionInput
	mockFire(env, &fires)

	// Snooze by 1h, 30m before the original fire time — it should not have
	// fired at the 90-minute mark.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(IntentionSnoozeSignalName, time.Hour)
	}, 90*time.Minute)
	env.RegisterDelayedCallback(func() {
		require.Empty(t, fires, "should not have fired before the snooze extension elapses")
	}, 110*time.Minute)

	env.ExecuteWorkflow(IntentionWorkflow, types.IntentionInput{
		IntentionID: "intn:u:x",
		SessionKey:  "u:web",
		Kind:        "deadline",
		FireAt:      env.Now().Add(2 * time.Hour),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, fires, 1)
}

func TestIntentionWorkflow_InactivityResetKeepsArmed(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var fires []types.FireIntentionInput
	mockFire(env, &fires)

	// Reset at 1d and again at 2d — the 2-day idle timer never completes, so
	// by 2.5d nothing has fired.
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(IntentionResetSignalName, nil) }, 24*time.Hour)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(IntentionResetSignalName, nil) }, 48*time.Hour)
	env.RegisterDelayedCallback(func() {
		require.Empty(t, fires, "resets should keep the intention armed")
		env.CancelWorkflow()
	}, 60*time.Hour)

	env.ExecuteWorkflow(IntentionWorkflow, types.IntentionInput{
		IntentionID: "intn:u:nudge",
		SessionKey:  "u:web",
		Kind:        "inactivity",
		IdleFor:     48 * time.Hour,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.Empty(t, fires)
}

func TestIntentionWorkflow_ConditionPollsThenFires(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var fires []types.FireIntentionInput
	mockFire(env, &fires)

	calls := 0
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ types.CheckConditionInput) (types.CheckConditionResult, error) {
			calls++
			return types.CheckConditionResult{Fired: calls >= 3}, nil
		},
		activity.RegisterOptions{Name: "CheckCondition"},
	)

	env.ExecuteWorkflow(IntentionWorkflow, types.IntentionInput{
		IntentionID: "intn:u:stock",
		SessionKey:  "u:web",
		Kind:        "condition",
		PollEvery:   10 * time.Minute,
		Probe:       &types.ProbeSpec{Tool: "quotes/get", Predicate: "price below 100"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 3, calls)
	require.Len(t, fires, 1)
}

func TestIntentionWorkflow_ZeroFireAtFiresImmediately(t *testing.T) {
	// This is how a recurring intention works: a Schedule starts a
	// kind="time" IntentionWorkflow with no FireAt each tick.
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var fires []types.FireIntentionInput
	mockFire(env, &fires)

	env.ExecuteWorkflow(IntentionWorkflow, types.IntentionInput{
		IntentionID: "intn:u:daily-review", SessionKey: "u:web",
		Objective: "Review recent episodes.", Kind: "time",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, fires, 1)
}

func TestIntentionWorkflow_UnknownKindFails(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockFire(env, &[]types.FireIntentionInput{})

	env.ExecuteWorkflow(IntentionWorkflow, types.IntentionInput{
		IntentionID: "intn:u:x", SessionKey: "u:web", Kind: "bogus",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
}
