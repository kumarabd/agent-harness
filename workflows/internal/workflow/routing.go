package workflow

import (
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// retrievalPhaseTimeout bounds the whole retrieval fan-out — a slow backend
// (agent-brain, mcp-hub) can't stall the turn past this; unsettled subsystems
// are recorded "timed_out" and the turn proceeds with partial enrichment.
// Placeholder value, numeric-tuning-deferred like every other threshold here.
const retrievalPhaseTimeout = 10 * time.Second

// RoutingPlan is Route()'s decision: which retrieval subsystems this turn
// activates. FastPath == true means none of them — proceed straight to the
// reason-act loop with today's un-enriched context.
// docs/components/request-pipeline/03-routing.md has the rule table.
type RoutingPlan struct {
	FastPath bool `json:"fast_path"`
	Memory   bool `json:"memory"`
	Skills   bool `json:"skills"`
	Tools    bool `json:"tools"`
}

// Route decides the RoutingPlan purely from step 2's task representation —
// no I/O, deterministic, replay-safe, unit-testable without Temporal.
// Conservative: it only skips a subsystem when the classification is confident
// enough to justify it. A low-confidence or fallback classification
// (Confidence < 0.5, which includes the Confidence == 0 neutral fallback)
// takes the full path so nothing downstream is under-provisioned.
func Route(task types.TaskRepresentation) RoutingPlan {
	full := RoutingPlan{Memory: true, Skills: true, Tools: true}
	if task.Confidence < 0.5 {
		return full
	}
	switch task.Intent {
	case "conversational":
		return RoutingPlan{FastPath: true}
	case "meta":
		return RoutingPlan{Memory: true}
	case "question":
		if task.Complexity == "trivial" || task.Complexity == "simple" {
			return RoutingPlan{Memory: true}
		}
		return RoutingPlan{Memory: true, Skills: true}
	case "task":
		return full
	default:
		return full
	}
}

// RoutingWorkflowInput is RoutingWorkflow's input — a turn_id plus step 2's
// task representation (small derived routing metadata, not content).
// ParentTurnID is set only for a subagent turn (request-pipeline/
// 08-planning.md): its memory is inherited from the parent's snapshot rather
// than retrieved fresh.
//
// Mode == "reconcile" (request-pipeline/08-planning.md, "Reconciliation
// trigger") is a lighter re-run dispatched mid-turn when the user corrects
// course: memory + skill discovery only (no tools, no Route() gate), re-keyed
// on the correction inside the activities, replacing the stale bundle;
// ComposeSkill regenerates the composed block but leaves turn_plan alone.
type RoutingWorkflowInput struct {
	// EpisodeID (docs/components/episode-lifecycle.md) is the staging key —
	// retrieval rows go to turn_retrieval keyed by it, so every turn of the
	// episode shares one bundle. TurnID is the current turn (== EpisodeID for a
	// new episode's opening turn; different for a continuation-turn reconcile,
	// whose new message lives under TurnID).
	EpisodeID    string                   `json:"episode_id"`
	TurnID       string                   `json:"turn_id"`
	Task         types.TaskRepresentation `json:"task"`
	ParentTurnID string                   `json:"parent_turn_id,omitempty"`
	Mode         string                   `json:"mode,omitempty"` // "" | "reconcile"
}

// RoutingResult is RoutingWorkflow's output — the plan it chose plus a
// per-subsystem SubsystemResult (status + staged-row count). No content: the
// staged rows live in turn_retrieval, read from there by the planner / prompt
// assembly. Statuses: "ok" | "empty" | "error" | "timed_out" | "skipped".
type RoutingResult struct {
	Plan          RoutingPlan           `json:"plan"`
	Memory        types.SubsystemResult `json:"memory"`
	Tools         types.SubsystemResult `json:"tools"`
	Skills        types.SubsystemResult `json:"skills"`
	ComposedSkill bool                  `json:"composed_skill"`
}

// RoutingWorkflow — request pipeline step 3 (docs/components/request-pipeline/
// 03-routing.md). Child of TurnWorkflow, awaited before the reason-act loop.
// Decides the plan, runs the active retrieval subsystems in parallel under a
// phase deadline, then composes a skill if discovery produced candidates.
// Every path is best-effort — a failed or timed-out subsystem is recorded and
// the turn proceeds with whatever enrichment landed.
func RoutingWorkflow(ctx workflow.Context, input RoutingWorkflowInput) (RoutingResult, error) {
	logger := workflow.GetLogger(ctx)

	reconcile := input.Mode == "reconcile"
	// Reconcile mode skips the Route() gate entirely: the user has already
	// corrected course, so memory + skills are always worth re-keying. Tools
	// are not — the available capability set didn't change.
	plan := Route(input.Task)
	if reconcile {
		plan = RoutingPlan{Memory: true, Skills: true}
	}

	result := RoutingResult{
		Plan:   plan,
		Memory: types.SubsystemResult{Status: "skipped"},
		Tools:  types.SubsystemResult{Status: "skipped"},
		Skills: types.SubsystemResult{Status: "skipped"},
	}
	if plan.FastPath {
		logger.Info("routing: fast path — no enrichment", "episode_id", input.EpisodeID)
		return result, nil
	}

	// --- parallel fan-out over the active subsystems, raced against the phase
	// deadline. Cancelable context so a subsystem that misses the deadline is
	// actually cancelled (no wasted work), mirroring turn.go's tool-call
	// fan-out.
	retrievalCtx, cancelRetrieval := workflow.WithCancel(ctx)
	actx := workflow.WithActivityOptions(retrievalCtx, workflow.ActivityOptions{
		StartToCloseTimeout: retrievalPhaseTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})

	type pendingSubsystem struct {
		future workflow.Future
		out    *types.SubsystemResult
	}
	var subsystems []pendingSubsystem

	if plan.Memory {
		f := workflow.ExecuteActivity(actx, "MemoryRetrieve", types.MemoryRetrieveInput{
			EpisodeID:      input.EpisodeID,
			TurnID:         input.TurnID,
			RetrievalQuery: input.Task.RetrievalQuery,
			ParentTurnID:   input.ParentTurnID,
			Reconcile:      reconcile,
		})
		subsystems = append(subsystems, pendingSubsystem{f, &result.Memory})
	}
	if plan.Tools {
		f := workflow.ExecuteActivity(actx, "ToolDiscover", types.ToolDiscoverInput{
			EpisodeID:      input.EpisodeID,
			RetrievalQuery: input.Task.RetrievalQuery,
			Entities:       input.Task.Entities,
		})
		subsystems = append(subsystems, pendingSubsystem{f, &result.Tools})
	}
	if plan.Skills {
		f := workflow.ExecuteActivity(actx, "SkillDiscover", types.SkillDiscoverInput{
			EpisodeID:      input.EpisodeID,
			TurnID:         input.TurnID,
			RetrievalQuery: input.Task.RetrievalQuery,
			Reconcile:      reconcile,
		})
		subsystems = append(subsystems, pendingSubsystem{f, &result.Skills})
	}

	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	deadline := workflow.NewTimer(timerCtx, retrievalPhaseTimeout)

	settled := 0
	timedOut := false
	sel := workflow.NewSelector(ctx)
	sel.AddFuture(deadline, func(workflow.Future) { timedOut = true })
	for i := range subsystems {
		sel.AddFuture(subsystems[i].future, func(workflow.Future) { settled++ })
	}
	for settled < len(subsystems) && !timedOut {
		sel.Select(ctx)
	}
	cancelTimer()

	// Snapshot readiness before cancelling — distinguishes "settled with an
	// error" (a genuine error) from "cancelled for missing the deadline"
	// (timed_out), since both surface as an error from future.Get.
	ready := make([]bool, len(subsystems))
	allReady := true
	for i, s := range subsystems {
		ready[i] = s.future.IsReady()
		if !ready[i] {
			allReady = false
		}
	}
	if !allReady {
		cancelRetrieval()
		_ = workflow.Await(ctx, func() bool {
			for _, s := range subsystems {
				if !s.future.IsReady() {
					return false
				}
			}
			return true
		})
	}

	for i, s := range subsystems {
		var r types.SubsystemResult
		err := s.future.Get(ctx, &r)
		switch {
		case err == nil:
			*s.out = r
		case ready[i]:
			*s.out = types.SubsystemResult{Status: "error"}
		default:
			*s.out = types.SubsystemResult{Status: "timed_out"}
		}
	}
	cancelRetrieval()

	logger.Info("routing: retrieval fan-out complete", "episode_id", input.EpisodeID,
		"memory", result.Memory.Status, "tools", result.Tools.Status, "skills", result.Skills.Status)

	// --- step 6: compose a skill, only if discovery actually produced
	// candidates (plan.Skills alone isn't enough — there may be no matching
	// skeleton). ComposeSkill reads the staged memory/tool/skill rows itself.
	if result.Skills.Status == "ok" && result.Skills.Count > 0 {
		cctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: retrievalPhaseTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
		})
		var cr types.SubsystemResult
		if err := workflow.ExecuteActivity(cctx, "ComposeSkill", types.ComposeSkillInput{EpisodeID: input.EpisodeID, Reconcile: reconcile}).Get(cctx, &cr); err != nil {
			logger.Warn("routing: ComposeSkill failed", "episode_id", input.EpisodeID, "error", err)
		} else {
			result.ComposedSkill = cr.Status == "ok" && cr.Count > 0
		}
	}

	return result, nil
}

// startRouting spawns RoutingWorkflow as a child of the calling TurnWorkflow
// and races its completion against a follow-up message arriving in
// pendingMessages (docs/components/request-pipeline/03-routing.md, "option
// b"). If the user sends another message mid-routing, routing is cancelled
// and the turn proceeds un-enriched rather than making them wait for
// enrichment they've already superseded. Returns the RoutingResult (zero
// value if routing failed or was interrupted — always safe to read).
// episodeID is the retrieval staging key (== turnID for a new episode's opening
// turn); turnID is the current turn.
func startRouting(ctx workflow.Context, episodeID, turnID, parentTurnID string, task types.TaskRepresentation, pendingMessages *[]types.SignalPayload) RoutingResult {
	logger := workflow.GetLogger(ctx)

	routingCtx, cancelRouting := workflow.WithCancel(ctx)
	cwo := workflow.ChildWorkflowOptions{
		WorkflowID:        turnID + ":routing",
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
	}
	future := workflow.ExecuteChildWorkflow(workflow.WithChildOptions(routingCtx, cwo), RoutingWorkflow, RoutingWorkflowInput{
		EpisodeID:    episodeID,
		TurnID:       turnID,
		Task:         task,
		ParentTurnID: parentTurnID,
	})

	_ = workflow.Await(ctx, func() bool {
		return future.IsReady() || len(*pendingMessages) > 0
	})

	var result RoutingResult
	interrupted := !future.IsReady()
	if interrupted {
		cancelRouting()
		_ = workflow.Await(ctx, future.IsReady) // let cancellation settle — never ABANDON
	}
	err := future.Get(ctx, &result)
	cancelRouting()

	switch {
	case interrupted:
		logger.Info("routing interrupted by incoming message, proceeding un-enriched", "turn_id", turnID)
	case err != nil:
		logger.Error("routing did not complete, proceeding un-enriched", "turn_id", turnID, "error", err)
	default:
		logger.Info("routing complete", "turn_id", turnID,
			"fast_path", result.Plan.FastPath,
			"memory", result.Memory.Status, "tools", result.Tools.Status,
			"skills", result.Skills.Status, "composed_skill", result.ComposedSkill)
	}
	return result
}
