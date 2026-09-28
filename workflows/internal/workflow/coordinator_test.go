package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// mockCoordinatorInfra registers every activity/child-workflow
// CoordinatorWorkflow's own preamble and idle-exit path touch regardless of
// what a test is actually about — same role mockTurnInfra plays in
// turn_test.go, one level up.
func mockCoordinatorInfra(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (int, error) { return 0, nil },
		activity.RegisterOptions{Name: "GetMaxTurnSeq"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ types.InsertMessageInput) error { return nil },
		activity.RegisterOptions{Name: "InsertMessage"},
	)
	env.RegisterWorkflow(WriteMemoryWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string, _ string) error { return nil },
		activity.RegisterOptions{Name: "WriteMemory"},
	)
}

// TestCoordinatorWorkflow_SetModeChangesNextDispatch is this step's own
// central claim under direct test: a session with no active work, once
// SetMode has set its mode to a specific value, starts THAT workflow type
// for the next message instead of the default TurnWorkflow — with zero
// changes to the forwarding-to-an-already-active-worker path (unexercised
// here; that's the pre-existing, unmodified branch of coordinator.go this
// change deliberately left alone).
func TestCoordinatorWorkflow_SetModeChangesNextDispatch(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockCoordinatorInfra(env)

	var startedType string
	stub := func(_ workflow.Context, _ types.TurnInput) (types.TurnResult, error) {
		return types.TurnResult{StopReason: "done"}, nil
	}
	env.RegisterWorkflowWithOptions(stub, workflow.RegisterOptions{Name: "TurnWorkflow"})
	env.RegisterWorkflowWithOptions(
		func(ctx workflow.Context, in types.TurnInput) (types.TurnResult, error) {
			startedType = "journaling-mode"
			return types.TurnResult{StopReason: "done"}, nil
		},
		workflow.RegisterOptions{Name: "journaling-mode"},
	)

	// Deliberately staggered, not both at delay 0: when two signals are
	// already queued before the workflow's very first Select call, the
	// selector's own tie-break (registration order — signalChan is added
	// before setModeChan) can pick NewMessage first, dispatching against
	// the still-default mode before SetMode's callback ever runs. A real
	// gateway/activity call is never truly simultaneous with another either
	// — this models that SetMode's own effect is observably complete before
	// the next message arrives, which is the only ordering this test
	// actually needs to prove.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SetModeSignalName, "journaling-mode")
	}, 0)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(NewMessageSignalName, types.SignalPayload{Message: types.Message{Role: "user", Content: "hi"}})
	}, time.Second)

	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, "journaling-mode", startedType)
}

// TestCoordinatorWorkflow_DefaultModeStartsTurnWorkflow is the fix's own
// guardrail: with no SetMode signal ever sent, a session's default mode
// ("chat") must still dispatch ordinary TurnWorkflow, exactly as before this
// change existed.
func TestCoordinatorWorkflow_DefaultModeStartsTurnWorkflow(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockCoordinatorInfra(env)

	var startedType string
	env.RegisterWorkflowWithOptions(
		func(ctx workflow.Context, in types.TurnInput) (types.TurnResult, error) {
			startedType = "TurnWorkflow"
			return types.TurnResult{StopReason: "done"}, nil
		},
		workflow.RegisterOptions{Name: "TurnWorkflow"},
	)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(NewMessageSignalName, types.SignalPayload{Message: types.Message{Role: "user", Content: "hi"}})
	}, 0)

	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, "TurnWorkflow", startedType)
}
