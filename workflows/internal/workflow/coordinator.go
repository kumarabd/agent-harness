package workflow

import (
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/ids"
	"agent-harness/workflows/internal/types"
)

// idleTTL is deliberately short in this local-dev slice so the coordinator's
// self-termination behavior is easy to observe without a long wait. The real
// design's resolved default is 5-15 minutes (components/session-coordinator.md);
// 30s here is a dev-loop convenience, not a design change.
const idleTTL = 30 * time.Second

// CoordinatorInput starts (or is ignored by, if the workflow already exists —
// SignalWithStart handles that) a Session Coordinator.
type CoordinatorInput struct {
	SessionKey string `json:"session_key"`
	// ParentSessionKey — gateway.md's "Resolved: Multi-Session Channels".
	// Set ONLY by the Gateway's own genuine genesis check (its sessions-table
	// INSERT's RowsAffected — never re-derived here), so this workflow can
	// trust its mere presence as proof this is genuinely this session_key's
	// first-ever execution, safe to act on unconditionally rather than
	// needing its own genesis check. A later restart of this SAME session
	// (idleTTL, SignalWithStart's ALLOW_DUPLICATE reuse) never carries this
	// — the Gateway only sets it once, at true genesis — so seeding never
	// re-fires on an ordinary restart.
	ParentSessionKey string `json:"parent_session_key,omitempty"`
	// ConnectionID — types.TurnInput's own doc comment has the full detail.
	// Unlike ParentSessionKey, this is NOT gated to true genesis: the
	// Gateway sets it on every SignalWithStart call, so it's re-supplied
	// correctly both on a session's real first message and on an ordinary
	// idle-timeout coordinator restart (Temporal only consumes these
	// start-args on whichever call actually starts a fresh execution,
	// whatever the reason).
	ConnectionID string `json:"connection_id,omitempty"`
}

// CoordinatorWorkflow is the long-lived, nearly-stateless control-plane
// workflow: workflow ID = session key. It holds only a pointer to the
// currently-running Turn Workflow (if any) and a turn-sequence counter — no
// conversation content (components/session-coordinator.md).
func CoordinatorWorkflow(ctx workflow.Context, input CoordinatorInput) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("coordinator started", "session_key", input.SessionKey)

	// gateway.md's "Resolved: Multi-Session Channels" — LCM-copy genesis
	// context injection. Best-effort, same fire-and-forget tolerance as
	// Persist/Deliver's own end-of-turn bookkeeping calls elsewhere in this
	// codebase: a failed seed means this child session just starts with no
	// injected parent context (the pre-this-feature behavior), not a hard
	// failure of the whole session.
	if input.ParentSessionKey != "" {
		sao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
		sctx := workflow.WithActivityOptions(ctx, sao)
		if err := workflow.ExecuteActivity(sctx, "SeedChildSessionContext", input.ParentSessionKey, input.SessionKey).Get(sctx, nil); err != nil {
			logger.Error("failed to seed child session context", "session_key", input.SessionKey, "parent_session_key", input.ParentSessionKey, "error", err)
		}
	}

	// Seed turnSeq from the real Postgres-backed maximum rather than always
	// starting at 0 — a fresh CoordinatorWorkflow execution (workflow ID =
	// session key, so this runs every time a prior execution idled out and a
	// later message starts a new one) otherwise reminted turn:1 on every
	// restart, colliding with turns the session already had. The Coordinator
	// can't query Postgres directly (would break the workflow determinism
	// boundary), so GetMaxTurnSeq does that lookup on its behalf, mirroring
	// cmd/starter/main.go's own client-side prediction of this same value.
	var maxTurnSeq int
	gao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	gctx := workflow.WithActivityOptions(ctx, gao)
	if err := workflow.ExecuteActivity(gctx, "GetMaxTurnSeq", input.SessionKey).Get(gctx, &maxTurnSeq); err != nil {
		logger.Error("failed to look up max turn seq, starting from 0", "session_key", input.SessionKey, "error", err)
		maxTurnSeq = 0
	}
	turnSeq := maxTurnSeq
	var currentTurnHandle workflow.ChildWorkflowFuture
	var currentTurnID string
	turnActive := false

	signalChan := workflow.GetSignalChannel(ctx, NewMessageSignalName)
	var pendingSignal *types.SignalPayload
	haveSignal := false

	for {
		idleTimerCtx, cancelIdleTimer := workflow.WithCancel(ctx)
		idleTimer := workflow.NewTimer(idleTimerCtx, idleTTL)

		sel := workflow.NewSelector(ctx)
		sel.AddReceive(signalChan, func(c workflow.ReceiveChannel, more bool) {
			var payload types.SignalPayload
			c.Receive(ctx, &payload)
			pendingSignal = &payload
			haveSignal = true
		})
		if turnActive {
			sel.AddFuture(currentTurnHandle, func(f workflow.Future) {
				var result types.TurnResult
				err := f.Get(ctx, &result)
				if err != nil {
					logger.Error("turn workflow ended with error", "turn_id", currentTurnID, "error", err)
				} else {
					logger.Info("turn workflow completed", "turn_id", currentTurnID, "stop_reason", result.StopReason)
				}
				turnActive = false
				currentTurnID = ""
			})
		} else {
			sel.AddFuture(idleTimer, func(f workflow.Future) {
				// no-op callback; presence in the selector is what lets the
				// idle path win the select below
			})
		}
		sel.Select(ctx)
		cancelIdleTimer()

		if !turnActive && !haveSignal {
			// Only the idle timer could have fired for us to get here with no
			// signal and no active turn — self-terminate per the resolved TTL
			// design (components/session-coordinator.md). A fresh
			// SignalWithStart recreates this workflow on demand.
			logger.Info("coordinator idle timeout, exiting", "session_key", input.SessionKey)
			return nil
		}

		if !haveSignal {
			// Turn completion path looped back around with no new signal yet;
			// go wait again.
			continue
		}

		payload := *pendingSignal
		pendingSignal = nil
		haveSignal = false

		if turnActive {
			// Forward into the running Turn Workflow rather than starting a
			// second one — this IS the distributed active-session guard
			// (02-architecture-temporal-execution.md §2).
			err := workflow.SignalExternalWorkflow(ctx, currentTurnID, "", NewMessageSignalName, payload).Get(ctx, nil)
			if err != nil {
				logger.Error("failed to forward signal to active turn", "turn_id", currentTurnID, "error", err)
			}
			continue
		}

		turnSeq++
		newTurnID := ids.TurnID(input.SessionKey, turnSeq)
		turnSeqCopy := turnSeq // TurnInput.TurnSeq is a pointer; don't let it alias the loop variable
		childInput := types.TurnInput{
			SessionKey:     input.SessionKey,
			TurnID:         newTurnID,
			ParentType:     "session",
			ParentID:       input.SessionKey,
			TurnSeq:        &turnSeqCopy,
			InitialMessage: payload.Message,
			ConnectionID:   input.ConnectionID,
		}
		cwo := workflow.ChildWorkflowOptions{
			WorkflowID:        newTurnID,
			ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON, // coordinator crash never tears down an in-flight turn
		}
		cctx := workflow.WithChildOptions(ctx, cwo)
		handle := workflow.ExecuteChildWorkflow(cctx, TurnWorkflow, childInput)

		// Wait for the child to actually be accepted by the server before
		// treating it as active — this is where the resolved "already
		// started" reconciliation would surface an ABANDON-survived turn from
		// a prior coordinator crash (components/02-architecture-temporal-execution.md §2).
		var childWE workflow.Execution
		startErr := handle.GetChildWorkflowExecution().Get(ctx, &childWE)
		if startErr != nil {
			if temporal.IsWorkflowExecutionAlreadyStartedError(startErr) {
				// A turn with this ID is still genuinely running (survived a
				// prior coordinator crash under ABANDON) — attach to it
				// rather than trusting our freshly-reset "no active turn"
				// assumption (02-architecture-temporal-execution.md §2).
				logger.Info("turn already running, attaching instead of starting fresh", "turn_id", newTurnID)
			} else {
				logger.Error("failed to start turn workflow", "turn_id", newTurnID, "error", startErr)
				continue
			}
		}

		currentTurnHandle = handle
		currentTurnID = newTurnID
		turnActive = true
	}
}
