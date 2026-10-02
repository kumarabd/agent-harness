package workflow

import (
	"context"
	"testing"
	"time"

	"agent-harness/shared/types"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

type coordinatorInfraRecords struct {
	memoryWrites int
	turnSeqReads int
}

func mockCoordinatorInfra(env *testsuite.TestWorkflowEnvironment) *coordinatorInfraRecords {
	records := &coordinatorInfraRecords{}
	env.RegisterActivityWithOptions(func(context.Context, string) (int, error) { records.turnSeqReads++; return 0, nil }, activity.RegisterOptions{Name: "GetMaxTurnSeq"})
	env.RegisterActivityWithOptions(func(context.Context, types.InsertMessageInput) error { return nil }, activity.RegisterOptions{Name: "InsertMessage"})
	env.RegisterActivityWithOptions(func(context.Context, string) error { records.memoryWrites++; return nil }, activity.RegisterOptions{Name: "WriteMemory"})
	env.RegisterWorkflow(WriteMemoryWorkflow)
	return records
}

func sendUser(env *testsuite.TestWorkflowEnvironment, content string) {
	env.SignalWorkflow(NewMessageSignalName, types.SignalPayload{Message: types.Message{Role: "user", Content: content, SpeakerID: "user", ClientMsgID: content}})
}

func TestCoordinator_ContinuedTurnSequenceSkipsDatabaseRead(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	records := mockCoordinatorInfra(env)
	seq := 7
	env.ExecuteWorkflow(CoordinatorWorkflow, CoordinatorInput{
		SessionKey: "u:web", TurnSeq: &seq,
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
