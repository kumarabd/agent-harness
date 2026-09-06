# Request Pipeline — Step 3: Routing + Retrieval Orchestration

> STATUS: BUILT — `workflows/internal/workflow/routing.go` (`Route`,
> `RoutingPlan`, `RoutingWorkflow`, `RoutingResult`, `startRouting`), registered
> on the loop-worker, spawned from `turn.go`. `turn_retrieval` table = migration
> `013` (key column `owner_id` since `021`). Subsystem activities
> (`MemoryRetrieve` / `ToolDiscover` / `SkillDiscover`) live in
> `activities/activities/retrieval/`.
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md). Owns the `Route()`
> decision + `RoutingWorkflow` (the retrieval fan-out).
>
> `RoutingWorkflow` = `Route()` gate → **memory + tool discovery every turn**
> (staged under `TurnID`) + **skill discovery only on a planning turn**
> (`RoutingWorkflowInput.PlanID` set — staged under the plan_id). There is no
> `ComposeSkill` step (removed — [`06-skill-composition.md`](06-skill-composition.md));
> the lane split is [`../lane-model.md`](../lane-model.md)'s `laneIsDeliberate`,
> also the plan-vs-plain-turn decision in `dispatch.go`.

### Role

Decide which retrieval subsystems this turn needs (memory / skills / tools),
run the active subset in parallel under a phase deadline, and hand
`TurnWorkflow` a `RoutingResult` — the plan plus per-subsystem status. The bulk
content is staged to `turn_retrieval`, read there by prompt assembly (step 9).

### `Route()` — the decision

A **pure deterministic Go function**, `Route(taskRep) RoutingPlan`, in
`routing.go`. No I/O — replay-safe, unit-testable without Temporal.

```go
type RoutingPlan struct {
    FastPath bool // no enrichment — straight to the reason-act loop
    Memory   bool // step 4
    Skills   bool // step 5 — only when RoutingWorkflowInput.PlanID is set
    Tools    bool // step 7
}
```

The lane split is `laneIsDeliberate(taskRep)` ([`../lane-model.md`](../lane-model.md),
the single source of truth, also `dispatch.go`'s plan-vs-plain-turn decision):

- **Deliberate** — `(task, moderate|complex)`, `(question, complex)`, any
  `Confidence < 0.5`, or an unrecognised intent → `{Memory, Skills, Tools}`.
- **`conversational`** → `FastPath` (no enrichment).
- **everything else (Lite)** → `{Memory}` only.

`Route` returns `Skills: true` for a Deliberate turn, but `RoutingWorkflow`
clears it unless `PlanID` is set — skills stage once, on the planning turn.

Each subsystem also keeps a cheap *internal* guard ("backend unconfigured",
"empty query") — "I can't run", not policy. A misroute to `FastPath` self-heals
(the model still has full tool access and can call `memory_search` /
`search_tools` mid-turn); a misroute to full is a wasted parallel retrieval.
Neither is a correctness bug.

### `RoutingWorkflow` — the orchestration

A **child workflow of `TurnWorkflow`**, spawned by `startRouting` and raced
against a follow-up message (below), for top-level turns only.

```
TurnWorkflow
  ├─ InsertMessage
  ├─ ClassifyRequest → taskRep                     (step 2)
  ├─ startRouting(seedPlanID, turn_id, ..., taskRep, &pendingMessages) → RoutingResult
  │    └─ RoutingWorkflow  child, REQUEST_CANCEL
  │         1. plan := Route(taskRep); if PlanID == "" { plan.Skills = false }
  │         2. if plan.FastPath: return all-"skipped"
  │         3. dispatch the plan's subset in parallel:
  │              MemoryRetrieve(owner=turn_id)  ┐  Selector loop over the futures,
  │              ToolDiscover(owner=turn_id)    ├  racing workflow.NewTimer(retrievalPhaseTimeout);
  │              SkillDiscover(plan_id)         ┘  unsettled → cancelled, recorded "timed_out"
  │         4. return RoutingResult{ Plan, Memory, Tools, Skills }
  └─ reason-act loop  (prompt.assemble reads the staged rows)
```

`RoutingWorkflow` input is `{turn_id, plan_id, taskRep, parent_turn_id}` — small
derived routing metadata (see step 2), not content. Output is the plan + each
subsystem's `SubsystemResult` (`{Status, Count}`) — no content. `seedPlanID` is
non-empty only for a planning turn (or a Deliberate subagent's fresh run).

**Why a child workflow, not inline in `TurnWorkflow`:**
- History isolation — `TurnWorkflow` already runs a ≤20-iteration loop; the
  retrieval phase's 4–6 activity events stay out of that history.
- One clean phase deadline for the whole fan-out.
- One legible execution per turn in the Temporal UI; independently versionable.
- Reusable — mid-turn re-retrieval, a subagent's lighter retrieval, or a
  post-compaction re-run can all invoke it.

**Why the subsystems are activities, not sub-workflows:** each is 1–3
best-effort I/O calls with no independent lifecycle. An activity `RetryPolicy`
already gives durable retry + backoff + heartbeat cancellation; a workflow per
subsystem adds an execution's overhead for no durability gain. Matches the
codebase's deliberate use of child workflows only where they earn it
(`WriteMemoryWorkflow` / `CompressContextWorkflow` — ABANDON / detached
semantics).

### Interrupt handling ("option b")

`TurnWorkflow` awaits routing synchronously (the enriched context feeds the
first `ModelCall`), but `startRouting` races the child-workflow future against
`len(pendingMessages) > 0`. If the user sends a follow-up mid-routing, routing
is **cancelled** and the turn proceeds un-enriched — no making them wait for
enrichment they've already superseded. This is why the signal-drain goroutine
in `turn.go` is now set up *before* the request-pipeline steps.

### Temporal-native mechanics

- **Parallel fan-out** — dispatch the active activities on a cancelable context,
  `workflow.Selector` loop over the futures until all settle or the deadline
  fires. Wall-clock ≈ the slowest single retrieval.
- **Phase deadline** — `workflow.NewTimer(ctx, retrievalPhaseTimeout)` (10s
  placeholder) in the Selector. Unsettled subsystems are cancelled
  (`cancelRetrieval()`, then `workflow.Await` for cancellation to settle — never
  ABANDON) and recorded `timed_out`.
- **Genuine-error vs timed-out** — readiness is snapshotted before the cancel,
  so a future that settled with an error before the deadline is `error`, one
  cancelled for missing it is `timed_out`.
- **Retry** — each activity: `RetryPolicy{MaximumAttempts: 3}`.
- **Cancellation** — `ParentClosePolicy: REQUEST_CANCEL` on the child, plus the
  interrupt race above.
- **Determinism** — `Route()` is pure; all I/O is in activities.

### Per-subsystem status

`SubsystemResult.Status`: `ok` | `empty` | `error` | `timed_out` | `skipped`.
The activity returns `ok` / `empty` / `error`; `RoutingWorkflow` assigns
`timed_out` (missed the deadline) and `skipped` (not in the plan).
`RoutingResult` carries all three subsystems' outcomes so downstream knows
exactly what it's working with — `{memory: ok, tools: error, skills: empty}`,
never a silent gap.

### What's tolerated, what fails

1. **Fan-out subsystem miss — tolerated.** MemoryRetrieve / ToolDiscover /
   SkillDiscover return `empty` or `error`, or miss the phase deadline
   (`timed_out`). Recorded in `RoutingResult`, never a silent gap, and the
   phase continues with whatever landed — these are genuinely additive
   enrichment and a slow backend must not stall the turn.
2. **Interrupt race — tolerated.** A follow-up message arrives mid-routing →
   routing is cancelled, `startRouting` returns a nil error, the turn proceeds
   against the superseding message. A deliberate supersede, not a failure.
3. **`RoutingWorkflow` genuine error — fails the turn.** The fan-out subsystems
   record their own errors into the result and never fail the workflow, so this
   only fires on an infra fault (activity dispatch, Postgres). `startRouting`
   returns it; `turn.go` fails the turn.

### Reference-passing — two categories

**Small derived routing signals** — `taskRep` (intent, complexity, confidence,
`retrieval_query`, `entities`), per-subsystem status, result counts — cross the
workflow freely as activity I/O. Same category as `ModelCallOutput.NextHintTier`
and `ToolCallRef.{Server,Tool}`.

**Bulk retrieved content** — memory item text, rendered skill procedures, tool
descriptions/schemas (KBs) — goes through the `turn_retrieval` staging table
(migration `013`); workflows carry only references + status.

```sql
CREATE TABLE turn_retrieval (
  owner_id   text NOT NULL,             -- the current turn_id (memory/tool) or the plan_id (skill)
  kind       text NOT NULL CHECK (kind IN ('memory', 'tool', 'skill')),
  seq        int  NOT NULL,             -- rank within (owner_id, kind)
  content    text NOT NULL,
  score      real,
  metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_id, kind, seq)
);
```

`activities/activities/retrieval/staging.py` — `write_rows` / `read_rows`
helpers, `ON CONFLICT` upsert so an activity retry re-writes. `prompt.assemble`
reads memory/tool rows `WHERE owner_id = <turn_id>` and skill rows
`WHERE owner_id = <plan_id>`.

### Open Questions

- Phase deadline value — deferred, numeric-tuning discipline.
- Whether `RoutingWorkflow`'s reusability (mid-turn re-retrieval) is ever
  exercised, or it stays a once-per-turn child.
