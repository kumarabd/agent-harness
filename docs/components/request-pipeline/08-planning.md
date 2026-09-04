# Request Pipeline — Step 8: Planning

> STATUS: BUILT 2026-09-03 on branch `proactivity-substrate` (`go build ./...` +
> `go test ./internal/...` green, `activities/` compiles; **NOT deployed, NOT
> live-verified**). Design: **plan-and-execute orchestrator** — a Deliberate
> task-run is a `PlanWorkflow`, not a ledger the model narrates inside one loop.
>
> **Open:** deploy + live-verify; the gateway renders `plan_approval` requests
> through the kind-agnostic user-input path (no bespoke UI). Scenarios reworked
> (the starter now scripts checkpoint turns via `checkpoint_responses` /
> `plan_followup` — see `workflows/scenarios/README.md`), not yet run against a
> deploy.
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md),
> [`../episode-lifecycle.md`](../episode-lifecycle.md) (why a task-run is the unit).
> Feeds: [`../proactivity.md`](../proactivity.md) — a `PlanWorkflow` blocked
> between checkpoints is what a scheduled wake advances.

## The model

The **Lite lane** is unchanged: one reason-act `TurnWorkflow`, no plan.

The **Deliberate lane** is a `PlanWorkflow` (workflow id `<plan_id>:plan`, where
`plan_id` is the planning turn's id). `dispatch.go` starts it instead of a plain
`TurnWorkflow` when `ClassifyRequest` + `ResolveOpenPlan` say "fresh Deliberate
task-run".

```
PlanWorkflow(plan_id, task):
  1. PLANNING TURN   child TurnWorkflow id=plan_id, PlanningMode
       reads task + retrieved skills + memory + discovered tools + lcm
       ends by calling propose_plan({checkpoints:[{intent, done_when, complex?}], needs_approval})
       → ModelCall peels it, plan.seed writes PLAN.md; needs_approval rides out on TurnResult
  2. APPROVAL GATE   (root plan only)
       needs_approval false → proceed
       true → UserInputRequestWorkflow(kind="plan_approval"): approve · revise (re-plan, ≤3×) · reject
  3. EXECUTION LOOP   each non-terminal checkpoint, in order:
       a. pending user follow-up?  → foldInFollowups: a PlanHandling turn answers + may re-seed the plan
       b. NextCheckpoint → seed text
       c. checkpoint is `complex`?  → nested PlanWorkflow  (3C-iii, opaque)
          else                      → flat checkpoint TurnWorkflow → calls checkpoint_done({status, note?, revised_tail?})
  4. CLOSE   dispatch RecordSkill (async, ABANDON) over the whole tree; signal the coordinator PlanDone
```

## PLAN.md — the store

One file per plan at `$SESSION_ROOT/session/<session-key>/plans/<plan_id
':'→'_'>/PLAN.md` on the tenant PV. **It is the store, not a mirror** — there is
no `turn_plan` table (dropped, migration `023`). Greppable and editable by
`shell_exec` and by any delegated Claude Code on the same task; human-readable,
so the user reads and edits the exact artifact they approve.

```markdown
# Plan
status: executing            # executing | complete

- cp1 [x] Locate the failing test
- cp2 [ ] Reproduce the failure
      done_when: the failure reproduces locally
- cp3 [ ] Bisect to the offending change
      complex: true
```

Marks: `[x]` done · `[-]` skipped · `[~]` revised · `[ ]` pending. `render_block`
marks the first non-terminal checkpoint `[>]` at render time only. `plan.py` I/O
functions take a `plan_id` and touch the PV directly — every caller is a
tenant-worker activity with the volume mounted.

**Re-seed merge is by intent, not position.** When `plan.seed` runs over an
existing ledger (a mid-execution `PlanHandling` turn re-proposing the whole
plan), a checkpoint whose intent matches one already there carries its
status/note over; a new step is `pending`. cp ids are just fresh position
labels — so a re-plan that inserts or reorders steps before a completed one
doesn't misattribute the mark. (`checkpoint_done`'s `revised_tail` path is
separate: it only ever replaces the still-pending tail after the current
checkpoint.)

## PlanWorkflow

Runs on the loop-worker (pure orchestration, no tenant credentials — the PV I/O
is in the activities and the child turns). One per Deliberate task-run, plus one
per `complex` checkpoint (nested).

| | |
|---|---|
| **spawns** | child `TurnWorkflow`s (`initiated_by='plan'`): the planning turn, then one per checkpoint / follow-up / re-plan; child `PlanWorkflow`s for `complex` checkpoints |
| **signals in** | `NewMessage` (user follow-up, folded in at the next checkpoint boundary; forwarded into the active checkpoint turn if one is running), `abandon` (a new task superseded this one — wrap up at the next boundary) |
| **signals out** | `PlanDone` to `CoordinatorWorkflow` (root only); a nested plan's completion reaches its parent via the child-workflow future |
| **on completion** | `dispatchRecordSkill` (ABANDON) over the whole tree, then `PlanDone` |
| **outlives the coordinator** | ABANDON child — a coordinator idle-exit doesn't tear it down; a later message recreates the coordinator, which finds the running `<plan_id>:plan` via `ResolveOpenPlan` and resumes forwarding to it |

## The planning turn

A child `TurnWorkflow` in `PlanningMode`: `ModelCall` swaps in
`PLANNING_SYSTEM_PROMPT` and offers only `propose_plan` + the next-step hint
tool. Its context is the retrieved skills (full rendered procedures, staged by
`SkillDiscover` under the plan_id), memory, a **capability catalog** (the
`ToolDiscover` rows rendered as a one-line reference list — `09-prompt-assembly.md`,
the "capabilities" section, which since 2026-09-04 survives only for this turn
kind), and the `lcm` conversation. It calls `propose_plan` and the turn ends
(`stop_reason=planned`). Before this the planning turn saw only a hint block it
couldn't interrogate; the catalog lets it draft against what's actually
reachable. Checkpoint turns instead get those tools bound as **callable**
schemas (`tool-registry.md`, "Resolved: Three-Layer Tool Taxonomy").

**Skills are input to a draft, never executed verbatim** — a matched procedure
shapes the plan; it does not become the execution. That is the core mitigation
of the "decompose on assumptions" objection.

## Approval gate

`propose_plan`'s `needs_approval` rides out on
`ModelCallOutput.NeedsApproval → TurnResult.NeedsApproval` (a control bool, same
category as the next-step tier hint — no PLAN.md state, no dedicated activity).

- `false` → proceed.
- `true` → `runApprovalGate` parks the plan on a child `UserInputRequestWorkflow`
  of kind `plan_approval` — the same primitive permission gating uses, rendering
  the plan with `approve` / `reject` buttons and free text. **approve** → run.
  **free text** → a `<plan_id>:replan:<n>` planning turn seeded with the
  feedback, then re-gate (cap 3 rounds, then proceed with the standing draft).
  **reject / expire / cancel** → wrap up, `RecordSkill` `close_reason=rejected`.

Nested plans never gate (the root plan carries the user's oversight). This
approval is the first concrete instance of proactivity — the agent initiating a
message and waiting on the user — and it rides machinery that already exists.

## Checkpoint execution

`NextCheckpoint` reads PLAN.md, returns the first non-terminal checkpoint as a
seed message plus its `complex` flag.

- **Flat checkpoint** → one child `TurnWorkflow` seeded with the checkpoint
  intent + `done_when` + the rendered plan. Full reason-act loop. It ends by
  calling `checkpoint_done({checkpoint_id, status, note?, revised_tail?})` —
  peeled by `ModelCall`, applied to PLAN.md by `plan.apply_checkpoint_done`.
- **Re-planning the tail.** `revised_tail: [{intent, done_when?, complex?}]`
  replaces every still-pending checkpoint *after* this one — this is where "the
  first executing step invalidated its siblings" is fixed: every boundary is a
  re-plan with real results in hand.
- **Recursion (3C-iii).** A `complex:true` checkpoint runs as a nested
  `PlanWorkflow` (`<plan_id>:cp:<n>:sub:plan`, `Depth+1`, cap `maxPlanDepth=2`).
  The nested plan is an **opaque single task**: no approval gate, no `PlanDone`,
  no `RecordSkill` of its own. On its return the parent calls
  `MarkCheckpointDone`. Every turn id in the tree sits under the root's, so the
  root's one `RecordSkill` prefix-sweeps them all — a deep task is learned as one
  skill, deliberately.

## Interruption

| when | handling |
|---|---|
| mid-checkpoint-turn | the user message is forwarded into the running child turn (the existing turn interrupt path) |
| at a checkpoint boundary | `foldInFollowups`: drain `pending` into one `PlanHandling` turn (normal reason-act + `propose_plan`) → answers the user, may re-seed the plan; its output is delivered |
| mid-nested-plan | `runNestedPlan` selects on a wake channel — a follow-up or `abandon` cancels the child; the checkpoint stays pending and the loop re-reads the ledger |
| "stop / different thing now" | `abandon` (sent by `dispatch.go` when `ResolveOpenPlan` says supersede) → the loop breaks; `RecordSkill` still fires over the completed portion, `close_reason=superseded` |

## Completion → RecordSkill

One async activity, dispatched detached (ABANDON) by the root plan. **Replaces**
the `RecordSkillOutcome` → `skill_candidates` → `SkillSynthesizeWorkflow` chain
(migration `022` drops the table):

- **input:** the plan's whole multi-turn trajectory (prefix-swept by
  `turns.plan_id`), the final PLAN.md, and the outcome (`close_reason` +
  stop reasons).
- embeds the task, matches against `skill_procedures`: match → reinforce
  (confidence EMA, re-`generalize` on a divergence); no match + success →
  insert a new `learned:` procedure whose body is the trajectory's shape.
- no candidates table, no synthesis debounce — record the procedure that was
  actually followed, directly. Match-or-insert keeps the "similar runs converge
  on one procedure" benefit the candidates table used to provide.

## How the 2026-08-31 objections are handled

The prior design made the plan an in-loop advisory ledger, rejecting
plan-and-execute for three reasons:

| objection | now |
|---|---|
| an upfront planner call costs 2–10s on every complex turn | **one** planning turn per task-run, not per user turn; it replaced `ComposeSkill`'s prose generation — roughly net-even |
| plans built on assumptions; the first step invalidates its siblings | per-checkpoint tail re-planning (`revised_tail`) — the tail is re-derived with real results at every boundary |
| model plan inertia | checkpoints are `done_when`-observable and the checkpoint turn is told "revise where the task diverges"; the tail is *expected* to change |
| decompose-to-leaves recursion explodes | recursion only on an explicit `complex:true` flag; depth capped at `maxPlanDepth=2` |

## Degradation (no fallback)

- The planning turn fails to produce a valid `propose_plan` → bounded retry →
  the run fails and surfaces (same posture as `ClassifyRequest`).
- A checkpoint / nested-plan child fails → the error propagates and fails the
  `PlanWorkflow`. No "skip and continue", no infinite retry.
- `RecordSkill` fails → logged; the run is already closed; that trajectory is
  lost to the skill store but nothing user-facing breaks.

## Deferred

- **Parallel checkpoints** — independent checkpoints fanned out concurrently
  (`depends_on` in PLAN.md). Execution is sequential for now.
- **Procedure generalization (topic → shape)** — `generalize.py`'s prompt now
  forces task-CLASS altitude for `trigger_text`; unverified, and the
  `RecordSkill` match test still embeds raw `task_text` against it (see
  `skill-subsystem.md`).
- **`plan_approval` gateway UI** — currently the kind-agnostic button path.
- **Cross-session plan resumption** — picking an abandoned `PlanWorkflow` back up
  days later; folded into proactivity's deliberation work.
