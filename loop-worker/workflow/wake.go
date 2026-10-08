package workflow

import (
	"agent-harness/shared/types"
	"go.temporal.io/sdk/workflow"
)

// docs/components/proactivity.md — "The fire path".
//
// WakeWorkflow is the whole of a time-based wake. A Temporal Schedule's action
// starts one of these per firing; it calls WakeSession once and completes. It
// carries no state, takes no signals and answers no queries — the Schedule owns
// the cadence, the coordinator owns the commitment, so there is nothing here to
// keep alive and nothing to bound with ContinueAsNew.
//
// This is deliberately NOT an "IntentionWorkflow". A standing request you can
// list, pause and cancel is a Schedule (Temporal's own, with the calendar maths,
// catch-up and overlap policy already solved); a thing that can be woken is a
// session. The workflow between them is bookkeeping, not a place to store
// anything — which is why a wake costs a handful of history events rather than a
// durable per-commitment execution.
func WakeWorkflow(ctx workflow.Context, input types.WakeInput) error {
	ctx = WithTenantTaskQueue(ctx, input.TenantSlug)
	logger := workflow.GetLogger(ctx)

	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, ao), "WakeSession", types.WakeSessionInput{
		WakeID:     input.WakeID,
		SessionKey: input.SessionKey,
		Objective:  input.Objective,
		Why:        input.Why,
	}).Get(ctx, nil)
	if err != nil {
		// No fallback. A wake that cannot reach its session is a real failure and
		// surfaces as one — the Schedule's own next tick is the retry, and a
		// schedule that keeps failing is visible in its own action history.
		logger.Error("wake failed", "wake_id", input.WakeID, "session_key", input.SessionKey, "error", err)
		return err
	}
	logger.Info("woke session", "wake_id", input.WakeID, "session_key", input.SessionKey)
	return nil
}
