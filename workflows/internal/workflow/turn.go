package workflow

import (
	"errors"
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

const (
	maxIterations         = 20        // components/temporal-workflow.md, Resolved: Stop-Condition Default Values
	maxRetries            = 5         // turn-level cumulative cap, distinct from per-activity MaximumAttempts
	budgetTokens          = 2_000_000 // high placeholder ceiling, not infinite — see resolved doc
	compressionGateTokens = 1_500_000 // inline check threshold; compression activity itself is a stub in this slice

	// activityTimeoutTierA matches the stub ModelCall/InsertMessage/Persist/Deliver
	// activities' shape: sub-2s, fire-and-complete, no heartbeat — Tier A per
	// components/activities-outbound-delivery.md. Real Tier B/C tuning is
	// deliberately deferred (components/temporal-workflow.md).
	activityTimeoutTierA = 30 * time.Second
)

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
	compressed := false

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
			return types.TurnResult{}, err
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

		// --- Resolved: Compression / Context Management (inline gate, activity-backed action) ---
		if !compressed && cumulativeTokens >= compressionGateTokens {
			cctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA})
			_ = workflow.ExecuteActivity(cctx, "CompressContext", input.TurnID).Get(cctx, nil)
			compressed = true
		}

		iterations++
		cancelCtx, cancel := workflow.WithCancel(ctx)

		// --- Reason: model-call activity (mints tool_call_id/subagent IDs
		// itself, writes its own response to Postgres, returns refs only) ---
		var mcOut types.ModelCallOutput
		mao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
		mctx := workflow.WithActivityOptions(cancelCtx, mao)
		modelInput := types.ModelCallInput{TurnID: input.TurnID, ContextSeq: contextSeq}
		if err := workflow.ExecuteActivity(mctx, "ModelCall", modelInput).Get(mctx, &mcOut); err != nil {
			cancel()
			return types.TurnResult{}, err
		}
		contextSeq++
		cumulativeTokens += mcOut.Usage.InputTokens + mcOut.Usage.OutputTokens
		metrics.WithTags(map[string]string{"direction": "input"}).Counter("model_call_tokens_total").Inc(int64(mcOut.Usage.InputTokens))
		metrics.WithTags(map[string]string{"direction": "output"}).Counter("model_call_tokens_total").Inc(int64(mcOut.Usage.OutputTokens))

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
			toolCallID string
			future     workflow.Future
			isSubagent bool
		}
		var calls []pendingCall

		for _, tc := range mcOut.ToolCalls {
			if tc.IsSubagent {
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
				drainResult(ctx, c.toolCallID, c.future, c.isSubagent)
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
				return types.TurnResult{}, err
			}
			continue loop
		}

		cancel()
		for _, c := range calls {
			status := drainResult(ctx, c.toolCallID, c.future, c.isSubagent)
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
func drainResult(ctx workflow.Context, toolCallID string, f workflow.Future, isSubagent bool) string {
	if isSubagent {
		var subResult types.TurnResult
		if err := f.Get(ctx, &subResult); err != nil {
			return statusFromError(err)
		}
		return "ok"
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
