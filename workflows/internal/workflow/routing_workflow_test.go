package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"agent-harness/workflows/internal/types"
)

// mockSubsystem registers a Go stand-in for a retrieval activity (the real
// ones are Python) under its wire name, returning a fixed result.
func mockSubsystem[I any](env *testsuite.TestWorkflowEnvironment, name string, out types.SubsystemResult, err error) {
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ I) (types.SubsystemResult, error) { return out, err },
		activity.RegisterOptions{Name: name},
	)
}

func TestRoutingWorkflow_FastPath(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	env.ExecuteWorkflow(RoutingWorkflow, RoutingWorkflowInput{
		TurnID: "s:turn:1",
		Task:   types.TaskRepresentation{Intent: "conversational", Complexity: "trivial", Confidence: 0.95},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result RoutingResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.True(t, result.Plan.FastPath)
	require.Equal(t, "skipped", result.Memory.Status)
	require.Equal(t, "skipped", result.Tools.Status)
	require.Equal(t, "skipped", result.Skills.Status)
	require.False(t, result.ComposedSkill)
}

func TestRoutingWorkflow_FullFanOut(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	mockSubsystem[types.MemoryRetrieveInput](env, "MemoryRetrieve", types.SubsystemResult{Status: "ok", Count: 3}, nil)
	mockSubsystem[types.ToolDiscoverInput](env, "ToolDiscover", types.SubsystemResult{Status: "empty", Count: 0}, nil)
	mockSubsystem[types.SkillDiscoverInput](env, "SkillDiscover", types.SubsystemResult{Status: "empty", Count: 0}, nil)

	env.ExecuteWorkflow(RoutingWorkflow, RoutingWorkflowInput{
		TurnID: "s:turn:1",
		Task:   types.TaskRepresentation{Intent: "task", Complexity: "moderate", Confidence: 0.9},
	})

	require.NoError(t, env.GetWorkflowError())
	var result RoutingResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, types.SubsystemResult{Status: "ok", Count: 3}, result.Memory)
	require.Equal(t, "empty", result.Tools.Status)
	require.Equal(t, "empty", result.Skills.Status)
	require.False(t, result.ComposedSkill) // no skill candidates -> ComposeSkill not run
}

func TestRoutingWorkflow_SubsystemError(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	mockSubsystem[types.MemoryRetrieveInput](env, "MemoryRetrieve", types.SubsystemResult{}, context.DeadlineExceeded)
	mockSubsystem[types.ToolDiscoverInput](env, "ToolDiscover", types.SubsystemResult{Status: "ok", Count: 2}, nil)
	mockSubsystem[types.SkillDiscoverInput](env, "SkillDiscover", types.SubsystemResult{Status: "empty"}, nil)

	env.ExecuteWorkflow(RoutingWorkflow, RoutingWorkflowInput{
		TurnID: "s:turn:1",
		Task:   types.TaskRepresentation{Intent: "task", Complexity: "moderate", Confidence: 0.9},
	})

	require.NoError(t, env.GetWorkflowError()) // one failing subsystem never fails routing
	var result RoutingResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "error", result.Memory.Status)
	require.Equal(t, "ok", result.Tools.Status)
}

func TestRoutingWorkflow_ComposeRunsWhenSkillsFound(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	mockSubsystem[types.MemoryRetrieveInput](env, "MemoryRetrieve", types.SubsystemResult{Status: "ok", Count: 1}, nil)
	mockSubsystem[types.ToolDiscoverInput](env, "ToolDiscover", types.SubsystemResult{Status: "ok", Count: 1}, nil)
	mockSubsystem[types.SkillDiscoverInput](env, "SkillDiscover", types.SubsystemResult{Status: "ok", Count: 2}, nil)
	composeCalled := false
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ types.ComposeSkillInput) (types.SubsystemResult, error) {
			composeCalled = true
			return types.SubsystemResult{Status: "ok", Count: 1}, nil
		},
		activity.RegisterOptions{Name: "ComposeSkill"},
	)

	env.ExecuteWorkflow(RoutingWorkflow, RoutingWorkflowInput{
		TurnID: "s:turn:1",
		Task:   types.TaskRepresentation{Intent: "task", Complexity: "complex", Confidence: 0.9},
	})

	require.NoError(t, env.GetWorkflowError())
	var result RoutingResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.True(t, composeCalled)
	require.True(t, result.ComposedSkill)
}
