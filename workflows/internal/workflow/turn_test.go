package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// testSkillWorkflowOK/testSkillWorkflowCancel are stub domain workflows
// registered only for these tests — they don't exercise
// DraftNoteSkillWorkflow's own real logic (that's scenario-tested against a
// live cluster, workflows/scenarios/), just prove turn.go's isSkill dispatch
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
				{ToolCallID: "t1:act:1", ToolName: "draft_note", IsSkill: true, ResolvedWorkflowType: "TestSkillWorkflow"},
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
				{ToolCallID: "t1:act:1", ToolName: "draft_note", IsSkill: true, ResolvedWorkflowType: "TestSkillWorkflow"},
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
