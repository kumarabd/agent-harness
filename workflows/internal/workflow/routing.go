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

// laneIsDeliberate reports whether a turn takes the Deliberate lane
// (docs/components/lane-model.md) — the full retrieval pipeline plus an episode
// and RL recording. Everything else is the Lite lane: memory-only retrieval
// (or nothing, for conversational), no episode, no skills/tools/plan/recording.
// Pure, deterministic, replay-safe — the single source of truth for the lane
// split, consumed by both Route() here and turn.go's OpenEpisode / recording
// gates. Deliberate is exactly (task, moderate|complex), (question, complex),
// plus the Confidence < 0.5 fallback (a misclassified real task must not be
// under-provisioned) and any unrecognised intent.
func laneIsDeliberate(task types.TaskRepresentation) bool {
	if task.Confidence < 0.5 {
		return true
	}
	switch task.Intent {
	case "conversational", "meta":
		return false
	case "question":
		return task.Complexity == "complex"
	case "task":
		return task.Complexity == "moderate" || task.Complexity == "complex"
	default:
		return true
	}
}

// Route decides the RoutingPlan purely from step 2's task representation —
// no I/O, deterministic, replay-safe, unit-testable without Temporal.
// docs/components/lane-model.md: a Deliberate turn gets the full fan-out; a
// Lite turn gets memory only, except pure chit-chat which needs nothing.
func Route(task types.TaskRepresentation) RoutingPlan {
	if laneIsDeliberate(task) {
		return RoutingPlan{Memory: true, Skills: true, Tools: true}
	}
	if task.Intent == "conversational" {
		return RoutingPlan{FastPath: true}
	}
	return RoutingPlan{Memory: true}
}

// RoutingWorkflowInput is RoutingWorkflow's input — the ids to stage under plus
// step 2's task representation (small derived routing metadata, not content).
// ParentTurnID is set only for a subagent turn (request-pipeline/
// 08-planning.md): its memory is inherited from the parent's snapshot rather
// than retrieved fresh.
//
// REVISED 2026-09-02 (episode-lifecycle.md REVISION): memory + tool discovery
// run every turn, staged under TurnID. Skill discovery + ComposeSkill run only
// when PlanID is set (a fresh Deliberate episode's opening turn) — they seed
// the plan once. The reconcile mode is gone.
type RoutingWorkflowInput struct {
	TurnID string `json:"turn_id"` // memory + tool staging key (the current turn)
	// PlanID set ⇒ also run skill discovery + ComposeSkill and seed the plan,
	// staged under PlanID. Empty on a continuation / Lite turn.
	PlanID       string                   `json:"episode_id,omitempty"`
	Task         types.TaskRepresentation `json:"task"`
	ParentTurnID string                   `json:"parent_turn_id,omitempty"`
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

	plan := Route(input.Task)
	// Skill discovery + ComposeSkill only run for a fresh episode's opening turn
	// (PlanID set) — they seed the plan once. Continuation / Lite turns still
	// get fresh memory + tools.
	seedEpisode := input.PlanID != ""
	if !seedEpisode {
		plan.Skills = false
	}

	result := RoutingResult{
		Plan:   plan,
		Memory: types.SubsystemResult{Status: "skipped"},
		Tools:  types.SubsystemResult{Status: "skipped"},
		Skills: types.SubsystemResult{Status: "skipped"},
	}
	if plan.FastPath {
		logger.Info("routing: fast path — no enrichment", "turn_id", input.TurnID)
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
			OwnerID:        input.TurnID,
			RetrievalQuery: input.Task.RetrievalQuery,
			ParentTurnID:   input.ParentTurnID,
		})
		subsystems = append(subsystems, pendingSubsystem{f, &result.Memory})
	}
	if plan.Tools {
		f := workflow.ExecuteActivity(actx, "ToolDiscover", types.ToolDiscoverInput{
			OwnerID:        input.TurnID,
			RetrievalQuery: input.Task.RetrievalQuery,
			Entities:       input.Task.Entities,
		})
		subsystems = append(subsystems, pendingSubsystem{f, &result.Tools})
	}
	if plan.Skills {
		f := workflow.ExecuteActivity(actx, "SkillDiscover", types.SkillDiscoverInput{
			PlanID:         input.PlanID,
			RetrievalQuery: input.Task.RetrievalQuery,
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

	logger.Info("routing: retrieval fan-out complete", "turn_id", input.TurnID, "episode_id", input.PlanID,
		"memory", result.Memory.Status, "tools", result.Tools.Status, "skills", result.Skills.Status)

	// ComposeSkill is removed (Phase 3C — 08-planning.md): SkillDiscover's rows
	// feed the planning turn, which drafts the plan via `propose_plan`. The
	// staged `kind='skill'` rows are read directly by the planning turn's prompt.
	return result, nil
}

// startRouting spawns RoutingWorkflow as a child of the calling TurnWorkflow
// and races its completion against a follow-up message arriving in
// pendingMessages (docs/components/request-pipeline/03-routing.md, "option
// b"). If the user sends another message mid-routing, routing is cancelled
// and the turn proceeds un-enriched rather than making them wait for
// enrichment they've already superseded — that's a deliberate supersede, not
// a failure, so err is nil in that case.
//
// A non-nil err means RoutingWorkflow genuinely failed — which, after the
// "no fallback" changes, means ComposeSkill couldn't produce a merged
// procedure (the fan-out subsystems record their own errors into the result
// and never fail the workflow). The caller (turn.go) fails the turn on it.
// The RoutingResult is always safe to read (zero value on failure/interrupt).
// planID is the retrieval staging key (== turnID for a new episode's opening
// turn); turnID is the current turn.
func startRouting(ctx workflow.Context, planID, turnID, parentTurnID string, task types.TaskRepresentation, pendingMessages *[]types.SignalPayload) (RoutingResult, error) {
	logger := workflow.GetLogger(ctx)

	routingCtx, cancelRouting := workflow.WithCancel(ctx)
	cwo := workflow.ChildWorkflowOptions{
		WorkflowID:        turnID + ":routing",
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
	}
	future := workflow.ExecuteChildWorkflow(workflow.WithChildOptions(routingCtx, cwo), RoutingWorkflow, RoutingWorkflowInput{
		PlanID:       planID,
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
		return result, nil
	case err != nil:
		logger.Error("routing failed", "turn_id", turnID, "error", err)
		return result, err
	default:
		logger.Info("routing complete", "turn_id", turnID,
			"fast_path", result.Plan.FastPath,
			"memory", result.Memory.Status, "tools", result.Tools.Status,
			"skills", result.Skills.Status, "composed_skill", result.ComposedSkill)
		return result, nil
	}
}
