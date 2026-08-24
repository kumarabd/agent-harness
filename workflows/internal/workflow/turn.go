package workflow

import (
	"errors"
	"strconv"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// NewMessageSignalName is the signal channel a running Turn Workflow listens on
// for follow-up messages forwarded by the Session Coordinator
// (02-architecture-temporal-execution.md §3).
const NewMessageSignalName = "NewMessage"

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
// succeeded. TurnWorkflow starts THIS workflow as a detached child
// (ParentClosePolicy: ABANDON) and does not await its result — the child
// keeps running independently after the parent closes, so the activity's
// completion gets recorded against the child's own still-open history
// instead.
func WriteMemoryWorkflow(ctx workflow.Context, turnID string) error {
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)
	return workflow.ExecuteActivity(actx, "WriteMemory", turnID).Get(actx, nil)
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
	maxIterations = 20        // components/temporal-workflow.md, Resolved: Stop-Condition Default Values
	maxRetries    = 5         // turn-level cumulative cap, distinct from per-activity MaximumAttempts
	budgetTokens  = 2_000_000 // high placeholder ceiling, not infinite — see resolved doc; turn-local API spend, unrelated to context size below

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
func failTurn(ctx workflow.Context, turnID string, parentType string, cause error) (types.TurnResult, error) {
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
		_ = workflow.ExecuteActivity(actx, "Deliver", turnID).Get(actx, nil)
	}
	return types.TurnResult{}, cause
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
	// docs/components/model-registry.md, "Resolved: Selection Mechanism" —
	// empty on the first iteration (bootstrap default supplied Python-side
	// by model_registry.default_hint(), not duplicated here), then copied
	// verbatim from each ModelCallOutput's own next-step hint. This
	// workflow never interprets these values, just passes them through.
	hintModality, hintTier := "", ""

	// --- Start-of-turn: write the inbound message (or, for a subagent, let
	// InsertMessage derive its kickoff content from its own tool_calls row)
	// before the first ModelCall — ModelCall's first read needs this content
	// already in Postgres (components/temporal-workflow.md, "Resolved:
	// Reference/ID Schema"). This also creates the turns row.
	{
		ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
		actx := workflow.WithActivityOptions(ctx, ao)
		insertInput := types.InsertMessageInput{
			TurnID:      input.TurnID,
			Message:     input.InitialMessage,
			IsTurnStart: true,
			ParentID:    input.ParentID,
			ParentType:  input.ParentType,
			TurnSeq:     input.TurnSeq,
		}
		if err := workflow.ExecuteActivity(actx, "InsertMessage", insertInput).Get(actx, nil); err != nil {
			return failTurn(ctx, input.TurnID, input.ParentType, err)
		}
	}

	// Deterministic FIFO queue for follow-up messages, per components/temporal-workflow.md
	// "Resolved: Signal Coalescing" — the handler only appends (pure, deterministic
	// under replay); dequeue-and-fold-one happens explicitly at loop boundaries below,
	// never batched.
	var pendingMessages []types.SignalPayload
	signalChan := workflow.GetSignalChannel(ctx, NewMessageSignalName)
	workflow.Go(ctx, func(gctx workflow.Context) {
		for {
			var payload types.SignalPayload
			signalChan.Receive(gctx, &payload)
			pendingMessages = append(pendingMessages, payload)
		}
	})

	var stopReason string

loop:
	for {
		// --- Resolved: Stop-Condition Logic (inline check, pure read of local state) ---
		if iterations >= maxIterations {
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
			TurnID:       input.TurnID,
			ContextSeq:   contextSeq,
			HintModality: hintModality,
			HintTier:     hintTier,
		}
		if err := workflow.ExecuteActivity(mctx, "ModelCall", modelInput).Get(mctx, &mcOut); err != nil {
			cancel()
			return failTurn(ctx, input.TurnID, input.ParentType, err)
		}
		contextSeq++
		hintModality, hintTier = mcOut.NextHintModality, mcOut.NextHintTier
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

		if !mcOut.HasToolCalls {
			stopReason = "no_tool_calls"
			cancel()
			break
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

		allReady := func() bool {
			for _, c := range calls {
				if !c.future.IsReady() {
					return false
				}
			}
			return true
		}

		// Wait for either all of this step's calls to settle, or a follow-up
		// message to arrive — whichever happens first.
		_ = workflow.Await(ctx, func() bool {
			return allReady() || len(pendingMessages) > 0
		})

		if !allReady() {
			// --- Resolved: cooperative cancellation, not queue-after ---
			cancel()
			_ = workflow.Await(ctx, allReady) // wait for cancellation to actually settle — never ABANDON

			// Drain results so Temporal's futures are consumed (their status
			// is already durably recorded in tool_calls by the activities
			// themselves — nothing to fold into workflow memory).
			for _, c := range calls {
				drainResult(ctx, c.toolCallID, c.future, c.isSubagent, c.isApprovalGated)
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
				return failTurn(ctx, input.TurnID, input.ParentType, err)
			}
			continue loop
		}

		cancel()
		for _, c := range calls {
			status := drainResult(ctx, c.toolCallID, c.future, c.isSubagent, c.isApprovalGated)
			if status == "error" {
				retries++
			}
		}
	}

	metrics.Counter("turn_iterations_total").Inc(int64(iterations))
	metrics.Counter("turn_retries_total").Inc(int64(retries))
	metrics.WithTags(map[string]string{"stop_reason": stopReason}).Counter("turn_stop_reason_total").Inc(1)

	// --- Egress: every turn (top-level or subagent) persists its own
	// turns.status — components/state-layer.md's read/write-split table
	// assigns that generically to "the persist activity" with no top-level
	// carve-out. Only Deliver (external gateway send) is top-level-only: a
	// subagent has no external delivery target, its result is read from
	// Postgres by its parent's next ModelCall instead.
	{
		ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
		actx := workflow.WithActivityOptions(ctx, ao)
		_ = workflow.ExecuteActivity(actx, "Persist", input.TurnID, "completed").Get(actx, nil)
	}
	if input.ParentType == "session" {
		// docs/components/memory-slot.md, "Resolved: Write-Path Construction"
		// + "Resolved: Subagent-Turn Write Scope" — top-level turns only,
		// genuinely fire-and-forget. Started as a DETACHED CHILD WORKFLOW
		// (ParentClosePolicy: ABANDON), not a bare ExecuteActivity — a bare
		// unawaited activity is NOT reliably fire-and-forget when the
		// calling workflow (this one) closes moments later: the activity's
		// completion gets reported against an already-closed workflow and is
		// silently discarded, leaving it stuck showing as pending forever
		// even though the real work succeeded (confirmed via a live test —
		// see WriteMemoryWorkflow's own doc comment). The child keeps
		// running independently after this workflow closes, so its
		// completion is recorded against its own still-open history
		// instead. Only waits for the child to have STARTED
		// (GetChildWorkflowExecution — a real, documented two-phase future,
		// not this workflow's own invention), not for it to finish.
		cwo := workflow.ChildWorkflowOptions{
			WorkflowID:        input.TurnID + ":write-memory",
			ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
		}
		cctx := workflow.WithChildOptions(ctx, cwo)
		childFuture := workflow.ExecuteChildWorkflow(cctx, WriteMemoryWorkflow, input.TurnID)
		_ = childFuture.GetChildWorkflowExecution().Get(cctx, nil)
	}
	if input.ParentType == "session" {
		ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
		actx := workflow.WithActivityOptions(ctx, ao)
		_ = workflow.ExecuteActivity(actx, "Deliver", input.TurnID).Get(actx, nil)
	}

	logger.Info("turn workflow complete", "turn_id", input.TurnID, "stop_reason", stopReason, "iterations", iterations)
	return types.TurnResult{TurnID: input.TurnID, StopReason: stopReason, Iterations: iterations}, nil
}

// drainResult calls Get on an already-ready (or now-cancelled) future purely
// to consume it and learn the outcome status — never to extract content. For
// a plain tool call, status comes from ToolCallOutput.Status (Temporal-level
// success) or, on cancellation/error, is inferred from the error itself; the
// real, durable status/result/reason/side_effect already live in the
// tool_calls row, written by the ToolCall activity itself. For a subagent,
// status is inferred the same way from TurnResult/error — its actual content
// lives in Postgres under its own turn_id, same as any other turn.
func drainResult(ctx workflow.Context, toolCallID string, f workflow.Future, isSubagent bool, isApprovalGated bool) string {
	if isSubagent {
		var subResult types.TurnResult
		if err := f.Get(ctx, &subResult); err != nil {
			return statusFromError(err)
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
