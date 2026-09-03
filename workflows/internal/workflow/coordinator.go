package workflow

import (
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"

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
	// The active unit of work: a plain TurnWorkflow (Lite / conversational) or a
	// PlanWorkflow (a Deliberate task-run) — dispatchWork decides. workHandle is
	// nil when we're attached to a PlanWorkflow the coordinator didn't start
	// (a restart mid-plan); completion then arrives via the PlanDone signal.
	var workHandle workflow.ChildWorkflowFuture
	var workID string
	var workKind WorkKind
	workActive := false

	signalChan := workflow.GetSignalChannel(ctx, NewMessageSignalName)
	planDoneChan := workflow.GetSignalChannel(ctx, PlanDoneSignalName)
	var pendingSignal *types.SignalPayload
	haveSignal := false

	// docs/components/proactivity.md — a fired IntentionWorkflow wakes the
	// coordinator here. Handled as a sibling of NewMessage: no active turn →
	// start a proactive turn from a synthesised seed; active turn → fold the
	// objective in as a follow-up and let the live turn place it.
	wakeChan := workflow.GetSignalChannel(ctx, WakeSignalName)
	var pendingWake *types.WakePayload
	haveWake := false

	for {
		// Were we actually idle-waiting on entry to this iteration? Only then
		// can the idle timer be what wakes us — a turn completing (which also
		// clears workActive, below) must NOT be mistaken for an idle timeout,
		// or the coordinator exits the instant every turn ends and a follow-up
		// message can never continue an in-progress task-run. The guard gives a
		// real post-turn grace window (idleTTL).
		wasIdle := !workActive
		idleTimerCtx, cancelIdleTimer := workflow.WithCancel(ctx)
		idleTimer := workflow.NewTimer(idleTimerCtx, idleTTL)

		sel := workflow.NewSelector(ctx)
		sel.AddReceive(signalChan, func(c workflow.ReceiveChannel, more bool) {
			var payload types.SignalPayload
			c.Receive(ctx, &payload)
			pendingSignal = &payload
			haveSignal = true
		})
		sel.AddReceive(wakeChan, func(c workflow.ReceiveChannel, more bool) {
			var w types.WakePayload
			c.Receive(ctx, &w)
			pendingWake = &w
			haveWake = true
		})
		// PlanDone: a root PlanWorkflow finished its task-run. Clears the guard
		// even when we hold no handle for it (attached after a restart).
		sel.AddReceive(planDoneChan, func(c workflow.ReceiveChannel, _ bool) {
			var planID string
			c.Receive(ctx, &planID)
			if workKind == WorkPlan {
				logger.Info("plan workflow reported done", "plan_id", planID)
				workActive = false
				workID = ""
				workHandle = nil
			}
		})
		if workActive {
			if workHandle != nil {
				sel.AddFuture(workHandle, func(f workflow.Future) {
					var result types.TurnResult
					err := f.Get(ctx, &result)
					if err != nil {
						logger.Error("work workflow ended with error", "work_id", workID, "kind", workKind, "error", err)
					} else {
						logger.Info("work workflow completed", "work_id", workID, "kind", workKind, "stop_reason", result.StopReason)
					}
					workActive = false
					workID = ""
					workHandle = nil
					// docs/components/gateway/discord-voice.md's "Resolved:
					// Overlapping Speech / Interrupts" gap, closed 2026-08-25:
					// a signal that arrived while this turn's connection-based
					// delivery was still in flight got cancelled and handed
					// back here (TurnWorkflow's own signal-drain queue is
					// gone along with that execution) rather than lost.
					// Treated exactly like a freshly-arrived signal — the very
					// next loop iteration starts a brand-new turn with it, the
					// same path an ordinary NewMessage signal already takes.
					if result.InterruptedDuringDelivery != nil {
						pendingSignal = result.InterruptedDuringDelivery
						haveSignal = true
					}
				})
			}
		} else {
			sel.AddFuture(idleTimer, func(f workflow.Future) {
				// no-op callback; presence in the selector is what lets the
				// idle path win the select below
			})
		}
		sel.Select(ctx)
		cancelIdleTimer()

		if wasIdle && !workActive && !haveSignal && !haveWake {
			// The idle timer fired while we were genuinely idle (no turn just
			// completed into this branch) — self-terminate per the resolved TTL
			// design (components/session-coordinator.md). A fresh
			// SignalWithStart recreates this workflow on demand. A turn
			// finishing instead falls through to the `!haveSignal` continue
			// below, which loops back and arms a fresh idle timer — so there IS
			// a real post-turn grace window for a continuation message.
			logger.Info("coordinator idle timeout, exiting", "session_key", input.SessionKey)

			// docs/components/memory-slot.md's "Resolved: Write-Path
			// Construction" correction — session completion (this idle
			// timeout) is one of the two boundaries agent-brain's own
			// mining-pipeline-redesign contract asks for (the other is a
			// real hard context compaction, turn.go's own compressionState
			// branch). Detached child, same ABANDON reasoning turn.go's
			// WriteMemoryWorkflow doc comment already gives — this
			// workflow returns right after, doesn't wait for the write to
			// finish, only for the child to have started.
			wcwo := workflow.ChildWorkflowOptions{
				WorkflowID:        input.SessionKey + ":write-memory:" + workflow.GetInfo(ctx).WorkflowExecution.RunID,
				ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
			}
			wcctx := workflow.WithChildOptions(ctx, wcwo)
			wmFuture := workflow.ExecuteChildWorkflow(wcctx, WriteMemoryWorkflow, input.SessionKey)
			_ = wmFuture.GetChildWorkflowExecution().Get(wcctx, nil)

			// No episode-close sweep any more (decision B — no `episodes` table):
			// a PlanWorkflow that's still running when the coordinator idles
			// keeps going under ABANDON and records itself when it finishes.

			return nil
		}

		if !haveSignal && !haveWake {
			// Turn completion path looped back around with nothing new yet;
			// go wait again.
			continue
		}

		// A real inbound message takes priority over a proactive wake — if both
		// landed, handle the message this iteration and the wake next.
		if haveSignal {
			payload := *pendingSignal
			pendingSignal = nil
			haveSignal = false

			if workActive {
				// Forward into the running Turn Workflow rather than starting a
				// second one — this IS the distributed active-session guard
				// (02-architecture-temporal-execution.md §2).
				if err := workflow.SignalExternalWorkflow(ctx, workID, "", NewMessageSignalName, payload).Get(ctx, nil); err != nil {
					logger.Error("failed to forward signal to active turn", "turn_id", workID, "error", err)
				}
				continue
			}

			turnSeq++
			res, err := dispatchWork(ctx, input.SessionKey, input.ConnectionID, turnSeq, payload.Message, "user")
			if err != nil {
				logger.Error("dispatchWork failed", "session_key", input.SessionKey, "error", err)
				continue
			}
			workHandle, workID, workKind = applyWork(ctx, res, &payload)
			workActive = true
			continue
		}

		// docs/components/proactivity.md — a fired intention.
		wake := *pendingWake
		pendingWake = nil
		haveWake = false

		if workActive {
			// Fold the objective into the live turn as an ordinary follow-up;
			// that turn's model decides whether/where to surface it — it has the
			// live conversation, this workflow does not.
			fold := types.SignalPayload{Message: types.Message{Role: "user", Content: proactiveFoldText(wake)}}
			if err := workflow.SignalExternalWorkflow(ctx, workID, "", NewMessageSignalName, fold).Get(ctx, nil); err != nil {
				logger.Error("failed to fold wake into active turn", "turn_id", workID, "intention_id", wake.IntentionID, "error", err)
			}
			continue
		}

		turnSeq++
		res, err := dispatchWork(ctx, input.SessionKey, input.ConnectionID, turnSeq,
			types.Message{Role: "user", Content: proactiveSeedText(wake)}, "intn:"+wake.IntentionID)
		if err != nil {
			logger.Error("dispatchWork (proactive) failed", "intention_id", wake.IntentionID, "error", err)
			continue
		}
		workHandle, workID, workKind = applyWork(ctx, res, nil)
		workActive = true
	}
}

// applyWork maps a dispatchWork decision onto the coordinator's tracking vars.
// For WorkAttach (a follow-up to a PlanWorkflow the coordinator isn't holding a
// handle for — e.g. after a mid-plan restart), it forwards the message and
// returns a nil handle: completion then arrives via the PlanDone signal.
func applyWork(ctx workflow.Context, res WorkResult, payload *types.SignalPayload) (workflow.ChildWorkflowFuture, string, WorkKind) {
	if res.Kind == WorkAttach {
		if payload != nil {
			_ = workflow.SignalExternalWorkflow(ctx, res.WorkflowID, "", NewMessageSignalName, *payload).Get(ctx, nil)
		}
		return nil, res.WorkflowID, WorkPlan
	}
	return res.Handle, res.WorkflowID, res.Kind
}

// proactiveSeedText builds the seed "user" message (ClassifyRequest requires
// role='user', seq=0 — turns.initiated_by carries the real provenance) for a
// proactive turn started with no conversation in flight.
func proactiveSeedText(w types.WakePayload) string {
	s := "[Proactive check — you set this intention for yourself; the user did not send this message]\n\n" + w.Objective
	if w.Why != "" {
		s += "\n\n" + w.Why
	}
	return s + "\n\nDecide whether and how to surface this to the user now. Check whatever you need to " +
		"first. If nothing is worth saying right now, end the turn without responding."
}

// proactiveFoldText builds the follow-up message when a wake arrives while a
// turn is already running — the live turn's model decides placement.
func proactiveFoldText(w types.WakePayload) string {
	s := "[Proactive note — surface this to the user if and when it fits the conversation]\n\n" + w.Objective
	if w.Why != "" {
		s += "\n\n" + w.Why
	}
	return s
}
