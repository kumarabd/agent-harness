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

// testSkillWorkflowOK/testSkillWorkflowCancel are stub domain workflows
// registered only for these tests. They prove turn.go's isSkill dispatch
// branch (docs/05-architecture-domain-control-loops.md, docs/components/
// turn-pipeline.md "Skills") actually starts a child workflow of the
// type-name STRING ModelCall resolved at mint time, awaits it, and reads its
// thin SkillWorkflowOutput via drainResult — no Go-side name-to-function
// registry involved.
func testSkillWorkflowOK(_ workflow.Context, input types.SkillWorkflowInput) (types.SkillWorkflowOutput, error) {
	return types.SkillWorkflowOutput{ToolCallID: input.ToolCallID, Status: "ok"}, nil
}

func testSkillWorkflowBlocksUntilCancelled(ctx workflow.Context, input types.SkillWorkflowInput) (types.SkillWorkflowOutput, error) {
	// Never resolves on its own — the test drives cancellation, exercising
	// the exact path a real skill's own ctx.Done() branch would take.
	ctx.Done().Receive(ctx, nil)
	return types.SkillWorkflowOutput{ToolCallID: input.ToolCallID, Status: "cancelled"}, ctx.Err()
}

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

func TestTurnWorkflow_SkillDispatch(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(testSkillWorkflowOK, workflow.RegisterOptions{Name: "TestSkillWorkflow"})
	mockTurnInfra(env)

	calls := mockModelCallScript(env, []types.ModelCallOutput{
		{
			Status: "working",
			ToolCalls: []types.ToolCallRef{
				{ToolCallID: "t1:act:1", ToolName: "service_monitoring", UseSkill: "TestSkillWorkflow"},
			},
		},
		{Status: "done", HasContent: true},
	})

	env.ExecuteWorkflow(TurnWorkflow, types.TurnInput{
		SessionKey:  "u:web",
		TurnID:      "t1",
		ParentType:  "turn",
		PreInserted: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, *calls)
}

func TestTurnWorkflow_SkillDispatch_CancelledOnInterrupt(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(testSkillWorkflowBlocksUntilCancelled, workflow.RegisterOptions{Name: "TestSkillWorkflow"})
	mockTurnInfra(env)

	mockModelCallScript(env, []types.ModelCallOutput{
		{
			Status: "working",
			ToolCalls: []types.ToolCallRef{
				{ToolCallID: "t1:act:1", ToolName: "service_monitoring", UseSkill: "TestSkillWorkflow"},
			},
		},
		{Status: "done", HasContent: true},
	})

	// A follow-up message mid-flight — turn.go's own interrupt path (docs/
	// components/turn-pipeline.md's interrupt table, "Skill workflow (child)"
	// row): cancels the in-flight skill child, drains it as "cancelled", then
	// folds the follow-up in and keeps looping.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(NewMessageSignalName, types.SignalPayload{Message: types.Message{Role: "user", Content: "never mind"}})
	}, 0)

	env.ExecuteWorkflow(TurnWorkflow, types.TurnInput{
		SessionKey:  "u:web",
		TurnID:      "t1",
		ParentType:  "turn",
		PreInserted: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
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
func testReasoningTurnWorkflow(ctx workflow.Context, _ struct{}) (RunReasonActLoopResult, error) {
	pendingMessages := []types.SignalPayload{}
	cancelRequested := false
	return RunReasonActLoop(ctx, RunReasonActLoopInput{
		TurnID:          "t1:act:1:reason",
		SessionKey:      "u:web",
		ParentType:      "skill",
		PendingMessages: &pendingMessages,
		CancelRequested: &cancelRequested,
		ProgressGen:     new(int),
	})
}

// TestRunReasonActLoop_ScopedReasoningTurn_CtxCancelDoesNotPanic reproduces
// (and regression-tests) a real stuck production turn found 2026-09-27:
// hitting Cancel from the web UI while wedged inside a skill's own scoped
// reasoning turn silently did nothing — the workflow task panicked instead
// of stopping cleanly (indexing PendingMessages[0] on a permanently empty
// slice, since neither flag this loop's own workflow.Await races against
// ever becomes true from ctx cancellation alone), and Temporal retries a
// panicking workflow task forever, so the workflow looked completely
// unresponsive to any further signal, including a second cancel attempt.
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

	env.ExecuteWorkflow(testReasoningTurnWorkflow, struct{}{})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result RunReasonActLoopResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "cancelled_by_user", result.StopReason)
}
