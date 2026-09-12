package workflow

import (
	"errors"
	"strconv"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// NewMessageSignalName is the signal channel a running Turn Workflow listens on
// for follow-up messages forwarded by the Session Coordinator
// (02-architecture-temporal-execution.md §3).
const NewMessageSignalName = "NewMessage"

// CancelSignalName — docs/components/gateway/first-party-plan.md's cancel/stop
// primitive. Deliberately its OWN signal, not a SignalPayload.Command field on
// NewMessage: a cancel must never be mistaken for real conversational content
// by any of the branches that inspect pendingMessages (status=="done" folding
// a stale follow-up back in, status=="blocked" doing the same, etc.) — those
// all mean "here's new input, adapt," which is exactly the opposite of what a
// cancel means. Sent by the Gateway directly to the session's own
// CoordinatorWorkflow via a plain SignalWorkflow (never SignalWithStart — a
// cancel with no active coordinator has nothing to do, so it should just no-op
// rather than spin one up), which forwards it into the active Turn Workflow
// exactly like NewMessage forwarding (coordinator.go).
const CancelSignalName = "Cancel"

// KeepAliveSignalName — docs/components/gateway/first-party-plan.md's
// cross-replica presence discussion. Deliberately harness-agnostic: the
// coordinator has no concept of a "device" or a "connection", only that
// something wants this session's idle timer held off a while longer. No
// payload, no tracking of who or why — a gateway sends this on its own
// schedule for as long as it has a live connection watching the session, and
// simply stops when that connection ends; there is no corresponding
// "disconnect" signal. Absence of KeepAlive for one idleTTL window IS
// "nobody needs this anymore," symmetric with how turn-activity idle-exit
// already works — a crashed gateway pod just stops sending it, no explicit
// cleanup required. Coordinator-only: never forwarded into the active Turn
// Workflow (unlike NewMessage/Cancel/Wake) — it has nothing to act on.
const KeepAliveSignalName = "KeepAlive"

// WakeSignalName — docs/components/proactivity.md, "The fire path". A fired
// IntentionWorkflow's FireIntention activity sends this to the session
// CoordinatorWorkflow (payload: types.WakePayload). The coordinator handles it
// as a sibling of NewMessage: with no active turn it synthesises a seed message
// and starts a proactive turn (initiated_by "intn:<id>"); with a turn already
// active it folds the objective in as a follow-up so the live turn's model
// decides placement.
const WakeSignalName = "Wake"

// modelCallChunkSignalName — docs/components/gateway.md's "Resolved:
// ModelCall Streaming". Signaled directly by the ModelCall ACTIVITY
// (Python, model_call.py), not forwarded by the Coordinator like
// NewMessage above — the one place this codebase has an activity signal
// its own parent workflow rather than just returning a result. Payload is
// a bare int (the new chunk's seq) — turn_id is already this workflow's
// own ID, and content never crosses this signal at all, matching the
// reference-passing contract this whole file's doc comment describes: this
// workflow learns "chunk N is ready" and nothing else, then reads the
// actual text from turn_deliveries by ID, same as it already does for
// every other piece of content.
const modelCallChunkSignalName = "ModelCallChunk"

// WriteMemoryWorkflow is a thin wrapper whose only job is to await the
// WriteMemory activity itself. It exists because a bare
// workflow.ExecuteActivity(...) without Get() is NOT genuinely fire-and-forget
// when the calling workflow closes moments later (as TurnWorkflow does,
// right after Persist/Deliver): the activity's completion is reported back
// against a workflow that's already closed, gets silently discarded
// server-side, and never appears as completed in the UI — confirmed via a
// real "Activity not found on completion... workflow execution already
// completed" warning while testing docs/components/memory-slot.md's
// write-path, even though the real memory_write call had genuinely
// succeeded. Started as a detached child (ParentClosePolicy: ABANDON)
// without awaiting its result — the child keeps running independently
// after the caller closes, so the activity's completion gets recorded
// against the child's own still-open history instead.
//
// Takes sessionKey, not turnID — docs/components/memory-slot.md's "Resolved:
// Write-Path Construction" correction (2026-08-29): agent-brain's own
// mining-pipeline-redesign contract asks for writes at session-completion
// and context-compaction boundaries, not once per turn. WriteMemoryActivity
// itself is fully stateless (second revision, same day) — every dispatch
// sends the session's current active-context merge (every active summary +
// every never-covered raw message), not a delta against a watermark; no
// Postgres state of its own. Dispatched from exactly two places:
// coordinator.go's idle-timeout exit (session completion) and turn.go's
// hard-compression path below (context compaction) — no longer from every
// turn's own end-of-turn block.
func WriteMemoryWorkflow(ctx workflow.Context, sessionKey string) error {
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)
	return workflow.ExecuteActivity(actx, "WriteMemory", sessionKey).Get(actx, nil)
}

// CompressContextWorkflow is WriteMemoryWorkflow's counterpart for the soft
// compression trigger (docs/components/context-slot.md, "Resolved: Duties
// and Strategies" #3 — soft fires async, doesn't block the turn). Same
// reasoning as WriteMemoryWorkflow's own doc comment for why this needs to
// be a detached child workflow rather than a bare unawaited ExecuteActivity.
func CompressContextWorkflow(ctx workflow.Context, turnID string) error {
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)
	return workflow.ExecuteActivity(actx, "CompressContext", turnID).Get(actx, nil)
}

const (
	// docs/components/turn-pipeline.md, "Iteration ceiling" — the model
	// proposes est_remaining_steps via report_status; the harness starts the
	// ceiling at baseIterations (the floor) and lets the model's estimate
	// raise it, to at most hardIterationCap. The model can never lower the
	// ceiling and never push it past the hard cap.
	baseIterations   = 20 // components/temporal-workflow.md, Resolved: Stop-Condition Default Values
	hardIterationCap = 50 // absolute — model-proposed estimates are clamped here
	estStepMultiple  = 2  // ceiling target = current iteration + est_remaining_steps * this

	maxRetries   = 5         // turn-level cumulative cap, distinct from per-activity MaximumAttempts
	budgetTokens = 2_000_000 // high placeholder ceiling, not infinite — see resolved doc; turn-local API spend, unrelated to context size below

	// docs/components/context-slot.md, "Resolved: Duties and Strategies" #3
	// — two-tier threshold, not one constant. Soft: fire compaction async,
	// don't block this turn. Hard: block until compaction completes. Both
	// compared against mcOut.ContextTokens (the session-wide assembled
	// context size ModelCall reports each call), not the turn-local
	// cumulativeTokens above — a genuinely different quantity (API spend
	// this turn vs. current context size).
	//
	// Fallback-only now — docs/components/model-registry.md exists (that
	// doc's own "Not blocked on that component landing first — swap the
	// source of truth when it does" instruction, acted on here): the real
	// thresholds below are a fraction of mcOut.ContextWindow, the active
	// model's actual context window. These constants only apply when
	// ContextWindow is 0 (the fixture path, which never assembles real
	// context — see model_call.py).
	softCompressionThreshold = 1_000_000
	hardCompressionThreshold = 1_500_000

	// docs/components/model-registry.md, "Responsibilities" — soft/hard as
	// a fraction of the active model's real context window, not a fixed
	// token count that's wrong for whichever model isn't the one it was
	// tuned against. Placeholder-simple fractions, not derived from
	// anything precise, same tolerance for approximation as the constants
	// above.
	softCompressionFraction = 0.6
	hardCompressionFraction = 0.8

	// activityTimeoutTierA matches the stub ModelCall/InsertMessage/Persist/Deliver
	// activities' shape: sub-2s, fire-and-complete, no heartbeat — Tier A per
	// components/activities-outbound-delivery.md. Real Tier B/C tuning is
	// deliberately deferred (components/temporal-workflow.md).
	activityTimeoutTierA = 30 * time.Second

	// voiceChunkDeliveryTimeout — connectionDeliveryChunkActivity's budget
	// for VoiceDeliverChunk: one sentence's worth of TTS synthesis plus
	// real-time playback of it, not activityTimeoutTierA's near-instant
	// text-edit assumption. Generous relative to how long one sentence
	// actually takes to speak, well under the voice platform's own
	// whole-turn 5-minute budget (turn.go's deliveryTaskQueue) since this is
	// deliberately a much smaller unit of work.
	voiceChunkDeliveryTimeout = 90 * time.Second
)

// compressionState mirrors lcm.compression_state's classification
// (docs/components/context-slot.md) — kept in Go too since the workflow is
// what has to act differently on "soft" (async, non-blocking) vs "hard"
// (blocking) before it can dispatch either compaction path. contextWindow
// of 0 (the fixture path — see model_call.py) falls back to the static
// thresholds above instead of computing a fraction of nothing.
func compressionState(contextTokens, contextWindow int) string {
	soft, hard := softCompressionThreshold, hardCompressionThreshold
	if contextWindow > 0 {
		soft = int(float64(contextWindow) * softCompressionFraction)
		hard = int(float64(contextWindow) * hardCompressionFraction)
	}
	if contextTokens >= hard {
		return "hard"
	}
	if contextTokens >= soft {
		return "soft"
	}
	return "none"
}

// failTurn is the shared cleanup path for every early-return failure inside
// TurnWorkflow's loop. Before this existed, a bare `return types.TurnResult{},
// err` (e.g. ModelCall exhausting its escalate-on-retry ladder at the expert
// tier — docs/components/model-registry.md's "Fallback beyond
// escalate-on-retry" open question) skipped the end-of-loop Persist/Deliver
// entirely: the turns row stayed stuck at 'running' forever and the user
// never received any response or error for that turn — not a designed
// fallback, an actual silent drop, confirmed by reading the code path (no
// defer/recover anywhere in this function). All three of TurnWorkflow's
// early-return sites shared this exact defect, not just the ModelCall one —
// routed through one helper instead of copying the same fix three times.
//
// Best-effort only, same tolerance as the normal end-of-turn Persist/Deliver
// calls (`_ = ...Get(...)`, errors ignored): if the turn's own row was never
// created (e.g. the very first InsertMessage call itself failed), the
// synthetic error message insert fails its FK against turns(turn_id) and the
// Persist/Deliver calls become harmless no-ops — there's nothing more
// meaningful to do when the turn never existed in the first place.
func failTurn(ctx workflow.Context, turnID, sessionKey, connectionID string, parentType string, cause error, interrupts *deliveryInterruptSource) (types.TurnResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Error("turn failed", "turn_id", turnID, "error", cause)

	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)

	errInsert := types.InsertMessageInput{
		TurnID:  turnID,
		Message: types.Message{Role: "assistant", Content: "Something went wrong processing this turn."},
	}
	_ = workflow.ExecuteActivity(actx, "InsertMessage", errInsert).Get(actx, nil)
	_ = workflow.ExecuteActivity(actx, "Persist", turnID, "failed").Get(actx, nil)
	if parentType == "session" {
		// The failure-notice text is short and fixed — no recovery needed
		// here even on a ContentTooLong error, unlike the normal end-of-turn
		// site below.
		payload, _ := deliverConnectionBased(ctx, interrupts, sessionKey, connectionID, turnID)
		if payload != nil {
			// nil error, not cause: Temporal discards a child workflow's
			// return VALUE when it also returns a non-nil error (recorded
			// as a failed execution instead) — returning cause here would
			// silently drop InterruptedDuringDelivery before
			// coordinator.go ever saw it. cause is already logged above;
			// from the Coordinator's perspective this turn concluded
			// (with an error handled internally) and handed off a new
			// message, not a Temporal-level failure worth flagging red.
			return types.TurnResult{TurnID: turnID, InterruptedDuringDelivery: payload}, nil
		}
	}
	return types.TurnResult{}, cause
}

// deliveryInterruptSource lets deliverConnectionBased race a connection-based
// delivery activity against a new signal arriving mid-flight, without giving
// it direct access to TurnWorkflow's own local pendingMessages/signalChan —
// nil is a valid, meaningful value (docs/components/gateway/discord-voice.md's
// "Resolved: Overlapping Speech / Interrupts"): the very first failTurn call
// site (before this infrastructure is even set up, mid-InsertMessage-failure)
// has nothing to race against, and deliverConnectionBased degrades to a
// plain uninterruptible await in that case, same as before this existed.
type deliveryInterruptSource struct {
	notify   workflow.Channel       // buffered, non-blocking-sent — see TurnWorkflow's own signal-draining goroutine
	messages *[]types.SignalPayload // TurnWorkflow's own pendingMessages, read (not drained) once interrupted
}

// deliverConnectionBased — gateway.md's "Resolved: Outbound Flow" (2026-08-25
// correction), and the single call site every plan-owned turn's own delivery
// also routes through as of 2026-09-06 (see deliveryTaskQueue's own doc
// comment on why the old separate no-op-stub-plus-this-function split was
// removed: nothing calls a generic "Deliver" on the default queue anymore —
// this IS the delivery, for every platform that has a live connection to
// deliver over). Web correctly gets a no-op here (deliveryTaskQueue's
// ok=false) — gateway/web.md's "delivery collapses" finding, Web gets
// responses via polling Postgres directly and never needed a real send.
// Routed to the specific connection's own task queue, addressed by
// connectionID (never platform alone — a tenant can run more than one
// connection of the same platform kind, e.g. two Discord bots, so platform
// alone would be ambiguous about which live socket to send over).
//
// Races the delivery activity against interrupts.notify (docs/components/
// gateway/discord-voice.md's "Resolved: Overlapping Speech / Interrupts" gap,
// closed 2026-08-25): a signal arriving while delivery is still in flight
// cancels it rather than being silently discarded once this call returns.
// Returns the interrupting payload (non-nil) if that happened — the caller
// is responsible for handing it back to the Coordinator via TurnResult,
// since only the caller has a real return path there. interrupts is nil for
// every plan-owned call site (plan_workflow.go) — none of them have (or
// need) an interrupt-racing source; nil degrades to a plain,
// uninterruptible send.
//
// Also returns the delivery activity's own error — but ONLY when it's the
// types.ErrTypeContentTooLong case (real, live bug found 2026-09-06: every
// other error here was, and still is, silently discarded, same tolerance
// Persist/other best-effort bookkeeping calls already get elsewhere in this
// file; deliberately not widening that now). The caller uses this one case
// to run a model-driven recovery round instead of simply losing the response.
func deliverConnectionBased(ctx workflow.Context, interrupts *deliveryInterruptSource, sessionKey, connectionID, turnID string) (*types.SignalPayload, error) {
	queue, timeout, ok := deliveryTaskQueue(sessionKey, connectionID)
	if !ok {
		return nil, nil
	}

	deliverCtx, deliverCancel := workflow.WithCancel(ctx)
	defer deliverCancel()
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: timeout,
		TaskQueue:           queue,
	}
	actx := workflow.WithActivityOptions(deliverCtx, ao)
	future := workflow.ExecuteActivity(actx, "Deliver", turnID)

	deliverErr := func(err error) error {
		var appErr *temporal.ApplicationError
		if errors.As(err, &appErr) && appErr.Type() == types.ErrTypeContentTooLong {
			return err
		}
		return nil
	}

	if interrupts == nil || interrupts.notify == nil {
		err := future.Get(actx, nil)
		return nil, deliverErr(err)
	}

	interrupted := false
	var settledErr error
	sel := workflow.NewSelector(ctx)
	sel.AddFuture(future, func(f workflow.Future) {
		settledErr = f.Get(actx, nil)
	})
	sel.AddReceive(interrupts.notify, func(c workflow.ReceiveChannel, more bool) {
		c.Receive(ctx, nil)
		interrupted = true
		deliverCancel()
	})
	sel.Select(ctx)
	if !interrupted {
		return nil, deliverErr(settledErr)
	}
	_ = future.Get(actx, nil) // wait for the now-cancelling activity to actually finish
	msgs := *interrupts.messages
	if len(msgs) == 0 {
		return nil, nil
	}
	payload := msgs[len(msgs)-1]
	return &payload, nil
}

// deliveryRecoveryRoundCap — bounded, same "retry a couple of times then give
// up" shape as this codebase's other retry ceilings (e.g. plan_workflow.go's
// planApprovalRevisionCap), not an unbounded loop.
const deliveryRecoveryRoundCap = 2

// runDiscordDeliveryRecovery — real, live bug fixed 2026-09-06: Deliver's
// error used to be silently discarded entirely (deliverConnectionBased's own
// `_ = future.Get(...)`), so a final answer too long for one Discord message
// just vanished, turn marked complete, no trace anywhere. Now that a
// ContentTooLong failure surfaces (see deliverConnectionBased above), this
// closes the loop the way docs/components/activities-outbound-delivery.md's
// "Retry Policy: Model-Driven, Not a Static Playbook" already does for every
// other tool failure: feed the model an observation and let IT decide how to
// redeliver — deliver_reply (split at natural boundaries) or
// deliver_attachment — informed by whatever it retrieves from
// skills/seeds/deliver-long-content.json, not a mechanical Go split.
//
// Bounded at deliveryRecoveryRoundCap rounds of its own small ModelCall +
// dispatch (not the main loop — this runs after TurnWorkflow's own loop has
// already exited, so it can't just `continue loop` back in without
// restructuring that loop's stop-condition machinery, which is out of scope
// here). If the model still hasn't gotten anything delivered by the cap,
// this logs and gives up — an accepted, bounded gap (same tolerance this
// codebase already gives a dropped streamed preview chunk elsewhere), not a
// mechanical fallback duplicating deliver_discord.go's own length-checked
// sends.
func runDiscordDeliveryRecovery(ctx workflow.Context, turnID, connectionID string, contextSeq int) {
	logger := workflow.GetLogger(ctx)
	iao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	iactx := workflow.WithActivityOptions(ctx, iao)

	obs := "Your reply didn't fit as one Discord message. Use deliver_reply (call it more than once " +
		"to split at natural boundaries — paragraphs, sections) or deliver_attachment (send it as a " +
		"file instead) to actually get it to the user."
	insert := types.InsertMessageInput{TurnID: turnID, Message: types.Message{Role: "user", Content: obs}}
	if err := workflow.ExecuteActivity(iactx, "InsertMessage", insert).Get(iactx, nil); err != nil {
		logger.Warn("delivery recovery: failed to insert observation", "turn_id", turnID, "error", err)
		return
	}

	delivered := false
	for round := 0; round < deliveryRecoveryRoundCap && !delivered; round++ {
		contextSeq++
		var mcOut types.ModelCallOutput
		mao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 3}}
		mctx := workflow.WithActivityOptions(ctx, mao)
		modelInput := types.ModelCallInput{TurnID: turnID, ContextSeq: contextSeq, OfferDeliveryTools: true}
		if err := workflow.ExecuteActivity(mctx, "ModelCall", modelInput).Get(mctx, &mcOut); err != nil {
			logger.Warn("delivery recovery: ModelCall failed", "turn_id", turnID, "round", round, "error", err)
			return
		}
		if len(mcOut.ToolCalls) == 0 {
			break
		}
		for _, tc := range mcOut.ToolCalls {
			activityName, ok := deliveryToolActivity("discord", tc.ToolName)
			if !ok {
				continue // the model called something else here — not this routine's concern
			}
			dao := workflow.ActivityOptions{
				ActivityID:          tc.ToolCallID,
				StartToCloseTimeout: activityTimeoutTierA,
				TaskQueue:           "deliver:discord:" + connectionID,
				RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
			}
			dctx := workflow.WithActivityOptions(ctx, dao)
			var out types.ToolCallOutput
			if err := workflow.ExecuteActivity(dctx, activityName, types.ToolCallInput{ToolCallID: tc.ToolCallID}).Get(dctx, &out); err == nil && out.Status == "ok" {
				delivered = true
			}
		}
	}
	if !delivered {
		logger.Error("delivery recovery: gave up after round cap, response may not have reached the user", "turn_id", turnID)
	}
}

// platformFromSessionKey extracts the platform segment from a session_key of
// the form "agent:main:{platform}:...". gateway.md's "Resolved: Multi-Session
// Channels" — every platform's session key format agrees on this prefix.
func platformFromSessionKey(sessionKey string) string {
	parts := strings.SplitN(sessionKey, ":", 4)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// deliveryTaskQueue — gateway.md's "Resolved: Outbound Flow": only a
// connection-based platform needs the embedded-worker delivery path at all
// (Web has no live connection — it delivers via polling, gateway/web.md's
// "delivery collapses" finding — so it correctly gets ok=false, no call at
// all, not a separate stub activity to remember to also invoke). Every
// connection-based platform's real Deliver implementation (deliver_discord.go,
// deliver_voice.go) is registered under the SAME literal activity name,
// "Deliver" — no per-platform name branching needed, since each is on its
// own task queue and Temporal activity names only need to be unique within
// one queue's worker, never globally. This function's only job is picking
// that queue (and the per-platform timeout — VoiceDeliver synthesizes and
// streams real audio, which can run well past 30s for a multi-sentence
// response, so it gets its own longer budget; DiscordDeliver is a near-
// instant text send). A literal lookup, not a registry or a dispatching
// function — it never calls ExecuteActivity itself, every caller does that
// the same, ordinary way. Two platforms exist today; adding a third is a
// one-line change here, not a reason to build an abstraction for cases that
// don't exist yet.
func deliveryTaskQueue(sessionKey, connectionID string) (queue string, timeout time.Duration, ok bool) {
	if connectionID == "" {
		return "", 0, false
	}
	switch platformFromSessionKey(sessionKey) {
	case "discord":
		return "deliver:discord:" + connectionID, activityTimeoutTierA, true
	case "discord-voice":
		return "deliver:discord-voice:" + connectionID, 5 * time.Minute, true
	default:
		return "", 0, false
	}
}

// --- Progress watchdog (docs/components/turn-pipeline.md, "Progress
// watchdog"). A workflow.Go goroutine, scoped to the turn's lifetime, that
// narrates "still working" while the model is heads-down — so the model never
// has to. Only connection-based text platforms need it: voice already has its
// own filler-audio player (voice_filler_player.go) covering the same gap
// better, and Web surfaces progress through its poll. So it's Discord-text
// only today; a third platform is a one-line addition here, same as
// deliveryTaskQueue.

// statusPingBackoff is the per-fire delay ladder: arm this long, and if no
// user-visible delivery landed in the meantime, ping and step to the next
// rung. Deliberately NOT sub-10s — a normal ModelCall already runs tens of
// seconds; the watchdog is for an abnormally long stretch, not routine ones.
// Numeric tuning is explicitly deferred (turn-pipeline.md's Deferred list) —
// adjust with real latency data.
func statusPingBackoff(platform string) []time.Duration {
	switch platform {
	case "discord", "mobile":
		return []time.Duration{20 * time.Second, 45 * time.Second, 90 * time.Second}
	default:
		return nil
	}
}

// statusDeliverActivity — the per-platform gateway activity name that pushes
// the latest turn_status_pings row's line out on the connection's own embedded
// worker (same literal-lookup idiom as deliveryTaskQueue / deliveryToolActivity).
func statusDeliverActivity(platform string) (activityName string, ok bool) {
	switch platform {
	case "discord":
		return "DiscordDeliverStatus", true
	default:
		return "", false
	}
}

// notifyProgress writes a StatusPing and, only for platforms that need an
// EXPLICIT push-to-one-connection dispatch (Discord — no live connection to
// tail, so the gateway has to actively post something), delivers it too.
// Mobile self-delivers via migration 032/033's NOTIFY triggers: the write
// alone already reaches every connected device, no dispatch activity needed
// or wanted. Shared by two call sites with different TRIGGERS for the exact
// same write: runProgressWatchdog's backoff timer (nothing has happened in a
// while — a periodic reminder) and the tool-dispatch call site in the fan-out
// below (something just started — instant, event-driven, not timer-gated).
// Best-effort throughout: a failure is logged, never surfaced into the turn.
func notifyProgress(ctx workflow.Context, turnID, sessionKey, connectionID, reason string, logger log.Logger) {
	platform := platformFromSessionKey(sessionKey)
	deliverName, deliverOK := statusDeliverActivity(platform)
	queue, _, queueOK := deliveryTaskQueue(sessionKey, connectionID)
	dispatch := deliverOK && queueOK

	var line string
	pctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA})
	if err := workflow.ExecuteActivity(pctx, "StatusPing", turnID, reason).Get(pctx, &line); err != nil {
		logger.Warn("notifyProgress: StatusPing failed", "turn_id", turnID, "reason", reason, "error", err)
		return
	}
	if line == "" || !dispatch {
		// Self-delivering platforms (mobile) stop here — the write above
		// already reached every connected device via NOTIFY.
		return
	}
	dctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: activityTimeoutTierA,
		TaskQueue:           queue,
	})
	if err := workflow.ExecuteActivity(dctx, deliverName, turnID).Get(dctx, nil); err != nil {
		logger.Warn("notifyProgress: status delivery failed", "turn_id", turnID, "reason", reason, "error", err)
	}
}

// runProgressWatchdog is the goroutine body. progress is a monotonic counter
// the turn's main goroutine bumps on every real step (a new reason-act
// iteration, the final delivery); done is set once the turn's loop has exited.
// Both are plain shared values — safe here for the same reason
// deliveryInterruptSource.messages is: workflow goroutines are cooperatively
// scheduled, never truly concurrent. All activity calls are on the root ctx,
// independent of the per-iteration cancelCtx.
func runProgressWatchdog(ctx workflow.Context, turnID, sessionKey, connectionID string, progress *int, done *bool) {
	platform := platformFromSessionKey(sessionKey)
	steps := statusPingBackoff(platform)
	if len(steps) == 0 {
		return
	}
	logger := workflow.GetLogger(ctx)

	stepIdx := 0
	lastSeen := *progress
	for {
		if err := workflow.NewTimer(ctx, steps[stepIdx]).Get(ctx, nil); err != nil {
			return // ctx cancelled — the workflow is ending
		}
		if *done {
			return
		}
		if *progress != lastSeen {
			// Real progress since the timer was armed — collapse the backoff
			// and don't ping; the user isn't actually left hanging.
			lastSeen = *progress
			stepIdx = 0
			continue
		}

		notifyProgress(ctx, turnID, sessionKey, connectionID, "", logger)
		if stepIdx < len(steps)-1 {
			stepIdx++
		}
	}
}

// deliverWedgedFallback is the coordinator's path (coordinator.go holds the
// TurnWorkflow child future) for the one case turn.go's own failTurn can't
// cover: the workflow was killed outright by its WorkflowRunTimeout, so no
// in-workflow code ran to tell the user. Writes a 'wedged' status ping —
// self-delivering on mobile via NOTIFY, no explicit push needed there (see
// runProgressWatchdog's own doc comment) — and, for platforms that need an
// explicit dispatch (Discord), pushes it too. Entirely best-effort — if the
// platform has no push channel at all (Web) or a step fails, it's logged and
// dropped, same as every other bookkeeping call in the coordinator.
func deliverWedgedFallback(ctx workflow.Context, sessionKey, connectionID, turnID string) {
	notifyProgress(ctx, turnID, sessionKey, connectionID, "wedged", workflow.GetLogger(ctx))
}

// connectionDeliveryChunkActivity mirrors deliveryTaskQueue above,
// for the per-chunk streaming path (awaitModelCallWithStreaming below) —
// deliberately not unified into one lookup shared with it: VoiceDeliverChunk
// returns (interrupted bool, error) so the caller can stop delivering
// further chunks once the human has barged in mid-playback, while
// DiscordDeliverChunk returns a plain error (no barge-in concept for text) —
// a genuinely different result shape, not just a different name/timeout, so
// the caller still needs a platform branch regardless. This only avoids
// hardcoding the activity name/timeout pair twice.
func connectionDeliveryChunkActivity(platform string) (activityName string, timeout time.Duration, ok bool) {
	switch platform {
	case "discord":
		return "DiscordDeliverChunk", activityTimeoutTierA, true
	case "discord-voice":
		return "VoiceDeliverChunk", voiceChunkDeliveryTimeout, true
	default:
		return "", 0, false
	}
}

// deliveryToolActivity — deliver_reply/deliver_attachment are real model
// tool calls (docs/components/activities-outbound-delivery.md's model-driven
// retry philosophy, applied to delivery itself), but unlike an ordinary tool
// they need the owning gateway connection's own live session, so the Act
// dispatch loop below routes them here instead of through the generic
// "ToolCall" tenant-worker path — same deliveryTaskQueue/
// connectionDeliveryChunkActivity literal-lookup idiom, just keyed on tool
// name instead of a fixed per-platform pair. Discord only for now (delivery-
// in-the-loop landed Discord-first); ok=false elsewhere falls through to the
// generic path, which will correctly fail these as "unknown tool" since
// capabilities.py gives them no handler_ref.
func deliveryToolActivity(platform, toolName string) (activityName string, ok bool) {
	if platform != "discord" {
		return "", false
	}
	switch toolName {
	case "deliver_reply":
		return "DiscordDeliverReply", true
	case "deliver_attachment":
		return "DiscordDeliverAttachment", true
	default:
		return "", false
	}
}

// awaitModelCallWithStreaming replaces a plain mcFuture.Get(mctx, mcOut) for
// the one iteration that can ever stream (context_seq == 0, i.e. this
// turn's first ModelCall call — model_call.py's own gate). docs/components/
// gateway.md's "Resolved: ModelCall Streaming", extended 2026-08-26 to
// Discord voice and, in the same pass, to close a real race the original
// Discord-text-only version had: model_call.py's on_chunk awaits each
// chunk's signal RPC before returning, so every chunk (including the
// forced final flush) is durably recorded in this workflow's history no
// later than ModelCall's own completion event — but a signal being
// *recorded* only means it's waiting in the channel, not that some
// separately-scheduled consumer has actually finished dispatching and
// awaiting the corresponding delivery activity for it. The original design
// ran that consumer as a detached workflow.Go goroutine with nothing
// forcing the caller to wait for it — the turn's own end-of-turn
// Deliver/VoiceDeliver call could race ahead of (and, for voice, silently
// never play) the last one or two streamed chunks. Merging ModelCall's own
// completion and each chunk signal into one Selector loop closes this: the
// loop cannot exit until modelCallDone fires, and every chunk signal
// recorded before that event is guaranteed (by the ordering argument above)
// to already be sitting in chunkSignalChan, so it will have been received
// and its delivery activity started — and, since each case body fully
// awaits its own delivery before the loop can select again, completed —
// before this function returns control to TurnWorkflow's main loop.
//
// Only ever reached for "discord"/"discord-voice" with a non-empty
// ConnectionID (the caller's own gate) — connectionDeliveryChunkActivity
// returning ok==false here would mean that gate and this switch have
// drifted out of sync; falls back to a plain await rather than blocking
// forever on a channel nothing will ever populate.
func awaitModelCallWithStreaming(ctx workflow.Context, mcFuture workflow.Future, mctx workflow.Context, mcOut *types.ModelCallOutput, turnID, connectionID, platform string) error {
	activityName, timeout, ok := connectionDeliveryChunkActivity(platform)
	if !ok {
		return mcFuture.Get(mctx, mcOut)
	}
	chunkSignalChan := workflow.GetSignalChannel(ctx, modelCallChunkSignalName)

	var mcErr error
	modelCallDone := false
	// bargedIn — voice-only (DiscordDeliverChunk's branch below never sets
	// it): true once one chunk's playback has been stopped by the fast-path
	// barge-in (voice_bargein.go, via VoiceDeliverChunk's own return value).
	// docs/components/gateway/discord-voice.md's "Resolved: Overlapping
	// Speech / Interrupts" — once the human has started talking over this
	// turn's audio, synthesizing and playing its later sentences would talk
	// over them a second time; every chunk signal received after this
	// point is drained (received, so the channel doesn't back up) but never
	// dispatched.
	bargedIn := false

	// lastSignalAt/chunkMetrics — docs/components/gateway/discord-voice.md's
	// Notes Log real-latency-metrics work: voice_chunk_signal_gap_seconds
	// measures the gap between consecutive sentence chunks actually being
	// GENERATED (this signal firing), as distinct from the Gateway's own
	// voice_chunk_gap_seconds (the gap between consecutive chunks' audio
	// being PLAYED) — comparing the two is what tells apart "the LLM is
	// slow to generate the next sentence" from "delivery/TTS is slow to
	// catch up with a model that's already keeping pace". workflow.Now, not
	// time.Now — this is workflow code, replayed deterministically.
	var lastSignalAt time.Time
	chunkMetrics := workflow.GetMetricsHandler(ctx).WithTags(map[string]string{
		"namespace": workflow.GetInfo(ctx).Namespace,
		"platform":  platform,
	})

	// deliverChunk dispatches and fully awaits one chunk's delivery —
	// factored out so both the Selector callback below AND the post-loop
	// drain (its own comment has why that second call site is required)
	// share exactly the same dispatch logic.
	deliverChunk := func(seq int) {
		now := workflow.Now(ctx)
		if !lastSignalAt.IsZero() {
			chunkMetrics.Timer("voice_chunk_signal_gap_seconds").Record(now.Sub(lastSignalAt))
		}
		lastSignalAt = now
		if bargedIn {
			return
		}
		cao := workflow.ActivityOptions{
			StartToCloseTimeout: timeout,
			TaskQueue:           "deliver:" + platform + ":" + connectionID,
			// Real, live bug found 2026-08-29: no RetryPolicy here meant the
			// SDK default applied — effectively unlimited attempts,
			// exponential backoff capped at 100s. Both call sites below
			// already treat a chunk-delivery failure as best-effort/non-
			// fatal (their own comments: "a dropped streamed preview chunk
			// isn't fatal"), but the retry policy never actually matched
			// that stated intent — a genuinely permanent failure (confirmed
			// live: DiscordDeliverChunk rejecting an all-whitespace chunk,
			// HTTP 400 from Discord, a failure no amount of retrying would
			// ever fix) retried forever instead of giving up, and since this
			// call blocks the Selector callback that owns it, the entire
			// TurnWorkflow was wedged before it ever processed ModelCall's
			// already-completed result — not a dropped preview, a
			// permanently stuck turn. MaximumAttempts: 3 matches this
			// codebase's own convention for "bounded retry, then treat as
			// failed" (e.g. user_input.go's ToolCall dispatch).
			RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 3},
		}
		cactx := workflow.WithActivityOptions(ctx, cao)
		if platform == "discord-voice" {
			var interrupted bool
			if err := workflow.ExecuteActivity(cactx, activityName, turnID, seq).Get(cactx, &interrupted); err != nil {
				// Best-effort, same tolerance as the DiscordDeliverChunk
				// branch below — a dropped streamed chunk isn't fatal to the
				// turn; VoiceDeliver's own end-of-turn call is the
				// authoritative fallback for a turn that was never streamed
				// at all, though a chunk dropped mid-stream here is
				// genuinely lost audio, not just a superseded preview the
				// way a dropped Discord text edit is (turns.voice_streamed
				// already being true by then skips VoiceDeliver's replay,
				// same as streamed_message_ref does for text) — an accepted,
				// bounded gap, not one this codebase has built recovery for
				// on the text side either.
				workflow.GetLogger(ctx).Warn("voice chunk delivery failed", "turn_id", turnID, "seq", seq, "error", err)
			} else if interrupted {
				bargedIn = true
			}
		} else {
			if err := workflow.ExecuteActivity(cactx, activityName, turnID, seq).Get(cactx, nil); err != nil {
				// docstring above: a dropped streamed preview chunk isn't
				// fatal — the final Deliver/DiscordDeliver call is either
				// the authoritative delivery (never streamed) or a correct
				// no-op (streamed_message_ref already set).
				workflow.GetLogger(ctx).Warn("discord chunk delivery failed", "turn_id", turnID, "seq", seq, "error", err)
			}
		}
	}

	sel := workflow.NewSelector(ctx)
	sel.AddFuture(mcFuture, func(f workflow.Future) {
		mcErr = f.Get(mctx, mcOut)
		modelCallDone = true
	})
	sel.AddReceive(chunkSignalChan, func(c workflow.ReceiveChannel, more bool) {
		var seq int
		c.Receive(ctx, &seq)
		deliverChunk(seq)
	})
	for !modelCallDone {
		sel.Select(ctx)
	}
	// Selector.Select fires exactly one ready case per call — if the
	// mcFuture case and a chunk-signal case both became ready in the same
	// history batch (entirely possible: model_call.py's on_chunk awaits
	// every signal, including the final forced-flush one, before the
	// ModelCall activity itself returns, so both events can legitimately
	// land together), Select could have fired the mcFuture case FIRST,
	// exiting the loop above with a chunk signal still sitting unreceived
	// in chunkSignalChan — exactly the drop this function exists to
	// prevent. A final non-blocking drain here, using the same deliverChunk
	// logic, catches anything left over: ReceiveAsync never blocks, so this
	// terminates as soon as the channel is actually empty, and per the
	// ordering argument in this function's own doc comment, nothing more
	// will ever arrive on it for this turn after modelCallDone is true.
	for {
		var seq int
		if !chunkSignalChan.ReceiveAsync(&seq) {
			break
		}
		deliverChunk(seq)
	}
	return mcErr
}

// TurnWorkflow implements the reason-act-observe loop. One workflow *type* for
// every level of the tree — a top-level turn and a subagent are both this same
// function, distinguished only by TurnInput.ParentType and by who started them
// (components/temporal-workflow.md).
//
// Under the reference-passing contract (docs/components/temporal-workflow.md,
// "Resolved: Reference-Passing Contract"), this workflow holds NO message
// content, tool arguments, or tool results in memory at any point — only IDs
// and control-flow metadata (counters, tool names, usage numbers). Every
// content read/write happens inside an activity, against Postgres.
func TurnWorkflow(ctx workflow.Context, input types.TurnInput) (types.TurnResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("turn workflow started", "turn_id", input.TurnID, "parent_type", input.ParentType)

	// docs/components/budget-guardrails.md, "Resolved: Metrics Export" —
	// namespace-tagged once here and reused, since loop-worker is shared
	// across every tenant's namespace from one process; an untagged metric
	// would collapse every tenant's turns into one undifferentiated number.
	metrics := workflow.GetMetricsHandler(ctx).WithTags(map[string]string{"namespace": workflow.GetInfo(ctx).Namespace})

	iterations := 0
	retries := 0
	cumulativeTokens := 0
	contextSeq := 0 // ModelCall's own call-index for fixture lookup — distinct from messages.seq, which activities compute themselves
	// docs/components/model-registry.md — the model authors its next-step tier
	// hint via report_status (Phase 7); this workflow carries it forward opaquely
	// into the next ModelCallInput. Empty on the first iteration — model_call.py
	// bootstraps from model_registry.default_hint() Python-side.
	hintModality, hintTier := "", ""

	// --- Start-of-turn: write the inbound message (or, for a subagent, let
	// InsertMessage derive its kickoff content from its own tool_calls row)
	// before the first ModelCall — ModelCall's first read needs this content
	// already in Postgres (components/temporal-workflow.md, "Resolved:
	// Reference/ID Schema"). This also creates the turns row.
	//
	// PreInserted: the dispatch helper (dispatch.go) already did InsertMessage
	// before starting this workflow — skip it.
	if !input.PreInserted {
		ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
		actx := workflow.WithActivityOptions(ctx, ao)
		insertInput := types.InsertMessageInput{
			TurnID:      input.TurnID,
			Message:     input.InitialMessage,
			IsTurnStart: true,
			ParentID:    input.ParentID,
			ParentType:  input.ParentType,
			TurnSeq:     input.TurnSeq,
			InitiatedBy: input.InitiatedBy,
		}
		if err := workflow.ExecuteActivity(actx, "InsertMessage", insertInput).Get(actx, nil); err != nil {
			return failTurn(ctx, input.TurnID, input.SessionKey, input.ConnectionID, input.ParentType, err, nil)
		}
	}

	// Deterministic FIFO queue for follow-up messages, per components/temporal-workflow.md
	// "Resolved: Signal Coalescing" — the handler only appends (pure, deterministic
	// under replay); dequeue-and-fold-one happens explicitly at loop boundaries below,
	// never batched. Set up before the request-pipeline steps below so routing can
	// race its own completion against a follow-up message arriving mid-phase.
	var pendingMessages []types.SignalPayload
	signalChan := workflow.GetSignalChannel(ctx, NewMessageSignalName)
	// deliveryInterruptNotify — docs/components/gateway/discord-voice.md's
	// "Resolved: Overlapping Speech / Interrupts" gap, closed 2026-08-25.
	// Buffered (size 1) and always sent non-blocking (Selector + AddDefault):
	// this must never block the signal-draining goroutine below, which has
	// to keep appending to pendingMessages regardless of whether anything is
	// currently racing against this channel (deliverConnectionBased only
	// listens on it during the narrow window it's actually awaiting a
	// connection-based delivery — most of a turn's lifetime, nothing is).
	deliveryInterruptNotify := workflow.NewBufferedChannel(ctx, 1)
	interrupts := &deliveryInterruptSource{notify: deliveryInterruptNotify, messages: &pendingMessages}
	workflow.Go(ctx, func(gctx workflow.Context) {
		for {
			var payload types.SignalPayload
			signalChan.Receive(gctx, &payload)
			pendingMessages = append(pendingMessages, payload)
			notifySel := workflow.NewSelector(gctx)
			notifySel.AddSend(deliveryInterruptNotify, struct{}{}, func() {})
			notifySel.AddDefault(func() {})
			notifySel.Select(gctx)
		}
	})

	// cancelRequested — a separate flag, deliberately never funneled through
	// pendingMessages: several branches below treat "pendingMessages is
	// non-empty" as "new input arrived, fold it in and keep reasoning,"
	// which is the opposite of what a cancel means. A cancel only ever
	// arrives once per turn in practice (the coordinator stops forwarding
	// once workActive clears), so a plain bool is enough — no queue.
	cancelRequested := false
	cancelChan := workflow.GetSignalChannel(ctx, CancelSignalName)
	workflow.Go(ctx, func(gctx workflow.Context) {
		var ignored struct{}
		cancelChan.Receive(gctx, &ignored)
		cancelRequested = true
	})

	// --- Progress watchdog (docs/components/turn-pipeline.md). Narrates
	// "still working" while the model is heads-down. Top-level turns only — a
	// subagent has no external delivery target. progressGen is bumped on every
	// real step below; turnDone is set once the loop exits.
	progressGen := 0
	turnDone := false
	if input.ParentType == "session" {
		pg := &progressGen
		td := &turnDone
		workflow.Go(ctx, func(gctx workflow.Context) {
			runProgressWatchdog(gctx, input.TurnID, input.SessionKey, input.ConnectionID, pg, td)
		})
	}

	// docs/components/turn-pipeline.md, Phase 8 — the pre-LLM pipeline
	// (ClassifyRequest → lane decision → RoutingWorkflow retrieval fan-out) is
	// gone. The turn goes straight to the reason-act loop; the model pulls
	// memory / skills / tools on demand via the meta-tools, and picks its own
	// model tier per step via report_status.

	var stopReason string

	// docs/components/turn-pipeline.md, "Iteration ceiling". Starts at the
	// floor; the model's report_status est_remaining_steps raises it (never
	// lowers it), clamped to hardIterationCap. ceilingRaised tracks whether the
	// model ever pushed it up, for the end-of-turn calibration log.
	ceiling := baseIterations
	ceilingRaised := false
	// future-work.md §4 — a model that reports "working" but produces neither
	// content nor a tool call is stuck; two such steps running ends the turn
	// instead of burning the whole ceiling on empty responses.
	emptyStreak := 0

loop:
	for {
		// --- Resolved: Stop-Condition Logic (inline check, pure read of local state) ---
		if cancelRequested {
			// Between steps — no active tool calls to cancel, just stop
			// before starting another ModelCall. Mid-step cancellation
			// (an active tool-call batch, or a parked ask_user) is handled
			// below, where that work actually is.
			stopReason = "cancelled_by_user"
			break
		}
		if iterations >= ceiling {
			stopReason = "max_iterations"
			break
		}
		if retries >= maxRetries {
			stopReason = "max_retries"
			break
		}
		if cumulativeTokens >= budgetTokens {
			stopReason = "budget_exhausted"
			break
		}

		iterations++
		progressGen++ // a fresh reason-act pass is real progress — resets the watchdog backoff
		cancelCtx, cancel := workflow.WithCancel(ctx)

		// --- Reason: model-call activity (mints tool_call_id/subagent IDs
		// itself, writes its own response to Postgres, returns refs only) ---
		var mcOut types.ModelCallOutput
		mao := workflow.ActivityOptions{
			StartToCloseTimeout: activityTimeoutTierA,
			// docs/components/model-registry.md, "Resolved: Escalate-on-Retry"
			// — sized to match the number of language tiers (3) so the retry
			// ladder and the escalation ladder line up, rather than an
			// arbitrary retry count picked independently. model_call.py reads
			// its own attempt number to decide how many tiers to escalate.
			RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 3},
		}
		mctx := workflow.WithActivityOptions(cancelCtx, mao)
		modelInput := types.ModelCallInput{
			TurnID:             input.TurnID,
			ContextSeq:         contextSeq,
			HintModality:       hintModality,
			HintTier:           hintTier,
			OfferDeliveryTools: input.OfferDeliveryTools,
		}
		mcFuture := workflow.ExecuteActivity(mctx, "ModelCall", modelInput)

		// docs/components/gateway.md's "Resolved: ModelCall Streaming" —
		// only this turn's first call can ever stream (model_call.py's own
		// context_seq == 0 gate; iterations == 1 here is the Go-side
		// equivalent, checked before contextSeq's post-call increment
		// below). awaitModelCallWithStreaming's own doc comment has the
		// full reasoning for why this needs a merged Selector loop rather
		// than a plain Get() plus a detached consumer goroutine.
		streamPlatform := platformFromSessionKey(input.SessionKey)
		streamingEligible := iterations == 1 && input.ConnectionID != "" &&
			(streamPlatform == "discord" || streamPlatform == "discord-voice")

		var mcErr error
		if streamingEligible {
			mcErr = awaitModelCallWithStreaming(ctx, mcFuture, mctx, &mcOut, input.TurnID, input.ConnectionID, streamPlatform)
		} else {
			mcErr = mcFuture.Get(mctx, &mcOut)
		}
		if mcErr != nil {
			cancel()
			return failTurn(ctx, input.TurnID, input.SessionKey, input.ConnectionID, input.ParentType, mcErr, interrupts)
		}
		contextSeq++
		if mcOut.NextStep != nil {
			if mcOut.NextStep.Modality != "" || mcOut.NextStep.Tier != "" {
				hintModality, hintTier = mcOut.NextStep.Modality, mcOut.NextStep.Tier
			}
			// The model's own estimate raises the ceiling (never lowers it),
			// clamped hard. docs/components/turn-pipeline.md, "Iteration ceiling".
			if est := mcOut.NextStep.EstRemainingSteps; est > 0 {
				want := iterations + est*estStepMultiple
				if want > ceiling {
					if want > hardIterationCap {
						want = hardIterationCap
					}
					ceiling = want
					ceilingRaised = true
				}
			}
		}
		cumulativeTokens += mcOut.Usage.InputTokens + mcOut.Usage.OutputTokens
		metrics.WithTags(map[string]string{"direction": "input"}).Counter("model_call_tokens_total").Inc(int64(mcOut.Usage.InputTokens))
		metrics.WithTags(map[string]string{"direction": "output"}).Counter("model_call_tokens_total").Inc(int64(mcOut.Usage.OutputTokens))

		// docs/components/context-slot.md, "Resolved: Duties and Strategies"
		// #3 — evaluated fresh every iteration (not gated to "once per
		// turn" the way the old single-threshold check was): compaction
		// genuinely shrinks content, so if context is still over threshold
		// after one pass, triggering again is correct, not a bug.
		//
		// Found via live testing (context-slot.md's Notes Log): the soft
		// path used to be gated on the SAME context_tokens the hard path
		// checks, but that count is structurally capped at lcm's
		// VERBATIM_WINDOW_MESSAGES — it can sit under the soft threshold
		// indefinitely while real content silently falls out of the window
		// with no summary ever written to preserve it. Fix: the soft
		// (fire-and-forget) path no longer waits for a token threshold at
		// all — it fires every iteration, unconditionally, relying on
		// lcm.compact's own cheap "nothing new to compact" no-op (already
		// live-verified this session) the same way WriteMemory already
		// fires unconditionally every turn below, and the same "simplicity
		// over a wasted-call-cost pre-check" call context-slot.md already
		// made for session-start memory retrieval. Hard stays
		// threshold-gated and blocking — a genuinely separate concern
		// (protect the model's actual context window right now), correctly
		// proportional to context_window.
		if compressionState(mcOut.ContextTokens, mcOut.ContextWindow) == "hard" {
			// Blocks until compaction completes, so the *next* ModelCall in
			// this same turn assembles a smaller context.
			cctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA})
			_ = workflow.ExecuteActivity(cctx, "CompressContext", input.TurnID).Get(cctx, nil)

			// docs/components/memory-slot.md's "Resolved: Write-Path
			// Construction" correction — a real, threshold-crossing hard
			// compaction is one of the two boundaries agent-brain's own
			// write contract asks for (the other is session completion,
			// coordinator.go's idle-timeout exit). Deliberately NOT the
			// soft path above/below (it fires every iteration, usually a
			// cheap no-op — dispatching WriteMemory there would just
			// reintroduce near-per-turn granularity through a different
			// door). Detached child, same ABANDON reasoning as
			// WriteMemoryWorkflow's own doc comment — this turn keeps
			// running after dispatching it, doesn't wait for it.
			if input.ParentType == "session" {
				wcwo := workflow.ChildWorkflowOptions{
					WorkflowID:        input.TurnID + ":write-memory:" + strconv.Itoa(iterations),
					ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
				}
				wcctx := workflow.WithChildOptions(ctx, wcwo)
				wmFuture := workflow.ExecuteChildWorkflow(wcctx, WriteMemoryWorkflow, input.SessionKey)
				_ = wmFuture.GetChildWorkflowExecution().Get(wcctx, nil)
			}
		} else {
			// Fire-and-forget — detached child workflow, same reasoning as
			// the WriteMemory dispatch below. Per-iteration unique
			// WorkflowID since this can fire on more than one iteration
			// within the same turn.
			cwo := workflow.ChildWorkflowOptions{
				WorkflowID:        input.TurnID + ":compress-context:" + strconv.Itoa(iterations),
				ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
			}
			cctx := workflow.WithChildOptions(ctx, cwo)
			childFuture := workflow.ExecuteChildWorkflow(cctx, CompressContextWorkflow, input.TurnID)
			_ = childFuture.GetChildWorkflowExecution().Get(cctx, nil)
		}

		// --- No-progress guard (future-work.md §4). A step with no content and
		// no tool calls that still isn't "done" produced nothing actionable —
		// two running means the model is stuck (a report_status "working" call
		// gets peeled, so it reads as an empty step here). End the turn rather
		// than loop to the ceiling.
		if !mcOut.HasContent && len(mcOut.ToolCalls) == 0 && mcOut.Status != "done" {
			emptyStreak++
			if emptyStreak >= 2 {
				stopReason = "no_progress"
				cancel()
				break
			}
		} else {
			emptyStreak = 0
		}

		// --- Stop / continue on the model's declared status
		// (docs/components/turn-pipeline.md's output schema).
		if mcOut.Status == "done" {
			// A follow-up that landed before this boundary makes the model's
			// "done" stale — fold it in and keep looping rather than ending on
			// input the model hadn't seen.
			if len(pendingMessages) > 0 {
				nextMsg := pendingMessages[0]
				pendingMessages = pendingMessages[1:]
				ictx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA})
				if err := workflow.ExecuteActivity(ictx, "InsertMessage", types.InsertMessageInput{TurnID: input.TurnID, Message: nextMsg.Message}).Get(ictx, nil); err != nil {
					cancel()
					return failTurn(ctx, input.TurnID, input.SessionKey, input.ConnectionID, input.ParentType, err, interrupts)
				}
				cancel()
				continue
			}
			stopReason = "no_tool_calls"
			cancel()
			break
		}
		// docs/components/turn-pipeline.md's interrupt model — a "blocked"
		// status means the model needs the user. If it paired that with an
		// ask_user call, the fan-out below parks the turn on it. If a follow-up
		// already arrived, that IS the answer — fold it in and keep going. If
		// it's "blocked" with nothing to wait on, the model has said its piece
		// and can't proceed: deliver that message and end.
		if mcOut.Status == "blocked" {
			if len(pendingMessages) > 0 {
				nextMsg := pendingMessages[0]
				pendingMessages = pendingMessages[1:]
				ictx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA})
				if err := workflow.ExecuteActivity(ictx, "InsertMessage", types.InsertMessageInput{TurnID: input.TurnID, Message: nextMsg.Message}).Get(ictx, nil); err != nil {
					cancel()
					return failTurn(ctx, input.TurnID, input.SessionKey, input.ConnectionID, input.ParentType, err, interrupts)
				}
				cancel()
				continue
			}
			if len(mcOut.ToolCalls) == 0 {
				stopReason = "blocked"
				cancel()
				break
			}
			// else: falls through to the fan-out, which parks on ask_user.
		}
		// "working" / "blocked"-with-an-action: no tool calls this step means
		// the model is still reasoning — loop again; the iteration / retry /
		// budget ceilings bound it.
		if len(mcOut.ToolCalls) == 0 {
			cancel()
			continue
		}

		// --- Act: parallel fan-out over this reasoning step's tool calls ---
		// components/02-architecture-temporal-execution.md §4: siblings run
		// concurrently, not as a queue of independent workflows. IDs are
		// already minted by ModelCall — the workflow only reuses them.
		type pendingCall struct {
			toolCallID      string
			future          workflow.Future
			isSubagent      bool
			isApprovalGated bool
			isAskUser       bool
		}
		var calls []pendingCall

		for _, tc := range mcOut.ToolCalls {
			if tc.RequiresApproval {
				// docs/components/user-input.md — dispatched as a child
				// workflow (never a plain activity — an activity can't
				// durably block for up to UserInputRequestTimeout). An
				// approval request IS a user input request, not a separate
				// workflow type — ApprovalGatedCall is this call's opt-in use
				// of the same generic UserInputRequestWorkflow every other
				// consumer would use.
				cwo := workflow.ChildWorkflowOptions{
					WorkflowID:        tc.ToolCallID + ":approval",
					ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
				}
				cctx := workflow.WithChildOptions(cancelCtx, cwo)
				req := types.UserInputRequest{
					RequestID: tc.ToolCallID,
					TurnID:    input.TurnID,
					Kind:      "permission",
					Prompt:    "Approve calling " + tc.Server + "/" + tc.Tool + "?",
					Options: []types.UserInputOption{
						{ID: "approve", Label: "Approve"},
						{ID: "deny", Label: "Deny"},
					},
					Context: map[string]any{"server": tc.Server, "tool": tc.Tool, "tool_call_id": tc.ToolCallID},
				}
				fut := workflow.ExecuteChildWorkflow(cctx, UserInputRequestWorkflow, types.UserInputRequestWorkflowInput{
					Request:           req,
					ApprovalGatedCall: &types.ApprovalGatedCallSpec{ToolCallID: tc.ToolCallID, ToolName: tc.ToolName},
					SessionKey:        input.SessionKey,
					ConnectionID:      input.ConnectionID,
				})
				calls = append(calls, pendingCall{toolCallID: tc.ToolCallID, future: fut, isApprovalGated: true})
			} else if tc.IsSubagent {
				childInput := types.TurnInput{
					SessionKey: input.SessionKey,
					TurnID:     tc.ToolCallID, // subagent's turn_id IS its tool_call_id
					ParentType: "turn",
					ParentID:   input.TurnID,
				}
				cwo := workflow.ChildWorkflowOptions{
					WorkflowID:        tc.ToolCallID,
					ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL, // never TERMINATE — components/temporal-workflow.md
				}
				cctx := workflow.WithChildOptions(cancelCtx, cwo)
				fut := workflow.ExecuteChildWorkflow(cctx, TurnWorkflow, childInput)
				calls = append(calls, pendingCall{toolCallID: tc.ToolCallID, future: fut, isSubagent: true})
			} else if tc.IsAskUser {
				// docs/components/turn-pipeline.md — a child UserInputRequestWorkflow
				// (Kind "question"), parked on the same way an approval request is.
				// The loop then awaits it against a follow-up message just like any
				// other call: a real answer resolves the child (RequestUserInput
				// reads the question from tool_calls.arguments, CloseUserInput
				// writes the answer back into tool_calls.result); a message
				// arriving instead cancels it and folds the message in as the
				// answer. Never a bare .Get().
				cwo := workflow.ChildWorkflowOptions{
					WorkflowID:        tc.ToolCallID + ":ask",
					ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
				}
				cctx := workflow.WithChildOptions(cancelCtx, cwo)
				fut := workflow.ExecuteChildWorkflow(cctx, UserInputRequestWorkflow, types.UserInputRequestWorkflowInput{
					Request: types.UserInputRequest{
						RequestID:     tc.ToolCallID,
						TurnID:        input.TurnID,
						Kind:          "question",
						AllowFreeText: true,
						// Explicit non-nil — the model's real question/options
						// live in the ask_user tool_calls.arguments row and are
						// read there by RequestUserInput. These fields must still
						// serialize as [] / {}, never null: the Python activity's
						// UserInputRequest dataclass rejects a null for a
						// list[UserInputOption] / dict field ("Failed decoding
						// arguments").
						Options: []types.UserInputOption{},
						Context: map[string]any{},
					},
					SessionKey:   input.SessionKey,
					ConnectionID: input.ConnectionID,
				})
				calls = append(calls, pendingCall{toolCallID: tc.ToolCallID, future: fut, isAskUser: true})
			} else if deliveryActivityName, ok := deliveryToolActivity(platformFromSessionKey(input.SessionKey), tc.ToolName); ok {
				// deliver_reply/deliver_attachment — routed to the owning
				// gateway connection's own embedded worker, same task-queue
				// scheme Deliver/DeliverChunk/DeliverInterim already use,
				// not the generic tenant-worker ToolCall path (see
				// deliveryToolActivity's own doc comment).
				ao := workflow.ActivityOptions{
					ActivityID:          tc.ToolCallID,
					StartToCloseTimeout: activityTimeoutTierA,
					TaskQueue:           "deliver:discord:" + input.ConnectionID,
					RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
				}
				actx := workflow.WithActivityOptions(cancelCtx, ao)
				fut := workflow.ExecuteActivity(actx, deliveryActivityName, types.ToolCallInput{ToolCallID: tc.ToolCallID})
				calls = append(calls, pendingCall{toolCallID: tc.ToolCallID, future: fut})
			} else {
				timing := toolTimingFor(tc.ToolName)
				ao := workflow.ActivityOptions{
					ActivityID:          tc.ToolCallID,
					StartToCloseTimeout: timing.StartToCloseTimeout,
					// HeartbeatTimeout is what actually makes cancellation delivery
					// possible: the SDK core throttles the real network heartbeat to
					// roughly 80% of this value (capped separately), so it has to be
					// short relative to how long the activity actually runs —
					// otherwise the first real heartbeat carrying the cancellation
					// notice never lands before the activity finishes on its own.
					// Per-tool via toolTimingFor (tool_tiers.go) — a real Tier B tool
					// like shell_exec needs a much longer timeout than the fixture-only
					// demo tools' fast local timing.
					HeartbeatTimeout:    timing.HeartbeatTimeout,
					WaitForCancellation: true, // WAIT_CANCELLATION_COMPLETED, never ABANDON — see docs
					RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
				}
				actx := workflow.WithActivityOptions(cancelCtx, ao)
				fut := workflow.ExecuteActivity(actx, "ToolCall", types.ToolCallInput{ToolCallID: tc.ToolCallID})
				calls = append(calls, pendingCall{toolCallID: tc.ToolCallID, future: fut})
			}
		}

		// Event-driven, not timer-gated: the moment this step's tool calls
		// are actually dispatched (they're already durably minted in
		// Postgres by ModelCall, before this loop even runs), narrate it
		// immediately instead of waiting for runProgressWatchdog's backoff
		// to eventually say something generic. Same write, same delivery —
		// notifyProgress's own doc comment has the reasoning. Only every
		// reason-act step already bumps progressGen (below the Await), so
		// this doesn't fight the watchdog's own collapse-on-progress logic —
		// they're complementary triggers on the identical mechanism.
		if len(calls) > 0 {
			notifyProgress(ctx, input.TurnID, input.SessionKey, input.ConnectionID, "tool_dispatch", logger)
		}

		allReady := func() bool {
			for _, c := range calls {
				if !c.future.IsReady() {
					return false
				}
			}
			return true
		}

		// Wait for either all of this step's calls to settle, a follow-up
		// message to arrive, or a cancel — whichever happens first.
		_ = workflow.Await(ctx, func() bool {
			return allReady() || len(pendingMessages) > 0 || cancelRequested
		})

		if !allReady() {
			// --- Resolved: cooperative cancellation, not queue-after ---
			cancel()
			_ = workflow.Await(ctx, allReady) // wait for cancellation to actually settle — never ABANDON

			// Drain results so Temporal's futures are consumed (their status
			// is already durably recorded in tool_calls by the activities
			// themselves — nothing to fold into workflow memory).
			for _, c := range calls {
				drainResult(ctx, c.toolCallID, c.future, c.isSubagent, c.isApprovalGated, c.isAskUser)
			}
			// Even a cancelled subagent may have written files before its
			// interrupt landed — surface those to the parent's next
			// ModelCall (fold-in below) so the model can decide whether to
			// merge them, same reasoning as the ok-path dispatch further
			// down. See docs/components/session-filesystem.md, "Resolved:
			// Subagent Merge-Back Mechanics."
			var subagentIDs []string
			for _, c := range calls {
				if c.isSubagent {
					subagentIDs = append(subagentIDs, c.toolCallID)
				}
			}
			dispatchSubagentManifests(ctx, subagentIDs)

			if cancelRequested {
				// A stop, not new input — end here rather than folding
				// anything in and paying for another ModelCall. The
				// cancel() above already tore down this step's in-flight
				// tool calls (including a parked ask_user child, same
				// mechanism an ordinary interrupt already used).
				stopReason = "cancelled_by_user"
				break loop
			}

			// Dequeue exactly ONE pending message — never batch multiple
			// queued messages into a single fold-in (components/temporal-workflow.md,
			// Resolved: Signal Coalescing).
			next := pendingMessages[0]
			pendingMessages = pendingMessages[1:]
			iao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
			iactx := workflow.WithActivityOptions(ctx, iao)
			insertInput := types.InsertMessageInput{TurnID: input.TurnID, Message: next.Message}
			if err := workflow.ExecuteActivity(iactx, "InsertMessage", insertInput).Get(iactx, nil); err != nil {
				return failTurn(ctx, input.TurnID, input.SessionKey, input.ConnectionID, input.ParentType, err, interrupts)
			}

			// A mid-turn follow-up lands in the conversation (InsertMessage
			// above); the next ModelCall sees it and adapts. The old
			// reconcile-retrieval pass is gone (episode-lifecycle.md REVISION) —
			// real course-correction is the plan-and-execute phase's
			// per-checkpoint re-planning.
			continue loop
		}

		cancel()
		for _, c := range calls {
			status := drainResult(ctx, c.toolCallID, c.future, c.isSubagent, c.isApprovalGated, c.isAskUser)
			if status == "error" {
				retries++
			}
		}
		// Every subagent that ran this step gets a manifest activity
		// dispatched against its own turn_id, so the changed-file list is
		// folded into its tool_calls.result before the parent's next
		// ModelCall reads it (via lcm.assemble). See
		// docs/components/session-filesystem.md, "Resolved: Subagent
		// Merge-Back Mechanics."
		var subagentIDs []string
		for _, c := range calls {
			if c.isSubagent {
				subagentIDs = append(subagentIDs, c.toolCallID)
			}
		}
		dispatchSubagentManifests(ctx, subagentIDs)
	}

	// The reason-act loop is done — tell the watchdog goroutine to stop before
	// the (bounded) egress + delivery below, so it can't ping during teardown.
	turnDone = true

	metrics.Counter("turn_iterations_total").Inc(int64(iterations))
	metrics.Counter("turn_retries_total").Inc(int64(retries))
	metrics.WithTags(map[string]string{"stop_reason": stopReason}).Counter("turn_stop_reason_total").Inc(1)

	// docs/components/turn-pipeline.md, "Iteration ceiling" — estimate-vs-actual
	// is logged as a calibration signal (how well the model's report_status
	// est_remaining_steps tracked reality). hitCeiling is the case that matters:
	// the turn ran out of budget rather than finishing on its own.
	hitCeiling := stopReason == "max_iterations"
	logger.Info("iteration budget",
		"turn_id", input.TurnID, "iterations", iterations, "ceiling", ceiling,
		"ceiling_raised", ceilingRaised, "hit_ceiling", hitCeiling)
	if hitCeiling {
		metrics.Counter("turn_hit_iteration_ceiling_total").Inc(1)
	}

	// --- Egress: every turn (top-level or subagent) persists its own
	// turns.status — components/state-layer.md's read/write-split table
	// assigns that generically to "the persist activity" with no top-level
	// carve-out. Only Deliver (external gateway send) is top-level-only: a
	// subagent has no external delivery target, its result is read from
	// Postgres by its parent's next ModelCall instead.
	{
		finalStatus := "completed"
		if stopReason == "cancelled_by_user" {
			// A real, distinct terminal status — not "completed" — so a
			// client (mobile/web's shared isTerminal()/turn_end.status
			// already accept "cancelled") can tell "the agent finished" from
			// "the user stopped it" apart.
			finalStatus = "cancelled"
		}
		ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
		actx := workflow.WithActivityOptions(ctx, ao)
		_ = workflow.ExecuteActivity(actx, "Persist", input.TurnID, finalStatus).Get(actx, nil)
	}
	// docs/components/memory-slot.md's "Resolved: Write-Path Construction"
	// correction (2026-08-29): WriteMemory no longer dispatches here, once
	// per top-level turn — agent-brain's own write contract asks for
	// session-completion and context-compaction boundaries instead
	// (coordinator.go's idle-timeout exit, and the hard-compression branch
	// above), not per turn. Removing this per-turn dispatch is the actual
	// fix for the gap that correction named; nothing replaces it here.
	var interruptedPayload *types.SignalPayload
	if input.ParentType == "session" {
		var deliverErr error
		interruptedPayload, deliverErr = deliverConnectionBased(ctx, interrupts, input.SessionKey, input.ConnectionID, input.TurnID)
		if deliverErr != nil {
			runDiscordDeliveryRecovery(ctx, input.TurnID, input.ConnectionID, contextSeq)
		}
	}

	logger.Info("turn workflow complete", "turn_id", input.TurnID, "stop_reason", stopReason, "iterations", iterations, "interrupted_during_delivery", interruptedPayload != nil)
	return types.TurnResult{TurnID: input.TurnID, StopReason: stopReason, Iterations: iterations, InterruptedDuringDelivery: interruptedPayload}, nil
}

// dispatchSubagentManifests fans out one SubagentManifest activity per
// completed subagent turn and waits for all of them to finish before
// returning — the parent's next ModelCall reads each subagent's
// tool_calls.result via lcm.assemble, so the manifest must be written
// before the next iteration's ModelCall runs (or, in the interrupt path,
// before InsertMessage folds in the follow-up and the next iteration
// starts). Tier A: just a directory walk and a single UPDATE.
// docs/components/session-filesystem.md, "Resolved: Subagent Merge-Back
// Mechanics."
func dispatchSubagentManifests(ctx workflow.Context, subagentIDs []string) {
	if len(subagentIDs) == 0 {
		return
	}
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)
	futures := make([]workflow.Future, 0, len(subagentIDs))
	for _, id := range subagentIDs {
		futures = append(futures, workflow.ExecuteActivity(actx, "SubagentManifest", id))
	}
	// Fold errors into the log rather than failing the whole turn — a
	// missing manifest degrades the parent's model to the same "no
	// changed-file info" state it'd see for a subagent that wrote nothing,
	// not a turn-ending failure.
	for i, f := range futures {
		if err := f.Get(actx, nil); err != nil {
			workflow.GetLogger(ctx).Warn("SubagentManifest failed", "subagent_turn_id", subagentIDs[i], "error", err)
		}
	}
}

// drainResult calls Get on an already-ready (or now-cancelled) future purely
// to consume it and learn the outcome status — never to extract content. For
// a plain tool call, status comes from ToolCallOutput.Status (Temporal-level
// success) or, on cancellation/error, is inferred from the error itself; the
// real, durable status/result/reason/side_effect already live in the
// tool_calls row, written by the ToolCall activity itself. For a subagent,
// status is inferred the same way from TurnResult/error — its actual content
// lives in Postgres under its own turn_id, same as any other turn.
func drainResult(ctx workflow.Context, toolCallID string, f workflow.Future, isSubagent bool, isApprovalGated bool, isAskUser bool) string {
	if isSubagent {
		var subResult types.TurnResult
		if err := f.Get(ctx, &subResult); err != nil {
			return statusFromError(err)
		}
		return "ok"
	}
	if isAskUser {
		// The child UserInputRequestWorkflow already wrote the ask_user
		// tool_calls row (answer, or cancelled) via CloseUserInput — its own
		// return value carries no ToolCallOutput. A workflow-level error here
		// is just its cancellation (a follow-up message pre-empted the wait);
		// that's a resolution, not a turn retry.
		var out types.UserInputRequestWorkflowOutput
		if err := f.Get(ctx, &out); err != nil {
			return "cancelled"
		}
		return "ok"
	}
	if isApprovalGated {
		// docs/components/user-input.md — UserInputRequestWorkflow's result
		// wraps the real ToolCallOutput (set whenever ApprovalGatedCall was
		// on the input, which it always is for this dispatch path); a
		// workflow-level error here means the workflow itself failed before
		// producing any output at all (e.g. a genuine cancellation that
		// short-circuited before ToolCallOutput was ever assigned).
		var out types.UserInputRequestWorkflowOutput
		if err := f.Get(ctx, &out); err != nil {
			return statusFromError(err)
		}
		if out.ToolCallOutput != nil {
			return out.ToolCallOutput.Status
		}
		return "cancelled"
	}
	var out types.ToolCallOutput
	if err := f.Get(ctx, &out); err != nil {
		return statusFromError(err)
	}
	return out.Status
}

func statusFromError(err error) string {
	var canceledErr *temporal.CanceledError
	if errors.As(err, &canceledErr) {
		return "cancelled"
	}
	return "error"
}
