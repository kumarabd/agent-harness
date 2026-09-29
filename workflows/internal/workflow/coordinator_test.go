package workflow

import (
	"context"
	"testing"
	"time"

	"agent-harness/workflows/internal/types"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func mockCoordinatorInfra(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivityWithOptions(func(context.Context, string) (int, error) { return 0, nil }, activity.RegisterOptions{Name: "GetMaxTurnSeq"})
	env.RegisterActivityWithOptions(func(context.Context, string) (string, error) { return "journaling", nil }, activity.RegisterOptions{Name: "LoadSessionMode"})
	env.RegisterActivityWithOptions(func(context.Context, types.InsertMessageInput) error { return nil }, activity.RegisterOptions{Name: "InsertMessage"})
	env.RegisterActivityWithOptions(func(context.Context, string) error { return nil }, activity.RegisterOptions{Name: "WriteMemory"})
	env.RegisterActivityWithOptions(func(_ context.Context, in types.UserSelectionInput) (string, error) {
		if in.Message.Content == "/chat" {
			return "chat", nil
		}
		return "", nil
	}, activity.RegisterOptions{Name: "ApplyUserSelection"})
	env.RegisterWorkflow(WriteMemoryWorkflow)
}

func updateSkill(t *testing.T, env *testsuite.TestWorkflowEnvironment, id string) {
	env.UpdateWorkflow(skillCommandUpdate, id, &testsuite.TestUpdateCallback{
		OnReject:   func(err error) { require.NoError(t, err) },
		OnAccept:   func() {},
		OnComplete: func(_ interface{}, err error) { require.NoError(t, err) },
	}, id)
}

func sendUser(env *testsuite.TestWorkflowEnvironment, content string) {
	env.SignalWorkflow(NewMessageSignalName, types.SignalPayload{Message: types.Message{Role: "user", Content: content, SpeakerID: "user", ClientMsgID: content}})
}

func TestCoordinator_SelectedSkillAlwaysDispatchesChat(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockCoordinatorInfra(env)
	var messages []string
	env.RegisterWorkflowWithOptions(func(_ workflow.Context, in types.TurnInput) (types.TurnResult, error) {
		messages = append(messages, in.InitialMessage.Content)
		return types.TurnResult{StopReason: "done"}, nil
	}, workflow.RegisterOptions{Name: "TurnWorkflow"})
	env.RegisterDelayedCallback(func() { sendUser(env, "How is my journal?") }, time.Second)
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web"})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{"How is my journal?"}, messages)
}

func TestCoordinator_ChatDuringSkillDoesNotInterruptChild(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockCoordinatorInfra(env)
	var chats []string
	env.RegisterWorkflowWithOptions(func(_ workflow.Context, in types.TurnInput) (types.TurnResult, error) {
		chats = append(chats, in.InitialMessage.Content)
		return types.TurnResult{StopReason: "done"}, nil
	}, workflow.RegisterOptions{Name: "TurnWorkflow"})
	env.RegisterActivityWithOptions(func(_ context.Context, in types.SkillCommandInput) (types.SkillCommand, error) {
		return types.SkillCommand{Action: in.ToolCallID, ContentID: "proposal"}, nil
	}, activity.RegisterOptions{Name: "PrepareSkillCommand"})
	env.RegisterActivityWithOptions(func(context.Context, string, types.ConversationState) error { return nil }, activity.RegisterOptions{Name: "RecordSkillCommand"})
	childCompleted := false
	env.RegisterWorkflowWithOptions(func(ctx workflow.Context, _ types.SkillStepInput) (types.SkillObservation, error) {
		if err := workflow.Sleep(ctx, 5*time.Second); err != nil {
			return types.SkillObservation{}, err
		}
		require.Zero(t, workflow.GetSignalChannel(ctx, NewMessageSignalName).Len())
		childCompleted = true
		return types.SkillObservation{Complete: true}, nil
	}, workflow.RegisterOptions{Name: "SkillStepWorkflow"})
	env.RegisterDelayedCallback(func() { updateSkill(t, env, "submit") }, time.Second)
	env.RegisterDelayedCallback(func() { updateSkill(t, env, "confirm") }, 2*time.Second)
	env.RegisterDelayedCallback(func() { sendUser(env, "what is happening?") }, 3*time.Second)
	env.RegisterDelayedCallback(func() {
		value, err := env.QueryWorkflow(conversationStateQuery)
		require.NoError(t, err)
		var state types.ConversationState
		require.NoError(t, value.Get(&state))
		require.Equal(t, "running", state.Skill.Phase)
		require.Equal(t, "journaling", state.Mode)
	}, 4*time.Second)
	env.RegisterDelayedCallback(func() {
		value, err := env.QueryWorkflow(conversationStateQuery)
		require.NoError(t, err)
		var state types.ConversationState
		require.NoError(t, value.Get(&state))
		require.Equal(t, "completed", state.Skill.Phase)
		require.Equal(t, "confirm", state.Skill.StepID)
		sendUser(env, "/chat")
	}, 2*IdleTTL)
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web"})
	require.NoError(t, env.GetWorkflowError())
	require.True(t, childCompleted)
	require.Len(t, chats, 3)
	require.Equal(t, "what is happening?", chats[0])
}

func TestCoordinator_PendingProposalSurvivesIdleAndDeactivation(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockCoordinatorInfra(env)
	env.RegisterWorkflowWithOptions(func(workflow.Context, types.TurnInput) (types.TurnResult, error) { return types.TurnResult{}, nil }, workflow.RegisterOptions{Name: "TurnWorkflow"})
	env.RegisterDelayedCallback(func() {
		v, err := env.QueryWorkflow(conversationStateQuery)
		require.NoError(t, err)
		var state types.ConversationState
		require.NoError(t, v.Get(&state))
		require.Equal(t, "awaiting_confirmation", state.Skill.Phase)
		require.Equal(t, 3, state.Skill.Revision)
		sendUser(env, "/chat")
	}, 2*IdleTTL)
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web", State: &types.ConversationState{Mode: "journaling", Skill: types.SkillState{Kind: "journaling", ContentID: "proposal", Revision: 3, Phase: "awaiting_confirmation"}}})
	require.NoError(t, env.GetWorkflowError())
}

func TestCoordinator_CompletionNoticeWaitsForChat(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockCoordinatorInfra(env)
	var chats []string
	chatFinished := false
	env.RegisterWorkflowWithOptions(func(ctx workflow.Context, in types.TurnInput) (types.TurnResult, error) {
		chats = append(chats, in.InitialMessage.Content)
		if in.InitialMessage.Content == "question" {
			require.NoError(t, workflow.Sleep(ctx, 10*time.Second))
			require.Zero(t, workflow.GetSignalChannel(ctx, NewMessageSignalName).Len(), "completion notice must not interrupt a live chat")
			chatFinished = true
		} else {
			require.True(t, chatFinished)
		}
		return types.TurnResult{}, nil
	}, workflow.RegisterOptions{Name: "TurnWorkflow"})
	env.RegisterActivityWithOptions(func(context.Context, types.SkillCommandInput) (types.SkillCommand, error) {
		return types.SkillCommand{Action: "confirm"}, nil
	}, activity.RegisterOptions{Name: "PrepareSkillCommand"})
	env.RegisterActivityWithOptions(func(context.Context, string, types.ConversationState) error { return nil }, activity.RegisterOptions{Name: "RecordSkillCommand"})
	env.RegisterWorkflowWithOptions(func(ctx workflow.Context, _ types.SkillStepInput) (types.SkillObservation, error) {
		return types.SkillObservation{Complete: true}, workflow.Sleep(ctx, 3*time.Second)
	}, workflow.RegisterOptions{Name: "SkillStepWorkflow"})
	env.RegisterDelayedCallback(func() { updateSkill(t, env, "confirm") }, time.Second)
	env.RegisterDelayedCallback(func() { sendUser(env, "question") }, 2*time.Second)
	env.RegisterDelayedCallback(func() { sendUser(env, "/chat") }, 20*time.Second)
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web", State: &types.ConversationState{Mode: "journaling", Skill: types.SkillState{Kind: "journaling", ContentID: "proposal", Revision: 1, Phase: "awaiting_confirmation"}}})
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, chats, 3)
	require.Contains(t, chats[1], "Skill step finished")
}

func TestCoordinator_ExplicitCancelStopsSkillButKeepsSelection(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockCoordinatorInfra(env)
	env.RegisterWorkflowWithOptions(func(workflow.Context, types.TurnInput) (types.TurnResult, error) { return types.TurnResult{}, nil }, workflow.RegisterOptions{Name: "TurnWorkflow"})
	env.RegisterActivityWithOptions(func(_ context.Context, in types.SkillCommandInput) (types.SkillCommand, error) {
		return types.SkillCommand{Action: in.ToolCallID}, nil
	}, activity.RegisterOptions{Name: "PrepareSkillCommand"})
	env.RegisterActivityWithOptions(func(context.Context, string, types.ConversationState) error { return nil }, activity.RegisterOptions{Name: "RecordSkillCommand"})
	interrupted := false
	env.RegisterWorkflowWithOptions(func(ctx workflow.Context, _ types.SkillStepInput) (types.SkillObservation, error) {
		err := workflow.Sleep(ctx, time.Hour)
		interrupted = err != nil
		return types.SkillObservation{}, err
	}, workflow.RegisterOptions{Name: "SkillStepWorkflow"})
	env.RegisterDelayedCallback(func() { updateSkill(t, env, "confirm") }, time.Second)
	env.RegisterDelayedCallback(func() { updateSkill(t, env, "cancel") }, 2*time.Second)
	env.RegisterDelayedCallback(func() {
		v, err := env.QueryWorkflow(conversationStateQuery)
		require.NoError(t, err)
		var state types.ConversationState
		require.NoError(t, v.Get(&state))
		require.Equal(t, "cancelled", state.Skill.Phase)
		require.Equal(t, "journaling", state.Mode)
		sendUser(env, "/chat")
	}, 2*IdleTTL)
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web", State: &types.ConversationState{Mode: "journaling", Skill: types.SkillState{Kind: "journaling", ContentID: "proposal", Revision: 1, Phase: "awaiting_confirmation"}}})
	require.NoError(t, env.GetWorkflowError())
	require.True(t, interrupted)
}
