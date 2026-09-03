package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// docs/components/proactivity.md — "An intention is a workflow, not a row."
//
// One IntentionWorkflow execution per intention (workflow id = the intention
// id, "intn:<user>:<slug>"). Temporal's own execution state IS the record:
// Running = armed, Completed = satisfied/fired, Canceled = dropped (via
// CancelIntention → client.CancelWorkflow, not a signal), history = fire log.
// It arms its own trigger and, when it fires, dispatches FireIntention, which
// SignalWithStarts the session coordinator's Wake handler (coordinator.go).
//
// Trigger kinds it runs:
//   - "time" / "deadline"          — one-shot: Sleep to FireAt, fire, complete.
//   - "condition" / "state" / "event" — poll loop: every PollEvery, CheckCondition;
//     fire on the first match (v1 fires once), or give up at ExpiresAt.
//     ContinueAsNew every intentionPollCap cycles to bound history.
//   - "inactivity"                 — an IdleFor timer restarted by every `reset`
//     signal (the coordinator sends one on user activity); fires if it ever
//     completes.
//
// A calendar-recurring intention is a Temporal Schedule that starts a fresh
// "time" IntentionWorkflow per occurrence — the workflow never sees "schedule".
const (
	IntentionReviseSignalName = "revise"
	IntentionSnoozeSignalName = "snooze"
	IntentionResetSignalName  = "reset"
	IntentionStatusQueryName  = "status"

	intentionPollCap      = 100
	intentionCheckTimeout = 2 * time.Minute
)

func IntentionWorkflow(ctx workflow.Context, input types.IntentionInput) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("intention started", "intention_id", input.IntentionID, "kind", input.Kind)

	state := "armed"
	firedCount := input.FiredCount
	objective := input.Objective
	why := input.Why
	fireAt := input.FireAt
	pollEvery := input.PollEvery

	if err := workflow.SetQueryHandler(ctx, IntentionStatusQueryName, func() (types.IntentionStatus, error) {
		return types.IntentionStatus{
			IntentionID: input.IntentionID,
			Objective:   objective,
			Kind:        input.Kind,
			State:       state,
			FiredCount:  firedCount,
		}, nil
	}); err != nil {
		return err
	}

	reviseChan := workflow.GetSignalChannel(ctx, IntentionReviseSignalName)
	snoozeChan := workflow.GetSignalChannel(ctx, IntentionSnoozeSignalName)
	resetChan := workflow.GetSignalChannel(ctx, IntentionResetSignalName)

	applyRevise := func(c workflow.ReceiveChannel) {
		var r types.IntentionReviseSignal
		c.Receive(ctx, &r)
		if r.Objective != "" {
			objective = r.Objective
		}
		if r.Why != "" {
			why = r.Why
		}
		if !r.FireAt.IsZero() {
			fireAt = r.FireAt
		}
		if r.PollEvery > 0 {
			pollEvery = r.PollEvery
		}
		logger.Info("intention revised", "intention_id", input.IntentionID)
	}

	fire := func() error {
		state = "firing"
		ao := workflow.ActivityOptions{
			StartToCloseTimeout: activityTimeoutTierA,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
		}
		actx := workflow.WithActivityOptions(ctx, ao)
		err := workflow.ExecuteActivity(actx, "FireIntention", types.FireIntentionInput{
			IntentionID: input.IntentionID,
			SessionKey:  input.SessionKey,
			Objective:   objective,
			Why:         why,
		}).Get(actx, nil)
		firedCount++
		state = "armed"
		if err != nil {
			logger.Error("FireIntention failed", "intention_id", input.IntentionID, "error", err)
		}
		return err
	}

	switch input.Kind {

	case "time", "deadline":
		// A zero FireAt means "fire now" — this is how a recurring intention
		// works: a Temporal Schedule (tools_intention.py) starts one of these
		// per tick with no FireAt.
		if fireAt.IsZero() {
			return fire()
		}
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d := fireAt.Sub(workflow.Now(ctx))
			if d <= 0 {
				return fire()
			}
			fired := false
			timerCtx, cancel := workflow.WithCancel(ctx)
			sel := workflow.NewSelector(ctx)
			sel.AddFuture(workflow.NewTimer(timerCtx, d), func(f workflow.Future) {
				fired = f.Get(ctx, nil) == nil // nil ⇒ elapsed; a CanceledError ⇒ workflow cancelled
			})
			sel.AddReceive(reviseChan, func(c workflow.ReceiveChannel, _ bool) { applyRevise(c) })
			sel.AddReceive(snoozeChan, func(c workflow.ReceiveChannel, _ bool) {
				var by time.Duration
				c.Receive(ctx, &by)
				fireAt = fireAt.Add(by)
				logger.Info("intention snoozed", "intention_id", input.IntentionID, "by", by.String())
			})
			sel.Select(ctx)
			cancel()
			if fired {
				return fire()
			}
		}

	case "condition", "state", "event":
		if input.Probe == nil {
			return temporal.NewNonRetryableApplicationError(
				"poll-kind intention has no probe", "IntentionMisconfigured", nil)
		}
		if pollEvery <= 0 {
			pollEvery = 5 * time.Minute
		}
		polls := 0
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !input.ExpiresAt.IsZero() && workflow.Now(ctx).After(input.ExpiresAt) {
				state = "expired"
				logger.Info("intention expired unfired", "intention_id", input.IntentionID)
				return nil
			}
			timerCtx, cancel := workflow.WithCancel(ctx)
			sel := workflow.NewSelector(ctx)
			sel.AddFuture(workflow.NewTimer(timerCtx, pollEvery), func(workflow.Future) {})
			sel.AddReceive(reviseChan, func(c workflow.ReceiveChannel, _ bool) { applyRevise(c) })
			sel.Select(ctx)
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}

			ao := workflow.ActivityOptions{
				StartToCloseTimeout: intentionCheckTimeout,
				RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
			}
			var res types.CheckConditionResult
			if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, ao), "CheckCondition",
				types.CheckConditionInput{IntentionID: input.IntentionID, Probe: *input.Probe},
			).Get(ctx, &res); err != nil {
				return err
			}
			if res.Fired {
				logger.Info("intention condition met", "intention_id", input.IntentionID, "note", res.Note)
				if err := fire(); err != nil {
					return err
				}
				return nil // v1: fires once
			}

			if polls++; polls >= intentionPollCap {
				input.FiredCount = firedCount
				input.Objective = objective
				input.Why = why
				input.PollEvery = pollEvery
				return workflow.NewContinueAsNewError(ctx, IntentionWorkflow, input)
			}
		}

	case "inactivity":
		idle := input.IdleFor
		if idle <= 0 {
			return temporal.NewNonRetryableApplicationError(
				"inactivity intention has no idle_for", "IntentionMisconfigured", nil)
		}
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fired := false
			timerCtx, cancel := workflow.WithCancel(ctx)
			sel := workflow.NewSelector(ctx)
			sel.AddFuture(workflow.NewTimer(timerCtx, idle), func(f workflow.Future) {
				fired = f.Get(ctx, nil) == nil
			})
			sel.AddReceive(resetChan, func(c workflow.ReceiveChannel, _ bool) { c.Receive(ctx, nil) })
			sel.AddReceive(reviseChan, func(c workflow.ReceiveChannel, _ bool) { applyRevise(c) })
			sel.Select(ctx)
			cancel()
			if fired {
				return fire()
			}
			// a reset (or revise) fell through — loop re-arms a fresh idle timer
		}

	default:
		return temporal.NewNonRetryableApplicationError(
			"unknown intention kind: "+input.Kind, "IntentionMisconfigured", nil)
	}
}
