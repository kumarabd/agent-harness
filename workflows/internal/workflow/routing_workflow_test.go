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
}

func TestRoutingWorkflow_FullFanOut(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	mockSubsystem[types.MemoryRetrieveInput](env, "MemoryRetrieve", types.SubsystemResult{Status: "ok", Count: 3}, nil)
	mockSubsystem[types.ToolDiscoverInput](env, "ToolDiscover", types.SubsystemResult{Status: "empty", Count: 0}, nil)
	mockSubsystem[types.SkillDiscoverInput](env, "SkillDiscover", types.SubsystemResult{Status: "empty", Count: 0}, nil)

	env.ExecuteWorkflow(RoutingWorkflow, RoutingWorkflowInput{
		TurnID: "s:turn:1",
		PlanID: "s:turn:1", // planning turn — skills stage under the plan
		Task:   types.TaskRepresentation{Intent: "task", Complexity: "moderate", Confidence: 0.9},
	})

	require.NoError(t, env.GetWorkflowError())
	var result RoutingResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, types.SubsystemResult{Status: "ok", Count: 3}, result.Memory)
	require.Equal(t, "empty", result.Tools.Status)
	require.Equal(t, "empty", result.Skills.Status)
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

func TestRoutingWorkflow_SubagentInheritsParentMemory(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var gotMemInput types.MemoryRetrieveInput
	env.RegisterActivityWithOptions(
		func(_ context.Context, in types.MemoryRetrieveInput) (types.SubsystemResult, error) {
			gotMemInput = in
			return types.SubsystemResult{Status: "ok", Count: 2}, nil
		},
		activity.RegisterOptions{Name: "MemoryRetrieve"},
	)
	mockSubsystem[types.ToolDiscoverInput](env, "ToolDiscover", types.SubsystemResult{Status: "empty"}, nil)
	mockSubsystem[types.SkillDiscoverInput](env, "SkillDiscover", types.SubsystemResult{Status: "empty"}, nil)

	env.ExecuteWorkflow(RoutingWorkflow, RoutingWorkflowInput{
		PlanID:       "s:turn:1:sub:1",
		TurnID:       "s:turn:1:sub:1",
		Task:         types.TaskRepresentation{Intent: "task", Complexity: "moderate", Confidence: 0.9},
		ParentTurnID: "s:turn:1",
	})

	require.NoError(t, env.GetWorkflowError())
	// The subagent's routing passes the parent turn id through to MemoryRetrieve,
	// which copies the parent turn's staged rows rather than re-querying
	// agent-brain. Staging is keyed on the subagent's own turn id.
	require.Equal(t, "s:turn:1", gotMemInput.ParentTurnID)
	require.Equal(t, "s:turn:1:sub:1", gotMemInput.OwnerID)
}

func TestRoutingWorkflow_ContinuationSkipsSkills(t *testing.T) {
	// A checkpoint / continuation turn (no PlanID) still retrieves fresh memory
	// + tools under its own turn id, but does not re-run skill discovery.
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var memIn types.MemoryRetrieveInput
	skillsCalled := false
	env.RegisterActivityWithOptions(
		func(_ context.Context, in types.MemoryRetrieveInput) (types.SubsystemResult, error) {
			memIn = in
			return types.SubsystemResult{Status: "ok", Count: 1}, nil
		},
		activity.RegisterOptions{Name: "MemoryRetrieve"},
	)
	mockSubsystem[types.ToolDiscoverInput](env, "ToolDiscover", types.SubsystemResult{Status: "ok", Count: 1}, nil)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ types.SkillDiscoverInput) (types.SubsystemResult, error) {
			skillsCalled = true
			return types.SubsystemResult{Status: "ok", Count: 2}, nil
		},
		activity.RegisterOptions{Name: "SkillDiscover"},
	)

	env.ExecuteWorkflow(RoutingWorkflow, RoutingWorkflowInput{
		TurnID: "s:turn:4", // continuation — no PlanID
		Task:   types.TaskRepresentation{Intent: "task", Complexity: "moderate", Confidence: 0.9},
	})

	require.NoError(t, env.GetWorkflowError())
	var result RoutingResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "s:turn:4", memIn.OwnerID)
	require.Equal(t, "ok", result.Memory.Status)
	require.False(t, skillsCalled, "continuation turn does not re-run skill discovery")
	require.Equal(t, "skipped", result.Skills.Status)
}

func TestRoutingWorkflow_SkillDiscoverRunsForFreshPlan(t *testing.T) {
	// A planning turn (PlanID set) runs SkillDiscover under the plan — its rows
	// feed the planning turn's prompt.
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	mockSubsystem[types.MemoryRetrieveInput](env, "MemoryRetrieve", types.SubsystemResult{Status: "ok", Count: 1}, nil)
	mockSubsystem[types.ToolDiscoverInput](env, "ToolDiscover", types.SubsystemResult{Status: "ok", Count: 1}, nil)
	var skillIn types.SkillDiscoverInput
	env.RegisterActivityWithOptions(
		func(_ context.Context, in types.SkillDiscoverInput) (types.SubsystemResult, error) {
			skillIn = in
			return types.SubsystemResult{Status: "ok", Count: 2}, nil
		},
		activity.RegisterOptions{Name: "SkillDiscover"},
	)

	env.ExecuteWorkflow(RoutingWorkflow, RoutingWorkflowInput{
		TurnID: "s:turn:1",
		PlanID: "s:turn:1",
		Task:   types.TaskRepresentation{Intent: "task", Complexity: "complex", Confidence: 0.9},
	})

	require.NoError(t, env.GetWorkflowError())
	var result RoutingResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "s:turn:1", skillIn.PlanID)
	require.Equal(t, "ok", result.Skills.Status)
}
