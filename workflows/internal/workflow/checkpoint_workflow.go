package workflow

import (
	"fmt"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// checkpointRetryCap — total attempts for one checkpoint: 1 normal + 1
// escalated retry, then give up. Same bounded-then-give-up shape as this
// codebase's other retry ceilings (planApprovalRevisionCap,
// deliveryRecoveryRoundCap) — a couple of attempts, not an unbounded loop.
const checkpointRetryCap = 2

// CheckpointWorkflow reliably gets ONE checkpoint done — see
// types.CheckpointWorkflowInput's own doc comment for why this is its own
// workflow type rather than logic inside PlanWorkflow's loop. Runs the
// checkpoint as a TurnWorkflow child, then checks PLAN.md directly
// (NextCheckpoint) to see whether it actually advanced — a checkpoint turn
// can "succeed" (no error) while making zero progress (empty content, no
// checkpoint_done call), and that's exactly the failure mode this exists to
// catch. Real infra failures (the child TurnWorkflow itself erroring) still
// propagate as a genuine workflow error, retried by Temporal's own
// ChildWorkflowOptions.RetryPolicy at the PlanWorkflow dispatch site — this
// workflow's own retry loop is for the "succeeded but did nothing" case
// specifically, which isn't a Temporal-level failure at all.
func CheckpointWorkflow(ctx workflow.Context, input types.CheckpointWorkflowInput) (types.CheckpointWorkflowOutput, error) {
	logger := workflow.GetLogger(ctx)
	metrics := workflow.GetMetricsHandler(ctx)
	started := workflow.Now(ctx)

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: activityTimeoutTierA,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	}
	actx := workflow.WithActivityOptions(ctx, ao)

	hintModality, hintTier := input.HintModality, input.HintTier
	for attempt := 1; attempt <= checkpointRetryCap; attempt++ {
		if attempt > 1 {
			// Real, live bug found 2026-09-07: a checkpoint turn that ends
			// with no content and no checkpoint_done leaves the SAME
			// checkpoint pending — NextCheckpoint just re-selects it,
			// identically, forever, until the blunt overall iteration cap
			// gives up and delivers that same empty turn as the plan's
			// final answer (confirmed live: 21 identical rounds of the same
			// seed, fast tier, zero progress every time). Force the tier up
			// on this one retry — a checkpoint carrying a full tool schema
			// plus real conversation history may just need more than its
			// own narrow seed text's fresh classify gave it credit for.
			hintModality, hintTier = "language", "expert"
			logger.Warn("checkpoint made no progress — retrying with an escalated tier",
				"plan_id", input.PlanID, "checkpoint", input.CheckpointID, "attempt", attempt)
		}

		cpTurnID := fmt.Sprintf("%s:cp:%d:attempt:%d", input.PlanID, input.CpN, attempt)
		cpInput := types.TurnInput{
			SessionKey:     input.SessionKey,
			TurnID:         cpTurnID,
			ParentType:     "plan",
			ParentID:       input.PlanID,
			ConnectionID:   input.ConnectionID,
			InitiatedBy:    input.InitiatedBy,
			InitialMessage: types.Message{Role: "user", Content: input.SeedText},
			PlanID:         input.PlanID,
			HintModality:   hintModality,
			HintTier:       hintTier,
		}
		res, err := runChildTurn(ctx, cpTurnID, cpInput)
		if err != nil {
			return types.CheckpointWorkflowOutput{}, fmt.Errorf("checkpoint turn %s: %w", cpTurnID, err)
		}

		var next types.NextCheckpointResult
		if err := workflow.ExecuteActivity(actx, "NextCheckpoint", input.PlanID).Get(ctx, &next); err != nil {
			return types.CheckpointWorkflowOutput{}, err
		}
		advanced := !next.HasNext || next.CheckpointID != input.CheckpointID

		metrics.WithTags(map[string]string{"outcome": outcomeTag(advanced)}).Counter("checkpoint_attempts_total").Inc(1)

		if advanced {
			logger.Info("checkpoint executed", "plan_id", input.PlanID, "cp", input.CheckpointID, "turn", cpTurnID, "attempt", attempt)
			metrics.Timer("checkpoint_duration_seconds").Record(workflow.Now(ctx).Sub(started))
			return types.CheckpointWorkflowOutput{NextHintModality: res.NextHintModality, NextHintTier: res.NextHintTier}, nil
		}
		logger.Warn("checkpoint made no progress", "plan_id", input.PlanID, "checkpoint", input.CheckpointID, "turn", cpTurnID, "attempt", attempt)
	}

	// Exhausted retries — give up on THIS checkpoint specifically rather
	// than let PlanWorkflow's own iteration cap silently burn through it.
	logger.Error("checkpoint made no progress after a retry — skipping it", "plan_id", input.PlanID, "checkpoint", input.CheckpointID)
	_ = workflow.ExecuteActivity(actx, "MarkCheckpointDone", input.PlanID, input.CheckpointID, "skipped",
		"no progress after a retry with an escalated tier — skipped automatically").Get(ctx, nil)
	metrics.Counter("checkpoint_skipped_total").Inc(1)
	metrics.Timer("checkpoint_duration_seconds").Record(workflow.Now(ctx).Sub(started))
	return types.CheckpointWorkflowOutput{Skipped: true}, nil
}

func outcomeTag(advanced bool) string {
	if advanced {
		return "advanced"
	}
	return "no_progress"
}
