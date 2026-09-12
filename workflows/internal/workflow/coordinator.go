package workflow

import (
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// IdleTTL is deliberately short in this local-dev slice so the coordinator's
// self-termination behavior is easy to observe without a long wait. The real
// design's resolved default is 5-15 minutes (components/session-coordinator.md);
// 30s here is a dev-loop convenience, not a design change. Exported so a
// gateway's own KeepAlive cadence (mobile/conn.go) can be derived as a
// fraction of it rather than hardcoding a second, possibly-stale constant
// that has to be remembered to move in lockstep with this one.
const IdleTTL = 30 * time.Second

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
	// The active unit of work: one TurnWorkflow (startTurn starts it, we hold its
	// future). nil / not-active between turns.
	var workHandle workflow.ChildWorkflowFuture
	var workID string
	workActive := false

	signalChan := workflow.GetSignalChannel(ctx, NewMessageSignalName)
	var pendingSignal *types.SignalPayload
	haveSignal := false

	// docs/components/proactivity.md — a fired IntentionWorkflow wakes the
	// coordinator here. Handled as a sibling of NewMessage: no active turn →
	// start a proactive turn from a synthesised seed; active turn → fold the
	// objective in as a follow-up and let the live turn place it.
	wakeChan := workflow.GetSignalChannel(ctx, WakeSignalName)
	var pendingWake *types.WakePayload
	haveWake := false

	// docs/components/gateway/first-party-plan.md's cancel/stop primitive —
	// its own signal (never a NewMessage payload, see CancelSignalName's own
	// doc comment). No active turn means nothing to cancel — a plain no-op,
	// never starts one the way a real NewMessage would.
	cancelChan := workflow.GetSignalChannel(ctx, CancelSignalName)
	haveCancel := false

	// docs/components/gateway/first-party-plan.md's cross-replica presence —
	// KeepAliveSignalName's own doc comment has the full reasoning. Widens
	// the idle-exit condition below; needs no forwarding, no payload, no
	// tracking of who sent it.
	keepAliveChan := workflow.GetSignalChannel(ctx, KeepAliveSignalName)
	haveKeepAlive := false

	for {
		// Were we actually idle-waiting on entry to this iteration? Only then
		// can the idle timer be what wakes us — a turn completing (which also
		// clears workActive, below) must NOT be mistaken for an idle timeout,
		// or the coordinator exits the instant every turn ends and a follow-up
		// message can never continue an in-progress task-run. The guard gives a
		// real post-turn grace window (idleTTL).
		wasIdle := !workActive
		idleTimerCtx, cancelIdleTimer := workflow.WithCancel(ctx)
		idleTimer := workflow.NewTimer(idleTimerCtx, IdleTTL)

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
		sel.AddReceive(cancelChan, func(c workflow.ReceiveChannel, more bool) {
			var ignored struct{}
			c.Receive(ctx, &ignored)
			haveCancel = true
		})
		sel.AddReceive(keepAliveChan, func(c workflow.ReceiveChannel, more bool) {
			var ignored struct{}
			c.Receive(ctx, &ignored)
			haveKeepAlive = true
		})
		if workActive {
			sel.AddFuture(workHandle, func(f workflow.Future) {
				var result types.TurnResult
				err := f.Get(ctx, &result)
				if err != nil {
					logger.Error("turn workflow ended with error", "turn_id", workID, "error", err)
					// docs/components/turn-pipeline.md, "Progress watchdog" —
					// a TurnWorkflow killed by its WorkflowRunTimeout (or any
					// other hard failure) cannot run failTurn, so nothing has
					// told the user. The coordinator holds the future, so it
					// is the one place that still can: best-effort fallback
					// notice, same tolerance as every other bookkeeping call
					// here.
					deliverWedgedFallback(ctx, input.SessionKey, input.ConnectionID, workID)
				} else {
					logger.Info("turn workflow completed", "turn_id", workID, "stop_reason", result.StopReason)
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
		} else {
			sel.AddFuture(idleTimer, func(f workflow.Future) {
				// no-op callback; presence in the selector is what lets the
				// idle path win the select below
			})
		}
		sel.Select(ctx)
		cancelIdleTimer()

		if wasIdle && !workActive && !haveSignal && !haveWake && !haveCancel && !haveKeepAlive {
			// The idle timer fired while we were genuinely idle (no turn just
			// completed into this branch, and nothing has sent a KeepAlive
			// recently either — first-party-plan.md's cross-replica
			// presence: as long as some gateway connection keeps one
			// arriving, this branch never fires, so WriteMemoryWorkflow
			// below waits for "nobody needs this any more," not just "the
			// conversation went quiet") — self-terminate per the resolved
			// TTL design (components/session-coordinator.md). A fresh
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

			return nil
		}

		if !haveSignal && !haveWake && !haveCancel && !haveKeepAlive {
			// Turn completion path looped back around with nothing new yet;
			// go wait again.
			continue
		}

		// A cancel takes priority over everything else landing the same tick
		// — an explicit stop shouldn't be starved behind processing one more
		// message first. Nothing is lost either way: a real message that
		// also arrived this tick stays in pendingSignal/haveSignal and is
		// handled next iteration, same as it would be for a wake.
		if haveCancel {
			haveCancel = false
			if workActive {
				if err := workflow.SignalExternalWorkflow(ctx, workID, "", CancelSignalName, struct{}{}).Get(ctx, nil); err != nil {
					logger.Error("failed to forward cancel to active turn", "turn_id", workID, "error", err)
				}
			}
			// No active turn: nothing to cancel, deliberately a no-op —
			// never starts one the way a real NewMessage would.
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
			h, id, err := startTurn(ctx, input.SessionKey, input.ConnectionID, turnSeq, payload.Message, "user")
			if err != nil {
				logger.Error("startTurn failed", "session_key", input.SessionKey, "error", err)
				continue
			}
			workHandle, workID = h, id
			workActive = true
			continue
		}

		if haveWake {
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
			h, id, err := startTurn(ctx, input.SessionKey, input.ConnectionID, turnSeq,
				types.Message{Role: "user", Content: proactiveSeedText(wake)}, "intn:"+wake.IntentionID)
			if err != nil {
				logger.Error("startTurn (proactive) failed", "intention_id", wake.IntentionID, "error", err)
				continue
			}
			workHandle, workID = h, id
			workActive = true
			continue
		}

		// The only remaining possibility per the continue-guard above —
		// nothing to do beyond having looped: the idle timer was already
		// cancelled on entry and gets re-armed fresh next iteration, which
		// is the whole mechanism (idle-exit becomes N seconds from the LAST
		// KeepAlive, not from session start). Lowest priority of the four
		// signal kinds on purpose — it never delays a real cancel, message,
		// or wake even by one iteration.
		haveKeepAlive = false
	}
}

// proactiveSeedText builds the seed "user" message (role='user', seq=0 — the
// turn's first ModelCall reads it as the request; turns.initiated_by carries
// the real provenance) for a proactive turn started with no conversation in
// flight.
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
