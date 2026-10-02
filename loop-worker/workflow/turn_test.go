package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"agent-harness/shared/types"
)

// mockTurnInfra registers the detached-child-workflow machinery every
// TurnWorkflow run touches regardless of what the test is actually about
// (compaction/memory write-back checks, docs/components/turn-pipeline.md
// "Safety mechanisms") — real Go workflow types from this same package, with
// their own underlying activities stubbed to succeed instantly.
func mockTurnInfra(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterWorkflow(CompressContextWorkflow)
	env.RegisterWorkflow(WriteMemoryWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) error { return nil },
		activity.RegisterOptions{Name: "CompressContext"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) error { return nil },
		activity.RegisterOptions{Name: "WriteMemory"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ types.InsertMessageInput) error { return nil },
		activity.RegisterOptions{Name: "InsertMessage"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string, _ string) error { return nil },
		activity.RegisterOptions{Name: "Persist"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string, _ string) (string, error) { return "", nil },
		activity.RegisterOptions{Name: "StatusPing"},
	)
}

func mockModelCallScript(env *testsuite.TestWorkflowEnvironment, responses []types.ModelCallOutput) *int {
	calls := 0
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ types.ModelCallInput) (types.ModelCallOutput, error) {
			out := responses[calls]
			if calls < len(responses)-1 {
				calls++
			}
			return out, nil
		},
		activity.RegisterOptions{Name: "ModelCall"},
	)
	return &calls
}

// testReasoningTurnWorkflow runs RunReasonActLoop directly, with
// PendingMessages/CancelRequested never fed — exercising a caller that only
// ever cascade-cancels this loop via ctx, never by signaling it directly.
// Run as the workflow-under-test on its own (not nested inside a real
// TurnWorkflow) so the assertion below observes THIS loop's own outcome
// directly — nesting it under a real TurnWorkflow would let the OUTER turn's
// own (already-correct) cancellation handling mask a bug here entirely,
// since the outer turn stops cleanly on its own cancelChan regardless of
// what happens to an inner child.
func testReasoningTurnWorkflow(ctx workflow.Context, parentType string) (RunReasonActLoopResult, error) {
	pendingMessages := []types.SignalPayload{}
	cancelRequested := false
	return RunReasonActLoop(ctx, RunReasonActLoopInput{
		TurnID:          "t1:act:1:reason",
		SessionKey:      "u:web",
		ParentType:      parentType,
		PendingMessages: &pendingMessages,
		CancelRequested: &cancelRequested,
		ProgressGen:     new(int),
	})
}

// TestRunReasonActLoop_ScopedReasoningTurn_CtxCancelDoesNotPanic reproduces
// (and regression-tests) a real stuck production turn found 2026-09-27:
// hitting Cancel from the web UI while wedged inside a nested reasoning turn
// that never gets its own CancelRequested/PendingMessages fed silently did
// nothing — the workflow task panicked instead of stopping cleanly (indexing
// PendingMessages[0] on a permanently empty slice, since neither flag this
// loop's own workflow.Await races against ever becomes true from ctx
// cancellation alone), and Temporal retries a panicking workflow task
// forever, so the workflow looked completely unresponsive to any further
// signal, including a second cancel attempt. "other" here is just an
// arbitrary non-"session" ParentType value exercising this generic path.
func TestRunReasonActLoop_ScopedReasoningTurn_CtxCancelDoesNotPanic(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testReasoningTurnWorkflow)
	mockTurnInfra(env)

	// This loop's own dispatched tool call — never resolves on its own, so
	// the loop is genuinely blocked in its own workflow.Await when
	// cancellation arrives, exactly like the real stuck turn this
	// regression-tests. Cancelling only once THIS activity has
	// actually started (SetOnActivityStartedListener), not on a bare
	// delayed callback, guarantees the cancellation lands while genuinely
	// parked here — not earlier, while the ModelCall activity that
	// dispatches it is still in flight, which would exercise a different,
	// already-safe error path instead of the one this test is for.
	env.RegisterActivityWithOptions(
		func(ctx context.Context) (types.ToolCallOutput, error) {
			<-ctx.Done()
			return types.ToolCallOutput{}, ctx.Err()
		},
		activity.RegisterOptions{Name: "ToolCall"},
	)
	env.SetOnActivityStartedListener(func(activityInfo *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if activityInfo.ActivityType.Name == "ToolCall" {
			// The exact mechanism a real parent's cancelled context uses to
			// cascade into this child — CancelWorkflow "requests
			// cancellation (through workflow Context)" (SDK doc comment),
			// not a signal this loop would ever see directly.
			env.CancelWorkflow()
		}
	})

	mockModelCallScript(env, []types.ModelCallOutput{
		{
			Status: "working",
			ToolCalls: []types.ToolCallRef{
				{ToolCallID: "t1:act:1:reason:act:1", ToolName: "shell_exec"},
			},
		},
	})

	env.ExecuteWorkflow(testReasoningTurnWorkflow, "other")

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result RunReasonActLoopResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "cancelled_by_user", result.StopReason)
}

// TestRunReasonActLoop_TopLevelTurn_NeverCallsATool_StaysNoToolCalls proves
// an ordinary turn that answers with zero tool calls — a question needing no
// lookup — is completely normal and stays "no_tool_calls", regardless of
// ParentType: every ParentType maps the same way, no special-cased value.
func TestRunReasonActLoop_TopLevelTurn_NeverCallsATool_StaysNoToolCalls(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testReasoningTurnWorkflow)
	mockTurnInfra(env)

	mockModelCallScript(env, []types.ModelCallOutput{
		{Status: "done", HasContent: true},
	})

	env.ExecuteWorkflow(testReasoningTurnWorkflow, "session")

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result RunReasonActLoopResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "no_tool_calls", result.StopReason)
}

// Every conversational turn remains on the ordinary model path.
func TestTurnWorkflow_UsesOrdinaryModelCall(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockTurnInfra(env)
	calls := 0
	env.RegisterActivityWithOptions(func(_ context.Context, in types.ModelCallInput) (types.ModelCallOutput, error) {
		calls++
		require.Equal(t, "t1", in.TurnID)
		return types.ModelCallOutput{Status: "done", HasContent: true}, nil
	}, activity.RegisterOptions{Name: "ModelCall"})
	env.ExecuteWorkflow(TurnWorkflow, types.TurnInput{SessionKey: "u:web", TurnID: "t1", ParentType: "session", PreInserted: true})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, calls)
}

func TestTurnWorkflow_ReturnsUnprocessedSignalsAfterModelFailure(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockTurnInfra(env)
	signaled := false
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name != "ModelCall" || signaled {
			return
		}
		signaled = true
		env.SignalWorkflow(NewMessageSignalName, types.SignalPayload{Message: types.Message{Role: "user", Content: "first", ClientMsgID: "m1"}})
		env.SignalWorkflow(NewMessageSignalName, types.SignalPayload{Message: types.Message{Role: "user", Content: "second", ClientMsgID: "m2"}})
	})
	env.RegisterActivityWithOptions(func(context.Context, types.ModelCallInput) (types.ModelCallOutput, error) {
		return types.ModelCallOutput{}, errors.New("provider unavailable")
	}, activity.RegisterOptions{Name: "ModelCall"})

	env.ExecuteWorkflow(TurnWorkflow, types.TurnInput{SessionKey: "u:web", TurnID: "t1", ParentType: "session", PreInserted: true})
	require.NoError(t, env.GetWorkflowError())
	var result types.TurnResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, []string{"m1", "m2"}, []string{result.UnprocessedMessages[0].Message.ClientMsgID, result.UnprocessedMessages[1].Message.ClientMsgID})
}
