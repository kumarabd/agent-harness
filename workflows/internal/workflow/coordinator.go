package workflow

import (
	"fmt"
	"time"

	"agent-harness/workflows/internal/types"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const IdleTTL = 30 * time.Second

type CoordinatorInput struct {
	SessionKey       string `json:"session_key"`
	ParentSessionKey string `json:"parent_session_key,omitempty"`
	ConnectionID     string `json:"connection_id,omitempty"`
	TenantSlug       string `json:"tenant_slug,omitempty"`
	// Continue-as-new carries live skill state. An unfinished skill never idles out.
	State *types.ConversationState `json:"state,omitempty"`
}

type queuedChatMessage struct {
	payload     types.SignalPayload
	initiatedBy string
}

// CoordinatorWorkflow owns user selection and skill lifecycle. Every unsolicited
// message goes to ordinary chat; bounded skill steps accept explicit commands.
// Only the one active chat turn may deliver messages to the user.
func CoordinatorWorkflow(ctx workflow.Context, input CoordinatorInput) error {
	ctx = WithTenantTaskQueue(ctx, input.TenantSlug)
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 3}}
	actx := workflow.WithActivityOptions(ctx, ao)
	logger := workflow.GetLogger(ctx)
	if input.ParentSessionKey != "" {
		if err := workflow.ExecuteActivity(actx, "SeedChildSessionContext", input.ParentSessionKey, input.SessionKey).Get(ctx, nil); err != nil {
			return err
		}
		input.ParentSessionKey = ""
	}
	state := types.ConversationState{Mode: "chat"}
	if input.State != nil {
		state = *input.State
	} else {
		if err := workflow.ExecuteActivity(actx, "LoadSessionMode", input.SessionKey).Get(ctx, &state.Mode); err != nil {
			return err
		}
	}
	var turnSeq int
	if err := workflow.ExecuteActivity(actx, "GetMaxTurnSeq", input.SessionKey).Get(ctx, &turnSeq); err != nil {
		return err
	}
	if err := workflow.SetQueryHandler(ctx, conversationStateQuery, func() (types.ConversationState, error) { return state, nil }); err != nil {
		return err
	}

	var chat workflow.ChildWorkflowFuture
	var chatID string
	var stepCancel workflow.CancelFunc
	var step workflow.ChildWorkflowFuture
	commands := workflow.NewMutex(ctx)
	changed := workflow.NewBufferedChannel(ctx, 1)
	var pending []queuedChatMessage
	var notices []queuedChatMessage
	if err := workflow.SetUpdateHandler(ctx, skillCommandUpdate, func(uctx workflow.Context, toolCallID string) (types.ConversationState, error) {
		defer func() {
			notify := workflow.NewSelector(ctx)
			notify.AddSend(changed, true, func() {})
			notify.AddDefault(func() {})
			notify.Select(ctx)
		}()
		if err := commands.Lock(uctx); err != nil {
			return state, err
		}
		defer commands.Unlock()
		var command types.SkillCommand
		in := types.SkillCommandInput{ToolCallID: toolCallID, SessionKey: input.SessionKey, State: state}
		if err := workflow.ExecuteActivity(workflow.WithActivityOptions(uctx, ao), "PrepareSkillCommand", in).Get(uctx, &command); err != nil {
			return state, err
		}
		if command.AlreadyApplied {
			return state, nil
		}
		if state != in.State {
			return state, temporal.NewNonRetryableApplicationError("skill state changed during validation; reread snapshot", "SkillStale", nil)
		}
		if step != nil {
			if command.Action != "cancel" {
				return state, temporal.NewNonRetryableApplicationError("skill step is running; cancel it before amending", "SkillBusy", nil)
			}
			stepCancel()
			state.Skill.Phase = "cancelling"
		} else {
			switch command.Action {
			case "submit", "amend":
				state.Skill = types.SkillState{Kind: state.Mode, ContentID: command.ContentID, Revision: state.Skill.Revision + 1, Phase: "awaiting_confirmation"}
			case "cancel":
				if state.Skill.Phase == "completed" {
					return state, temporal.NewNonRetryableApplicationError("completed work cannot be cancelled", "SkillCompleted", nil)
				}
				state.Skill.Phase = "cancelled"
			case "confirm":
				state.Skill.Phase = "running"
				state.Skill.StepID = toolCallID
				cctx, cancel := workflow.WithCancel(ctx)
				stepCancel = cancel
				cctx = workflow.WithChildOptions(cctx, workflow.ChildWorkflowOptions{WorkflowID: toolCallID + ":skill", ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL, WorkflowRunTimeout: 10 * time.Minute, WaitForCancellation: true})
				step = workflow.ExecuteChildWorkflow(cctx, SkillStepWorkflow, types.SkillStepInput{StepID: toolCallID, ContentID: state.Skill.ContentID, Kind: state.Skill.Kind, TenantSlug: input.TenantSlug, SessionKey: input.SessionKey})
				if err := step.GetChildWorkflowExecution().Get(uctx, nil); err != nil {
					step, stepCancel = nil, nil
					state.Skill.Phase = "failed"
					return state, err
				}
			default:
				return state, temporal.NewNonRetryableApplicationError("invalid skill action", "SkillCommand", nil)
			}
		}
		if err := workflow.ExecuteActivity(workflow.WithActivityOptions(uctx, ao), "RecordSkillCommand", toolCallID, state).Get(uctx, nil); err != nil {
			return state, err
		}
		return state, nil
	}); err != nil {
		return err
	}

	newMessage := workflow.GetSignalChannel(ctx, NewMessageSignalName)
	wake := workflow.GetSignalChannel(ctx, WakeSignalName)
	cancelSignal := workflow.GetSignalChannel(ctx, CancelSignalName)
	keepAlive := workflow.GetSignalChannel(ctx, KeepAliveSignalName)
	for {
		// Skill completion must not interrupt a user's active confirmation or
		// delivery. Only ordinary chat delivers the queued notification.
		if chat == nil && len(pending) == 0 && len(notices) > 0 {
			pending, notices = notices, nil
		}
		if len(pending) > 0 {
			queued := pending[0]
			payload := queued.payload
			pending = pending[1:]
			// Selection is applied only by this authenticated user-input path. The
			// activity recognizes registered explicit commands; models cannot set it.
			if payload.Message.SpeakerID != "" {
				if err := commands.Lock(ctx); err != nil {
					return err
				}
				var mode string
				err := workflow.ExecuteActivity(actx, "ApplyUserSelection", types.UserSelectionInput{SessionKey: input.SessionKey, Message: payload.Message}).Get(ctx, &mode)
				if err == nil && mode != "" && mode != state.Mode {
					state.Mode = mode
					state.Skill.Revision++
					if stepCancel != nil {
						stepCancel()
						state.Skill.Phase = "cancelling"
					} else {
						state.Skill = types.SkillState{Revision: state.Skill.Revision}
					}
				} else if err != nil {
					logger.Error("user selection failed", "error", err)
				}
				commands.Unlock()
			}
			if chat != nil {
				if err := workflow.SignalExternalWorkflow(ctx, chatID, "", NewMessageSignalName, payload).Get(ctx, nil); err != nil {
					// Preserve a message that raced child completion and consume completion
					// before trying to start the next chat turn.
					pending = append([]queuedChatMessage{queued}, pending...)
				} else {
					continue
				}
			} else {
				turnSeq++
				var err error
				chat, chatID, err = startTurn(ctx, input.TenantSlug, input.SessionKey, input.ConnectionID, turnSeq, payload.Message, queued.initiatedBy)
				if err != nil {
					return err
				}
				continue
			}
		}
		// Closing while updates or signals are queued would discard durable input.
		quiescent := chat == nil && step == nil && len(pending) == 0 && len(notices) == 0 && workflow.AllHandlersFinished(ctx) &&
			newMessage.Len() == 0 && wake.Len() == 0 && cancelSignal.Len() == 0 && keepAlive.Len() == 0 && changed.Len() == 0
		if quiescent && workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			input.State = &state
			return workflow.NewContinueAsNewError(ctx, CoordinatorWorkflow, input)
		}
		tctx, cancelTimer := workflow.WithCancel(ctx)
		sel := workflow.NewSelector(ctx)
		idle := false
		sel.AddReceive(changed, func(c workflow.ReceiveChannel, _ bool) { var v bool; c.Receive(ctx, &v) })
		sel.AddReceive(newMessage, func(c workflow.ReceiveChannel, _ bool) {
			var p types.SignalPayload
			c.Receive(ctx, &p)
			pending = append(pending, queuedChatMessage{payload: p, initiatedBy: "user"})
		})
		sel.AddReceive(wake, func(c workflow.ReceiveChannel, _ bool) {
			var p types.WakePayload
			c.Receive(ctx, &p)
			pending = append(pending, queuedChatMessage{payload: types.SignalPayload{Message: types.Message{Role: "user", Content: proactiveSeedText(p)}}, initiatedBy: "intn:" + p.IntentionID})
		})
		sel.AddReceive(keepAlive, func(c workflow.ReceiveChannel, _ bool) { var v struct{}; c.Receive(ctx, &v) })
		sel.AddReceive(cancelSignal, func(c workflow.ReceiveChannel, _ bool) {
			var v struct{}
			c.Receive(ctx, &v)
			if stepCancel != nil {
				stepCancel()
				state.Skill.Phase = "cancelling"
			}
			if chat != nil {
				_ = workflow.SignalExternalWorkflow(ctx, chatID, "", CancelSignalName, v).Get(ctx, nil)
			}
		})
		if chat != nil {
			sel.AddFuture(chat, func(f workflow.Future) {
				var result types.TurnResult
				if err := f.Get(ctx, &result); err != nil {
					deliverWedgedFallback(ctx, input.SessionKey, input.ConnectionID, chatID)
				}
				chat, chatID = nil, ""
				if result.InterruptedDuringDelivery != nil {
					pending = append(pending, queuedChatMessage{payload: *result.InterruptedDuringDelivery, initiatedBy: "user"})
				}
			})
		}
		if step != nil {
			sel.AddFuture(step, func(f workflow.Future) {
				var out types.SkillObservation
				err := f.Get(ctx, &out)
				state.Skill.Phase = "failed"
				if err == nil && out.Complete {
					state.Skill.Phase = "completed"
				}
				if temporal.IsCanceledError(err) {
					state.Skill.Phase = "cancelled"
				}
				step, stepCancel = nil, nil
				notice := fmt.Sprintf("[Skill step finished: kind=%s content_id=%s step_id=%s phase=%s. Report this outcome without conflating it with a newer proposal. Failed/cancelled work may have unverified external effects; do not claim success or automatic rollback. This is a system notification, not user input or authorization.]", state.Skill.Kind, state.Skill.ContentID, state.Skill.StepID, state.Skill.Phase)
				notices = append(notices, queuedChatMessage{payload: types.SignalPayload{Message: types.Message{Role: "user", Content: notice}}, initiatedBy: "system"})
			})
		}
		// Keep the latest proposal/outcome in durable workflow state until the
		// user clears it by selecting another mode. Continue-as-new bounds history.
		if quiescent && state.Skill.ContentID == "" {
			sel.AddFuture(workflow.NewTimer(tctx, IdleTTL), func(workflow.Future) { idle = true })
		}
		sel.Select(ctx)
		cancelTimer()
		if idle && len(pending) == 0 && len(notices) == 0 && workflow.AllHandlersFinished(ctx) && step == nil && state.Skill.ContentID == "" &&
			newMessage.Len() == 0 && wake.Len() == 0 && changed.Len() == 0 {
			cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{WorkflowID: input.SessionKey + ":write-memory:" + workflow.GetInfo(ctx).WorkflowExecution.RunID, ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON})
			_ = workflow.ExecuteChildWorkflow(cctx, WriteMemoryWorkflow, input.SessionKey, input.TenantSlug).GetChildWorkflowExecution().Get(ctx, nil)
			return nil
		}
	}
}

func proactiveSeedText(w types.WakePayload) string {
	return "[Proactive check — the user did not send this message]\n" + w.Objective + "\n" + w.Why + "\nDecide whether anything is worth reporting."
}
