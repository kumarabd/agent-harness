package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-harness/workflows/internal/types"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

type recordedSkillOutcome struct {
	stepID string
	state  types.ConversationState
	reason string
}

type coordinatorInfraRecords struct {
	outcomes     []recordedSkillOutcome
	memoryWrites int
	turnSeqReads int
	selectionErr error
}

func mockCoordinatorInfra(env *testsuite.TestWorkflowEnvironment) *coordinatorInfraRecords {
	records := &coordinatorInfraRecords{}
	env.RegisterActivityWithOptions(func(context.Context, string) (int, error) { records.turnSeqReads++; return 0, nil }, activity.RegisterOptions{Name: "GetMaxTurnSeq"})
	env.RegisterActivityWithOptions(func(context.Context, string) (string, error) { return "journaling", nil }, activity.RegisterOptions{Name: "LoadSessionMode"})
	env.RegisterActivityWithOptions(func(context.Context, types.InsertMessageInput) error { return nil }, activity.RegisterOptions{Name: "InsertMessage"})
	env.RegisterActivityWithOptions(func(context.Context, string) error { records.memoryWrites++; return nil }, activity.RegisterOptions{Name: "WriteMemory"})
	env.RegisterActivityWithOptions(func(_ context.Context, stepID string, state types.ConversationState, reason string) error {
		records.outcomes = append(records.outcomes, recordedSkillOutcome{stepID, state, reason})
		return nil
	}, activity.RegisterOptions{Name: "RecordSkillOutcome"})
	env.RegisterActivityWithOptions(func(_ context.Context, in types.UserSelectionInput) (string, error) {
		if records.selectionErr != nil {
			return "", records.selectionErr
		}
		if in.Message.Content == "/chat" {
			return "chat", nil
		}
		return "", nil
	}, activity.RegisterOptions{Name: "ApplyUserSelection"})
	env.RegisterWorkflow(WriteMemoryWorkflow)
	return records
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

func TestCoordinator_SkillCommandActivitiesUseTenantQueue(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{TaskQueue: "agent-loop"})
	mockCoordinatorInfra(env)
	queues := make(map[string]string)
	env.RegisterActivityWithOptions(func(ctx context.Context, _ types.SkillCommandInput) (types.SkillCommand, error) {
		queues["PrepareSkillCommand"] = activity.GetInfo(ctx).TaskQueue
		return types.SkillCommand{Action: "cancel"}, nil
	}, activity.RegisterOptions{Name: "PrepareSkillCommand"})
	env.RegisterActivityWithOptions(func(ctx context.Context, _ string, _ types.ConversationState) error {
		queues["RecordSkillCommand"] = activity.GetInfo(ctx).TaskQueue
		return nil
	}, activity.RegisterOptions{Name: "RecordSkillCommand"})
	env.RegisterDelayedCallback(func() { updateSkill(t, env, "cancel") }, time.Second)
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web", TenantSlug: "tenant-a"})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, map[string]string{
		"PrepareSkillCommand": "tenant-a-loop",
		"RecordSkillCommand":  "tenant-a-loop",
	}, queues)
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
	}, 10*time.Second)
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
	}, 5*time.Second)
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web", State: &types.ConversationState{Mode: "journaling", Skill: types.SkillState{Kind: "journaling", ContentID: "proposal", Revision: 1, Phase: "awaiting_confirmation"}}})
	require.NoError(t, env.GetWorkflowError())
	require.True(t, interrupted)
}

func TestCoordinator_TerminalSkillOutcomesCloseAndIdle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase string
	}{
		{"completed", "completed"},
		{"failed", "failed"},
		{"cancelled", "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			records := mockCoordinatorInfra(env)
			env.RegisterWorkflowWithOptions(func(workflow.Context, types.TurnInput) (types.TurnResult, error) {
				return types.TurnResult{}, nil
			}, workflow.RegisterOptions{Name: "TurnWorkflow"})
			env.RegisterActivityWithOptions(func(_ context.Context, in types.SkillCommandInput) (types.SkillCommand, error) {
				return types.SkillCommand{Action: in.ToolCallID}, nil
			}, activity.RegisterOptions{Name: "PrepareSkillCommand"})
			env.RegisterActivityWithOptions(func(context.Context, string, types.ConversationState) error {
				return nil
			}, activity.RegisterOptions{Name: "RecordSkillCommand"})
			env.RegisterWorkflowWithOptions(func(ctx workflow.Context, _ types.SkillStepInput) (types.SkillObservation, error) {
				switch tc.phase {
				case "completed":
					return types.SkillObservation{Complete: true}, workflow.Sleep(ctx, 3*time.Second)
				case "failed":
					return types.SkillObservation{}, temporal.NewNonRetryableApplicationError("step failed", "StepFailed", nil)
				default:
					return types.SkillObservation{}, workflow.Sleep(ctx, time.Hour)
				}
			}, workflow.RegisterOptions{Name: "SkillStepWorkflow"})
			env.RegisterDelayedCallback(func() { updateSkill(t, env, "confirm") }, time.Second)
			if tc.phase == "cancelled" {
				env.RegisterDelayedCallback(func() { updateSkill(t, env, "cancel") }, 2*time.Second)
			}
			env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web", State: &types.ConversationState{
				Mode: "journaling", Skill: types.SkillState{Kind: "journaling", ContentID: "proposal", Revision: 1, Phase: "awaiting_confirmation"},
			}})
			require.NoError(t, env.GetWorkflowError())
			require.Len(t, records.outcomes, 1)
			require.Equal(t, "confirm", records.outcomes[0].stepID)
			require.Equal(t, tc.phase, records.outcomes[0].state.Skill.Phase)
			require.Equal(t, "proposal", records.outcomes[0].state.Skill.ContentID)
			require.Equal(t, 1, records.memoryWrites)
		})
	}
}

func TestCoordinator_SelectionFailureIsReportedToChat(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	records := mockCoordinatorInfra(env)
	records.selectionErr = errors.New("selection store unavailable")
	var messages []string
	env.RegisterWorkflowWithOptions(func(_ workflow.Context, in types.TurnInput) (types.TurnResult, error) {
		messages = append(messages, in.InitialMessage.Content)
		return types.TurnResult{}, nil
	}, workflow.RegisterOptions{Name: "TurnWorkflow"})
	env.RegisterDelayedCallback(func() { sendUser(env, "/skill journaling") }, time.Second)
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web"})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, "/skill journaling", messages[0])
	require.Contains(t, messages[len(messages)-1], "selection could not be confirmed")
}

func TestCoordinator_ContinuedTurnSequenceSkipsDatabaseRead(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	records := mockCoordinatorInfra(env)
	seq := 7
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{
		SessionKey: "u:web", State: &types.ConversationState{Mode: "chat"}, TurnSeq: &seq,
	})
	require.NoError(t, env.GetWorkflowError())
	require.Zero(t, records.turnSeqReads)
}

func TestCoordinator_RequeuesEveryUnprocessedTurnMessage(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockCoordinatorInfra(env)
	var messages []string
	env.RegisterWorkflowWithOptions(func(_ workflow.Context, in types.TurnInput) (types.TurnResult, error) {
		messages = append(messages, in.InitialMessage.Content)
		if len(messages) == 1 {
			return types.TurnResult{UnprocessedMessages: []types.SignalPayload{
				{Message: types.Message{Role: "user", Content: "second"}},
				{Message: types.Message{Role: "user", Content: "third"}},
			}}, nil
		}
		return types.TurnResult{}, nil
	}, workflow.RegisterOptions{Name: "TurnWorkflow"})
	env.RegisterDelayedCallback(func() { sendUser(env, "first") }, time.Second)
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web"})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{"first", "second", "third"}, messages)
}

func TestCoordinator_WakeFoldsIntoActiveChat(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	mockCoordinatorInfra(env)
	var folded string
	env.RegisterWorkflowWithOptions(func(ctx workflow.Context, _ types.TurnInput) (types.TurnResult, error) {
		var message types.SignalPayload
		workflow.GetSignalChannel(ctx, NewMessageSignalName).Receive(ctx, &message)
		folded = message.Message.Content
		return types.TurnResult{}, nil
	}, workflow.RegisterOptions{Name: "TurnWorkflow"})
	env.RegisterDelayedCallback(func() { sendUser(env, "question") }, time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeSignalName, types.WakePayload{Objective: "Check the service", Why: "The user asked me to monitor it"})
	}, 2*time.Second)
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{SessionKey: "u:web"})
	require.NoError(t, env.GetWorkflowError())
	require.Contains(t, folded, "Proactive note")
	require.Contains(t, proactiveSeedText(types.WakePayload{Objective: "Check the service"}), "end the turn without responding")
}
