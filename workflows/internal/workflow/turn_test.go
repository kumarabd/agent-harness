package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
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

// testReasoningTurnWorkflow runs RunReasonActLoop directly, standing in for
// skills.RunReasoningTurn (support.go) exactly: PendingMessages/
// CancelRequested are never fed — a scoped reasoning turn is only meant to
// be "cascade-cancelled via ctx when the outer turn is interrupted," never
// signaled directly. Run as the workflow-under-test on its own (not nested
// inside a real TurnWorkflow/skill dispatch) so the assertion below observes
// THIS loop's own outcome directly — nesting it under a real TurnWorkflow
// would let the OUTER turn's own (already-correct) cancellation handling
// mask a bug here entirely, since the outer turn stops cleanly on its own
// cancelChan regardless of what happens to an inner child.
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
// signal, including a second cancel attempt. "skill" here is just an
// arbitrary ParentType value exercising this generic path, not a reference
// to the since-deleted skill-reasoning-turn mechanism.
func TestRunReasonActLoop_ScopedReasoningTurn_CtxCancelDoesNotPanic(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testReasoningTurnWorkflow)
	mockTurnInfra(env)

	// This loop's own dispatched tool call — never resolves on its own, so
	// the loop is genuinely blocked in its own workflow.Await when
	// cancellation arrives, exactly like the real journaling skill blocked
	// on a Notion tool call. Cancelling only once THIS activity has
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

	env.ExecuteWorkflow(testReasoningTurnWorkflow, "skill")

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result RunReasonActLoopResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "cancelled_by_user", result.StopReason)
}

// TestRunReasonActLoop_TopLevelTurn_NeverCallsATool_StaysNoToolCalls proves
// an ordinary turn that answers with zero tool calls — a question needing no
// lookup — is completely normal and stays "no_tool_calls", regardless of
// ParentType. (2026-09-27's "no_action_taken" ParentType=="skill" special
// case — the scoped-reasoning bridge's own patch for a since-deleted
// mechanism, RunReasoningTurn/support.go — is gone entirely now that both
// prior skills are mode-turns instead: every ParentType maps the same way.)
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

// TestJournalingModeTurn_PassesModeToModelCall / TestTurnWorkflow_PassesEmptyModeToModelCall
// prove runTurn's own mode parameter (turn.go) actually reaches
// ModelCallInput.Mode — the one thing model_call.py reads to pick
// llm.JOURNALING_SYSTEM_PROMPT over the session's own stored prompt
// (docs/05-architecture-domain-control-loops.md). JournalingModeTurn and
// TurnWorkflow share runTurn's entire body; this is the one observable
// difference between them under test.
func TestJournalingModeTurn_PassesModeToModelCall(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockTurnInfra(env)

	var gotMode string
	env.RegisterActivityWithOptions(
		func(_ context.Context, in types.ModelCallInput) (types.ModelCallOutput, error) {
			gotMode = in.Mode
			return types.ModelCallOutput{Status: "done", HasContent: true}, nil
		},
		activity.RegisterOptions{Name: "ModelCall"},
	)

	env.ExecuteWorkflow(JournalingModeTurn, types.TurnInput{
		SessionKey:  "u:web",
		TurnID:      "t1",
		ParentType:  "session",
		PreInserted: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, "journaling", gotMode)
}

func TestTurnWorkflow_PassesEmptyModeToModelCall(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockTurnInfra(env)

	var gotMode string
	sawCall := false
	env.RegisterActivityWithOptions(
		func(_ context.Context, in types.ModelCallInput) (types.ModelCallOutput, error) {
			gotMode = in.Mode
			sawCall = true
			return types.ModelCallOutput{Status: "done", HasContent: true}, nil
		},
		activity.RegisterOptions{Name: "ModelCall"},
	)

	env.ExecuteWorkflow(TurnWorkflow, types.TurnInput{
		SessionKey:  "u:web",
		TurnID:      "t1",
		ParentType:  "session",
		PreInserted: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.True(t, sawCall)
	require.Equal(t, "", gotMode)
}
