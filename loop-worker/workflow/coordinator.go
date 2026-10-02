package workflow

import (
	"time"

	"agent-harness/shared/types"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"
)

const IdleTTL = 30 * time.Second

type CoordinatorInput struct {
	SessionKey       string `json:"session_key"`
	ParentSessionKey string `json:"parent_session_key,omitempty"`
	ConnectionID     string `json:"connection_id,omitempty"`
	TenantSlug       string `json:"tenant_slug,omitempty"`
	// Continue-as-new carries the turn sequence counter across restarts.
	TurnSeq *int `json:"turn_seq,omitempty"`
}

type queuedChatMessage struct {
	payload     types.SignalPayload
	initiatedBy string
}

// CoordinatorWorkflow is the long-lived, nearly-stateless control-plane
// workflow: workflow ID = session key. It holds only a pointer to the
// currently-running chat turn (if any), a turn-sequence counter, and the
// queue of messages not yet forwarded into one — no conversation content.
// Every message starts or forwards into ordinary TurnWorkflow; there is no
// other dispatch target.
func CoordinatorWorkflow(ctx workflow.Context, input CoordinatorInput) error {
	ctx = WithTenantTaskQueue(ctx, input.TenantSlug)
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)
	logger := workflow.GetLogger(ctx)
	if input.ParentSessionKey != "" {
		if err := workflow.ExecuteActivity(actx, "SeedChildSessionContext", input.ParentSessionKey, input.SessionKey).Get(ctx, nil); err != nil {
			return err
		}
		input.ParentSessionKey = ""
	}
	turnSeq := 0
	if input.TurnSeq != nil {
		turnSeq = *input.TurnSeq
	} else {
		if err := workflow.ExecuteActivity(actx, "GetMaxTurnSeq", input.SessionKey).Get(ctx, &turnSeq); err != nil {
			return err
		}
	}

	var chat workflow.ChildWorkflowFuture
	var chatID string
	var pending []queuedChatMessage

	newMessage := workflow.GetSignalChannel(ctx, NewMessageSignalName)
	wake := workflow.GetSignalChannel(ctx, WakeSignalName)
	cancelSignal := workflow.GetSignalChannel(ctx, CancelSignalName)
	keepAlive := workflow.GetSignalChannel(ctx, KeepAliveSignalName)
	for {
		if len(pending) > 0 {
			queued := pending[0]
			payload := queued.payload
			pending = pending[1:]
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
		// Closing while signals are queued would discard durable input.
		quiescent := chat == nil && len(pending) == 0 &&
			newMessage.Len() == 0 && wake.Len() == 0 && cancelSignal.Len() == 0 && keepAlive.Len() == 0
		if quiescent && workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			input.TurnSeq = &turnSeq
			return workflow.NewContinueAsNewError(ctx, CoordinatorWorkflow, input)
		}
		tctx, cancelTimer := workflow.WithCancel(ctx)
		sel := workflow.NewSelector(ctx)
		idle := false
		sel.AddReceive(newMessage, func(c workflow.ReceiveChannel, _ bool) {
			var p types.SignalPayload
			c.Receive(ctx, &p)
			pending = append(pending, queuedChatMessage{payload: p, initiatedBy: "user"})
		})
		sel.AddReceive(wake, func(c workflow.ReceiveChannel, _ bool) {
			var p types.WakePayload
			c.Receive(ctx, &p)
			if chat != nil {
				fold := types.SignalPayload{Message: types.Message{Role: "user", Content: proactiveFoldText(p)}}
				if err := workflow.SignalExternalWorkflow(ctx, chatID, "", NewMessageSignalName, fold).Get(ctx, nil); err == nil {
					return
				} else {
					logger.Error("failed to fold wake into active turn", "turn_id", chatID, "intention_id", p.IntentionID, "error", err)
				}
			}
			pending = append(pending, queuedChatMessage{payload: types.SignalPayload{Message: types.Message{Role: "user", Content: proactiveSeedText(p)}}, initiatedBy: "intn:" + p.IntentionID})
		})
		sel.AddReceive(keepAlive, func(c workflow.ReceiveChannel, _ bool) { var v struct{}; c.Receive(ctx, &v) })
		sel.AddReceive(cancelSignal, func(c workflow.ReceiveChannel, _ bool) {
			var v struct{}
			c.Receive(ctx, &v)
			if chat != nil {
				if err := workflow.SignalExternalWorkflow(ctx, chatID, "", CancelSignalName, v).Get(ctx, nil); err != nil {
					logger.Error("failed to forward cancel to active turn", "turn_id", chatID, "error", err)
				}
			}
		})
		if chat != nil {
			sel.AddFuture(chat, func(f workflow.Future) {
				var result types.TurnResult
				if err := f.Get(ctx, &result); err != nil {
					deliverWedgedFallback(ctx, input.SessionKey, input.ConnectionID, chatID)
				}
				chat, chatID = nil, ""
				if len(result.UnprocessedMessages) > 0 {
					for _, message := range result.UnprocessedMessages {
						pending = append(pending, queuedChatMessage{payload: message, initiatedBy: "user"})
					}
				} else if result.InterruptedDuringDelivery != nil {
					pending = append(pending, queuedChatMessage{payload: *result.InterruptedDuringDelivery, initiatedBy: "user"})
				}
			})
		}
		if quiescent {
			sel.AddFuture(workflow.NewTimer(tctx, IdleTTL), func(workflow.Future) { idle = true })
		}
		sel.Select(ctx)
		cancelTimer()
		if idle && len(pending) == 0 && chat == nil &&
			newMessage.Len() == 0 && wake.Len() == 0 && cancelSignal.Len() == 0 && keepAlive.Len() == 0 {
			cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{WorkflowID: input.SessionKey + ":write-memory:" + workflow.GetInfo(ctx).WorkflowExecution.RunID, ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON})
			_ = workflow.ExecuteChildWorkflow(cctx, WriteMemoryWorkflow, input.SessionKey, input.TenantSlug).GetChildWorkflowExecution().Get(ctx, nil)
			return nil
		}
	}
}

func proactiveSeedText(w types.WakePayload) string {
	s := "[Proactive check — the user did not send this message]\n\n" + w.Objective
	if w.Why != "" {
		s += "\n\n" + w.Why
	}
	return s + "\n\nDecide whether and how to surface this to the user now. Check what you need to first. If nothing is worth saying right now, end the turn without responding."
}

func proactiveFoldText(w types.WakePayload) string {
	s := "[Proactive note — surface this to the user if and when it fits the conversation]\n\n" + w.Objective
	if w.Why != "" {
		s += "\n\n" + w.Why
	}
	return s
}
