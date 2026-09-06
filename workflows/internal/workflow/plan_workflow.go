package workflow

import (
	"fmt"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// PlanDoneSignalName — the PlanWorkflow tells the session coordinator its
// task-run is finished (or abandoned), so the coordinator can drop its
// active-work guard and idle / start the next thing. Root plan only — a nested
// plan's completion reaches its parent via the child-workflow future.
const PlanDoneSignalName = "PlanDone"

// PlanAbandonSignalName — dispatch.go signals a running PlanWorkflow to wrap up
// early when a new, unrelated task supersedes it.
const PlanAbandonSignalName = "abandon"

// planApprovalRevisionCap — how many times the user may send the plan back for
// revision before the gate stops asking and proceeds with the current draft.
const planApprovalRevisionCap = 3

// maxPlanDepth — 3C-iii recursion cap. The root is depth 0; a checkpoint at
// this depth runs as a flat turn even if flagged complex (it can still spawn
// subagents, so the work still gets done — it just isn't its own plan).
const maxPlanDepth = 2

// PlanWorkflow — docs/components/request-pipeline/08-planning.md (plan-and-execute).
//
// The orchestrator for one Deliberate task-run. Started by the dispatch helper
// (dispatch.go) as the root, or by another PlanWorkflow for a complex checkpoint
// (3C-iii); workflow id "<plan_id>:plan".
//
//  1. Planning turn — turn_id == plan_id, PlanningMode: one ModelCall, planning
//     system prompt, `propose_plan` tool. Writes PLAN.md and ends.
//  2. Approval gate (root only) — if the planning turn's result carries
//     NeedsApproval, park on a UserInputRequestWorkflow until the user approves
//     / revises / rejects. Auto-proceed otherwise.
//  3. Execution loop — at each checkpoint boundary: fold in any mid-plan
//     follow-up, then NextCheckpoint → a flat checkpoint TurnWorkflow (marks
//     itself terminal via `checkpoint_done`) OR, for a `complex` checkpoint, a
//     nested PlanWorkflow (this workflow marks the checkpoint done on its
//     return). Repeat until all terminal, `abandon`, or the checkpoint cap.
//  4. Close — root: dispatch RecordSkill over the whole tree + signal the
//     coordinator (PlanDone). Nested: just return (the parent is waiting).
func PlanWorkflow(ctx workflow.Context, input types.PlanWorkflowInput) error {
	logger := workflow.GetLogger(ctx)
	isRoot := input.ParentPlanID == ""
	logger.Info("plan workflow started", "plan_id", input.PlanID, "root", isRoot, "depth", input.Depth)

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: activityTimeoutTierA,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	}
	actx := workflow.WithActivityOptions(ctx, ao)

	// Follow-ups + abandon, drained into vars at every wait point. wakeCh (size
	// 1, coalescing) lets a wait that's blocked on a child workflow notice that
	// one of them changed — the loop-boundary checks read the vars directly.
	pending := &[]types.SignalPayload{}
	abandoned := false
	wakeCh := workflow.NewBufferedChannel(ctx, 1)
	msgChan := workflow.GetSignalChannel(ctx, NewMessageSignalName)
	workflow.Go(ctx, func(gctx workflow.Context) {
		for {
			var p types.SignalPayload
			msgChan.Receive(gctx, &p)
			*pending = append(*pending, p)
			wakeCh.SendAsync(true)
		}
	})
	abandonChan := workflow.GetSignalChannel(ctx, PlanAbandonSignalName)
	workflow.Go(ctx, func(gctx workflow.Context) {
		var supersededBy string
		abandonChan.Receive(gctx, &supersededBy)
		abandoned = true
		wakeCh.SendAsync(true)
		logger.Info("plan abandon signal received", "plan_id", input.PlanID, "superseded_by", supersededBy)
	})

	initiatedBy := input.InitiatedBy
	if initiatedBy == "" {
		initiatedBy = "user"
	}
	task := input.Task

	// --- 1. planning turn (turn_id == plan_id, PlanningMode) ----------------
	planTurnInput := types.TurnInput{
		SessionKey:   input.SessionKey,
		TurnID:       input.PlanID,
		ParentType:   "plan",
		ParentID:     input.PlanID,
		ConnectionID: input.ConnectionID,
		InitiatedBy:  initiatedBy,
		PreInserted:  input.SeedText == "", // root: dispatch.go inserted turn:1
		PlanningMode: true,
		PlanID:       input.PlanID,
		Task:         &task,
	}
	if input.SeedText != "" {
		planTurnInput.InitialMessage = types.Message{Role: "user", Content: input.SeedText}
	}
	planRes, err := runChildTurn(ctx, input.PlanID, planTurnInput)
	if err != nil {
		return fmt.Errorf("planning turn %s: %w", input.PlanID, err)
	}

	// --- 2. approval gate (root only) ------------------------------------
	closeReason := "plan_complete"
	if isRoot {
		proceed, gateReason := runApprovalGate(ctx, input, task, initiatedBy, planRes.NeedsApproval)
		if !proceed {
			logger.Info("plan not approved — wrapping up", "plan_id", input.PlanID, "reason", gateReason)
			finishPlan(ctx, input, task, 0, gateReason)
			return nil
		}
	}
	// Show the plan. When approval was required, the last approval-gate round
	// already showed it (the rendered plan rides inline in that prompt —
	// propose_plan never writes to the planning turn's own messages row, so a
	// generic Deliver(input.PlanID) here would show nothing); only the
	// auto-proceed path (no approval needed) still needs an explicit Deliver.
	if isRoot && !planRes.NeedsApproval {
		_ = workflow.ExecuteActivity(actx, "Deliver", input.PlanID).Get(ctx, nil)
	}

	// --- 3. execution loop ---------------------------------------------
	cpN := 0
	handlingN := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if abandoned {
			closeReason = "superseded"
			break
		}

		// Fold in any mid-plan follow-up before picking the next checkpoint.
		if len(*pending) > 0 {
			handlingN++
			foldInFollowups(ctx, input, task, pending, handlingN)
		}

		var next types.NextCheckpointResult
		if err := workflow.ExecuteActivity(actx, "NextCheckpoint", input.PlanID).Get(ctx, &next); err != nil {
			return err
		}
		if !next.HasNext {
			break
		}

		cpN++
		if err := runCheckpoint(ctx, input, cpN, next, task, pending, &abandoned, wakeCh); err != nil {
			return err
		}

		if cpN > maxIterations {
			logger.Error("plan exceeded checkpoint cap", "plan_id", input.PlanID)
			closeReason = "checkpoint_cap"
			break
		}
	}

	// --- 4. close ----------------------------------------------------
	finishPlan(ctx, input, task, cpN, closeReason)
	logger.Info("plan workflow complete", "plan_id", input.PlanID, "checkpoints", cpN, "close_reason", closeReason, "root", isRoot)
	return nil
}

// runCheckpoint runs one checkpoint — a flat TurnWorkflow, or (if the planning
// model flagged it `complex` and we're under the depth cap) a nested
// PlanWorkflow whose completion this function marks the checkpoint done for.
func runCheckpoint(
	ctx workflow.Context,
	input types.PlanWorkflowInput,
	cpN int,
	next types.NextCheckpointResult,
	task types.TaskRepresentation,
	pending *[]types.SignalPayload,
	abandoned *bool,
	wakeCh workflow.Channel,
) error {
	logger := workflow.GetLogger(ctx)
	recurse := next.Complex && input.Depth+1 <= maxPlanDepth

	if !recurse {
		cpTurnID := fmt.Sprintf("%s:cp:%d", input.PlanID, cpN)
		cpInput := types.TurnInput{
			SessionKey:     input.SessionKey,
			TurnID:         cpTurnID,
			ParentType:     "plan",
			ParentID:       input.PlanID,
			ConnectionID:   input.ConnectionID,
			InitiatedBy:    "plan",
			InitialMessage: types.Message{Role: "user", Content: next.SeedText},
			PlanID:         input.PlanID,
		}
		if _, err := runChildTurn(ctx, cpTurnID, cpInput); err != nil {
			return fmt.Errorf("checkpoint turn %s: %w", cpTurnID, err)
		}
		logger.Info("checkpoint executed (flat)", "plan_id", input.PlanID, "cp", next.CheckpointID, "turn", cpTurnID)
		return nil // the flat turn marked itself via checkpoint_done
	}

	// Nested plan for a complex checkpoint (3C-iii).
	subPlanID := fmt.Sprintf("%s:cp:%d:sub", input.PlanID, cpN)
	subInput := types.PlanWorkflowInput{
		PlanID:       subPlanID,
		SessionKey:   input.SessionKey,
		ConnectionID: input.ConnectionID,
		InitiatedBy:  "plan",
		Task:         task,
		ParentPlanID: input.PlanID,
		Depth:        input.Depth + 1,
		SeedText:     next.SeedText,
	}
	interrupted, err := runNestedPlan(ctx, subPlanID, subInput, pending, abandoned, wakeCh)
	if err != nil {
		return fmt.Errorf("nested plan %s: %w", subPlanID, err)
	}
	if interrupted {
		// Left the checkpoint pending on purpose — the loop folds in the
		// follow-up (or breaks on abandon) and re-reads the ledger.
		logger.Info("nested plan interrupted", "plan_id", input.PlanID, "cp", next.CheckpointID)
		return nil
	}
	// Merge-back: the nested plan has no checkpoint_done of its own.
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 3}}
	_ = workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, ao), "MarkCheckpointDone", input.PlanID, next.CheckpointID).Get(ctx, nil)
	logger.Info("checkpoint executed (nested plan)", "plan_id", input.PlanID, "cp", next.CheckpointID, "sub", subPlanID)
	return nil
}

// runNestedPlan starts a child PlanWorkflow and waits for it, but stays
// interruptible: if a follow-up arrives or `abandon` fires while the child
// runs, it cancels the child and returns interrupted=true. Depth-capped
// recursion + the child's ABANDON policy keep the tree bounded.
func runNestedPlan(
	ctx workflow.Context,
	subPlanID string,
	subInput types.PlanWorkflowInput,
	pending *[]types.SignalPayload,
	abandoned *bool,
	wakeCh workflow.Channel,
) (bool, error) {
	childCtx, cancelChild := workflow.WithCancel(ctx)
	defer cancelChild()
	cwo := workflow.ChildWorkflowOptions{
		WorkflowID:        subPlanID + ":plan",
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
	}
	fut := workflow.ExecuteChildWorkflow(workflow.WithChildOptions(childCtx, cwo), PlanWorkflow, subInput)

	done := false
	var childErr error
	for !done {
		sel := workflow.NewSelector(ctx)
		sel.AddFuture(fut, func(f workflow.Future) {
			childErr = f.Get(ctx, nil)
			done = true
		})
		sel.AddReceive(wakeCh, func(c workflow.ReceiveChannel, _ bool) {
			var v bool
			c.Receive(ctx, &v)
		})
		sel.Select(ctx)
		if done {
			break
		}
		if len(*pending) > 0 || *abandoned {
			cancelChild()
			_ = fut.Get(ctx, nil) // let the cancellation settle
			return true, nil
		}
	}
	return false, childErr
}

// runPlanPresentationTurn seeds a normal reason-act turn whose job is to show
// the current plan to the user before runApprovalGate opens its approve/
// reject prompt — the plan text itself is never Go-rendered or embedded in a
// UserInputRequest.Prompt; the model reads it (prompt.assemble already
// splices the current PLAN.md into any plan-owned turn's prompt via
// plan.render_block) and delivers it via deliver_reply/deliver_attachment,
// its own call on how (informed by skills/seeds/deliver-long-content.json —
// docs/components/activities-outbound-delivery.md's model-driven retry
// philosophy applied to delivery itself). Same runChildTurn shape as
// foldInFollowups/runRejectionNoticeTurn; a failure here is best-effort —
// logs and moves on rather than blocking the approval gate from opening.
func runPlanPresentationTurn(ctx workflow.Context, input types.PlanWorkflowInput, task types.TaskRepresentation, round int) {
	logger := workflow.GetLogger(ctx)
	turnID := fmt.Sprintf("%s:present:%d", input.PlanID, round)
	in := types.TurnInput{
		SessionKey:   input.SessionKey,
		TurnID:       turnID,
		ParentType:   "plan",
		ParentID:     input.PlanID,
		ConnectionID: input.ConnectionID,
		InitiatedBy:  "plan",
		PlanID:       input.PlanID,
		Task:         &task,
		InitialMessage: types.Message{
			Role: "user",
			Content: "The plan above needs to be presented to the user before they can approve or " +
				"reject it. Deliver it to them now.",
		},
		OfferDeliveryTools: true,
	}
	if _, err := runChildTurn(ctx, turnID, in); err != nil {
		logger.Warn("plan presentation turn failed — approval gate will open without it", "plan_id", input.PlanID, "round", round, "error", err)
	}
}

// runApprovalGate returns (proceed, closeReason). It's a no-op unless the
// planning turn's propose_plan set needs_approval. proceed=false means the user
// rejected the plan (or ignored the request until it expired) — the caller
// wraps up without executing anything. A free-text answer re-runs the planning
// turn with the user's feedback and re-gates, up to planApprovalRevisionCap
// rounds, after which it proceeds with whatever draft stands.
func runApprovalGate(ctx workflow.Context, input types.PlanWorkflowInput, task types.TaskRepresentation, initiatedBy string, needsApproval bool) (bool, string) {
	if !needsApproval {
		return true, ""
	}
	logger := workflow.GetLogger(ctx)

	for round := 1; ; round++ {
		// Present the plan fresh each round — a revision (below) changes the
		// ledger, and the re-gate must show the updated draft, not the stale
		// one from round 1. A real turn, not a Go-rendered string: it reads
		// the current PLAN.md via prompt.assemble's own plan section (already
		// spliced into any plan-owned turn's prompt) and delivers it itself
		// via deliver_reply/deliver_attachment — the model's own judgment on
		// how, informed by skills/seeds/deliver-long-content.json, not a
		// mechanical length-check-then-embed.
		runPlanPresentationTurn(ctx, input, task, round)

		reqID := fmt.Sprintf("%s:approval:%d", input.PlanID, round)
		cwo := workflow.ChildWorkflowOptions{
			WorkflowID:        reqID,
			ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
		}
		req := types.UserInputRequest{
			RequestID:     reqID,
			TurnID:        input.PlanID,
			Kind:          "plan_approval",
			Prompt:        "Approve the plan above to start, or send it back with changes.",
			Options:       []types.UserInputOption{{ID: "approve", Label: "Approve"}, {ID: "reject", Label: "Reject"}},
			AllowFreeText: true, // free text = a revision instruction
			Context:       map[string]any{"plan_id": input.PlanID, "round": round},
		}
		var out types.UserInputRequestWorkflowOutput
		err := workflow.ExecuteChildWorkflow(workflow.WithChildOptions(ctx, cwo), UserInputRequestWorkflow, types.UserInputRequestWorkflowInput{
			Request:      req,
			SessionKey:   input.SessionKey,
			ConnectionID: input.ConnectionID,
		}).Get(ctx, &out)
		if err != nil {
			// An unrelated message interrupted the wait (cancelled the child
			// workflow) — "don't execute", same fail-closed stance permission
			// gating takes on a cancelled approval, but distinct from an
			// explicit reject: the interrupting message already IS the
			// user's next word, so no cancellation notice or follow-up
			// intention here — that would talk over what they just sent.
			logger.Info("plan approval request cancelled", "plan_id", input.PlanID, "error", err)
			return false, "cancelled"
		}

		decision := ""
		if out.Response.SelectedOptionID != nil {
			decision = *out.Response.SelectedOptionID
		}
		feedback := ""
		if out.Response.FreeText != nil {
			feedback = *out.Response.FreeText
		}

		switch {
		case decision == "approve":
			return true, ""
		case decision == "reject" && feedback == "":
			return false, "rejected"
		case feedback == "":
			// No selection and no text — the request expired. Fail closed.
			logger.Info("plan approval expired with no response", "plan_id", input.PlanID)
			return false, "expired"
		}

		// A revision instruction (with or without an explicit option). Re-plan
		// with the feedback, then re-gate — unless we've hit the cap.
		if round >= planApprovalRevisionCap {
			logger.Info("plan revision cap reached — proceeding with current draft", "plan_id", input.PlanID)
			return true, ""
		}
		replanID := fmt.Sprintf("%s:replan:%d", input.PlanID, round)
		replanInput := types.TurnInput{
			SessionKey:     input.SessionKey,
			TurnID:         replanID,
			ParentType:     "plan",
			ParentID:       input.PlanID,
			ConnectionID:   input.ConnectionID,
			InitiatedBy:    initiatedBy,
			PlanningMode:   true,
			PlanID:         input.PlanID,
			Task:           &task,
			InitialMessage: types.Message{Role: "user", Content: "Revise the plan per this feedback, then re-propose it in full:\n\n" + feedback},
		}
		if _, err := runChildTurn(ctx, replanID, replanInput); err != nil {
			logger.Warn("re-plan turn failed — proceeding with the standing draft", "plan_id", input.PlanID, "error", err)
			return true, ""
		}
		// No Deliver(replanID) here — the next round's runPlanPresentationTurn
		// call re-reads PLAN.md fresh and presents the revised draft itself.
	}
}

// foldInFollowups drains `pending` and runs one PlanHandling turn seeded with
// the follow-up text. That turn is a normal reason-act turn (it can answer the
// user and use tools) that also has `propose_plan` — so it may re-shape the
// still-pending checkpoints. Its output is delivered to the user.
func foldInFollowups(ctx workflow.Context, input types.PlanWorkflowInput, task types.TaskRepresentation, pending *[]types.SignalPayload, n int) {
	logger := workflow.GetLogger(ctx)
	msgs := *pending
	*pending = nil
	if len(msgs) == 0 {
		return
	}

	combined := "The user sent this while the plan is executing:\n\n"
	for i, m := range msgs {
		if i > 0 {
			combined += "\n\n"
		}
		combined += m.Message.Content
	}
	combined += "\n\nAnswer them directly. If their message means the remaining plan should " +
		"change, also call propose_plan with the full updated checkpoint list — completed " +
		"checkpoints are preserved."

	handlingID := fmt.Sprintf("%s:followup:%d", input.PlanID, n)
	in := types.TurnInput{
		SessionKey:     input.SessionKey,
		TurnID:         handlingID,
		ParentType:     "plan",
		ParentID:       input.PlanID,
		ConnectionID:   input.ConnectionID,
		InitiatedBy:    "user",
		PlanID:         input.PlanID,
		PlanHandling:   true,
		Task:           &task,
		InitialMessage: types.Message{Role: "user", Content: combined},
	}
	if _, err := runChildTurn(ctx, handlingID, in); err != nil {
		logger.Warn("mid-plan follow-up turn failed — continuing the plan", "plan_id", input.PlanID, "error", err)
		return
	}
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	_ = workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, ao), "Deliver", handlingID).Get(ctx, nil)
	logger.Info("folded in mid-plan follow-up", "plan_id", input.PlanID, "turn", handlingID, "messages", len(msgs))
}

// finishPlan delivers the final checkpoint output (if any ran). For the root it
// then dispatches the one RecordSkill for the whole tree (prefix-swept by
// turns.plan_id) and tells the coordinator the run is done. A nested plan does
// neither — its parent is waiting on the child future and owns the merge-back.
func finishPlan(ctx workflow.Context, input types.PlanWorkflowInput, task types.TaskRepresentation, cpN int, closeReason string) {
	logger := workflow.GetLogger(ctx)
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)

	if cpN > 0 {
		_ = workflow.ExecuteActivity(actx, "Deliver", fmt.Sprintf("%s:cp:%d", input.PlanID, cpN)).Get(ctx, nil)
	}

	// Bug 2 (approval-gate outcomes): the gate's three non-proceed reasons
	// used to collapse into silence. "cancelled" (an unrelated message
	// interrupted the wait) stays silent on purpose — that message already
	// is the user's next word, matching this codebase's existing
	// interrupt-handling philosophy elsewhere. "expired" is a plain FYI, no
	// judgment call involved, so a fixed string is fine. "rejected" runs a
	// real (but content-neutral) turn instead: workflow code names no tool
	// and takes no stance on whether/how to follow up — that's deliberately
	// left to whatever the model decides is right for the situation, learned
	// procedure or not (docs/components/skill-subsystem.md: skills only
	// apply inside a real ModelCall turn, so a turn has to exist here for
	// that door to be open at all — a bare InsertMessage would close it).
	switch closeReason {
	case "rejected":
		runRejectionNoticeTurn(ctx, input, task)
	case "expired":
		deliverPlanNotice(actx, input.PlanID, "Plan cancelled — I didn't hear back in time. Send me another message whenever you'd like to pick this back up.")
	}

	if input.ParentPlanID != "" {
		return // nested — the parent owns close-out
	}
	// Root: the task-run *is* this tree — nothing else to close (decision B).
	dispatchRecordSkill(ctx, input.PlanID, task, "", closeReason)
	if err := workflow.SignalExternalWorkflow(ctx, input.SessionKey, "", PlanDoneSignalName, input.PlanID).Get(ctx, nil); err != nil {
		logger.Warn("failed to signal coordinator PlanDone", "plan_id", input.PlanID, "error", err)
	}
}

// deliverPlanNotice inserts a synthetic assistant message onto the planning
// turn (turn_id == plan_id — that turns row already exists by the time this
// is ever called) and delivers it, same InsertMessage+Deliver shape turn.go's
// own failTurn uses for its "something went wrong" notice. This is the only
// way the user ever sees a plan-approval outcome that isn't "approved": the
// planning turn's own messages row otherwise stays empty forever (propose_plan
// writes to PLAN.md, not to messages).
func deliverPlanNotice(actx workflow.Context, planID, content string) {
	insert := types.InsertMessageInput{
		TurnID:  planID,
		Message: types.Message{Role: "assistant", Content: content},
	}
	_ = workflow.ExecuteActivity(actx, "InsertMessage", insert).Get(actx, nil)
	_ = workflow.ExecuteActivity(actx, "Deliver", planID).Get(actx, nil)
}

// runRejectionNoticeTurn seeds a normal reason-act turn after an explicit
// plan rejection, so the model (not workflow code) both acknowledges the
// cancellation and reacts however it judges fit — including, if it has
// learned to, creating a check-in intention itself. The seed states the bare
// fact and nothing else: no tool is named, no follow-up is suggested. Same
// runChildTurn+explicit-Deliver shape foldInFollowups uses for its own
// mid-plan follow-up turns; if the turn itself fails, this logs and returns
// rather than falling back to a canned message, matching that function's own
// tolerance.
func runRejectionNoticeTurn(ctx workflow.Context, input types.PlanWorkflowInput, task types.TaskRepresentation) {
	logger := workflow.GetLogger(ctx)
	seed := "The plan you proposed"
	if task.RetrievalQuery != "" {
		seed += " for \"" + task.RetrievalQuery + "\""
	}
	seed += " was just rejected, with no reason given. Let the user know it's cancelled."

	turnID := input.PlanID + ":rejected"
	in := types.TurnInput{
		SessionKey:     input.SessionKey,
		TurnID:         turnID,
		ParentType:     "plan",
		ParentID:       input.PlanID,
		ConnectionID:   input.ConnectionID,
		InitiatedBy:    "plan",
		PlanID:         input.PlanID,
		Task:           &task,
		InitialMessage: types.Message{Role: "user", Content: seed},
	}
	if _, err := runChildTurn(ctx, turnID, in); err != nil {
		logger.Warn("plan-rejection follow-up turn failed", "plan_id", input.PlanID, "error", err)
		return
	}
	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	_ = workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, ao), "Deliver", turnID).Get(ctx, nil)
}

// runChildTurn runs a child TurnWorkflow under this plan and waits for it,
// returning the (content-free) TurnResult — the planning turn's NeedsApproval
// is the one field PlanWorkflow reads. ABANDON so a coordinator idle-exit
// doesn't tear a running turn down.
func runChildTurn(ctx workflow.Context, wfID string, in types.TurnInput) (types.TurnResult, error) {
	cwo := workflow.ChildWorkflowOptions{
		WorkflowID:        wfID,
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
	}
	var res types.TurnResult
	err := workflow.ExecuteChildWorkflow(workflow.WithChildOptions(ctx, cwo), TurnWorkflow, in).Get(ctx, &res)
	return res, err
}
