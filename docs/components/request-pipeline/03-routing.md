# Request Pipeline — Step 3: Routing + Retrieval Orchestration

> STATUS: IMPLEMENTED with stub subsystems. `workflows/internal/workflow/routing.go`
> (`Route`, `RoutingPlan`, `RoutingWorkflow`, `RoutingResult`, `startRouting`),
> registered on the loop-worker, spawned from `turn.go` for top-level turns.
> `turn_retrieval` table = migration `013`. The four subsystem activities
> (`MemoryRetrieve` / `ToolDiscover` / `SkillDiscover` / `ComposeSkill`) are
> registered stubs in `activities/activities/retrieval/` — they return `empty`
> and write nothing until steps 4/5/6/7 fill them in. `RoutingResult` has no
> consumer wired yet (`turn.go` logs and holds it for the planner / assembly).
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md).
> Owns: the `Route()` decision, and `RoutingWorkflow` (steps 3–6).

### Role

Decide which retrieval subsystems this turn needs (memory / skills / tools),
run the active subset in parallel under a phase deadline, compose a skill if
discovery found candidates, and hand `TurnWorkflow` a `RoutingResult` — the
plan plus per-subsystem status. The bulk content is staged to `turn_retrieval`,
read there by the planner (step 8) and prompt assembly (step 9).

### `Route()` — the decision

A **pure deterministic Go function**, `Route(taskRep) RoutingPlan`, in
`routing.go`. No I/O — replay-safe, unit-testable without Temporal
(`routing_test.go`).

```go
type RoutingPlan struct {
    FastPath bool // no enrichment — straight to the reason-act loop
    Memory   bool // step 4
    Skills   bool // steps 5 + 6
    Tools    bool // step 7
}
```

**Router-owned activation, conservative.** The router decides the set; each
subsystem keeps only a cheap *internal* guard ("backend unconfigured", "empty
query") — "I can't run", not policy. A **low-confidence or fallback**
classification (`Confidence < 0.5`, which includes the `Confidence == 0` step-2
neutral fallback) takes the full path so nothing downstream is
under-provisioned. Promote to per-subsystem `ShouldActivate(task)` predicates
only when step 2 carries richer inputs.

**Rule table (v1 — `intent` + `complexity`):**

| `intent` | `complexity` | Route |
|---|---|---|
| `conversational` | any | **fast path** (nothing) |
| `meta` | any | memory |
| `question` | `trivial` / `simple` | memory |
| `question` | `moderate` / `complex` | memory + skills |
| `task` | any | memory + skills + tools |
| any | `confidence < 0.5` | **full path** (memory + skills + tools) |

**Why aggressive skipping is safe:** the fast path *is* today's behavior
(`build_conversation` + the reason-act loop, nothing removed), and the model
keeps full tool access on every path — it can call `memory_search` /
`search_tools` itself mid-turn. A misroute to fast is no worse than the current
harness and self-heals; a misroute to full is a wasted parallel retrieval.
Neither is a correctness bug.

### `RoutingWorkflow` — the orchestration

A **child workflow of `TurnWorkflow`**, spawned by `startRouting` and raced
against a follow-up message (below), for top-level turns only.

```
TurnWorkflow
  ├─ InsertMessage
  ├─ ClassifyRequest → taskRep                     (step 2)
  ├─ startRouting(turn_id, taskRep, &pendingMessages)   → RoutingResult
  │    └─ RoutingWorkflow  child, REQUEST_CANCEL
  │         1. plan := Route(taskRep)
  │         2. if plan.FastPath: return all-"skipped"
  │         3. dispatch the plan's subset, each with
  │            {turn_id, taskRep.RetrievalQuery, taskRep.Entities}:
  │              MemoryRetrieve  ┐  Selector loop over the futures,
  │              ToolDiscover    ├  racing workflow.NewTimer(retrievalPhaseTimeout);
  │              SkillDiscover   ┘  unsettled → cancelled, recorded "timed_out"
  │         4. ComposeSkill(turn_id)  — iff Skills settled "ok" with Count > 0
  │         5. return RoutingResult{ Plan, Memory, Tools, Skills, ComposedSkill }
  ├─ [Planner — step 8]
  └─ reason-act loop
```

`RoutingWorkflow` input is `{turn_id, taskRep}` — small derived routing metadata
(see step 2), not content. Output is the plan + per-subsystem `SubsystemResult`
(`{Status, Count}`) + `ComposedSkill bool` — no content.

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
- **Dependency edge** — `ComposeSkill` runs only after the fan-out completes,
  and only when `SkillDiscover` returned `ok` with `Count > 0` (a plan bit alone
  isn't enough — there may be no matching skeleton). It reads the staged
  memory / tool / skill rows from `turn_retrieval` itself.
- **Retry** — each activity: `RetryPolicy{MaximumAttempts: 3}`.
- **Cancellation** — `ParentClosePolicy: REQUEST_CANCEL` on the child, plus the
  interrupt race above.
- **Determinism** — `Route()` is pure; all I/O is in activities.

### Per-subsystem status

`SubsystemResult.Status`: `ok` | `empty` | `error` | `timed_out` | `skipped`.
The activity returns `ok` / `empty` / `error`; `RoutingWorkflow` assigns
`timed_out` (missed the deadline) and `skipped` (not in the plan).
`RoutingResult` carries all three subsystems' outcomes so the planner knows
exactly what it's working with — `{memory: ok, tools: error, skills: empty}`,
never a silent gap.

### Two degradation layers

1. **Subsystem-level** — an activity returns `empty` or `error`; the phase
   continues with the others.
2. **Phase-level** — `RoutingWorkflow` failing, or the interrupt race firing →
   `startRouting` returns the zero-value `RoutingResult`, `turn.go` logs it, and
   the turn proceeds un-enriched (= today's behavior). Same posture as
   `ClassifyRequest`.

### Reference-passing — two categories

**Small derived routing signals** — `taskRep` (intent, complexity, confidence,
`retrieval_query`, `entities`), per-subsystem status, result counts — cross the
workflow freely as activity I/O. Same category as `ModelCallOutput.NextHintTier`
and `ToolCallRef.{Server,Tool}`.

**Bulk retrieved content** — memory item text, the composed skill procedure,
tool descriptions/schemas (KBs) — goes through the `turn_retrieval` staging
table (migration `013`); workflows carry only references + status.

```sql
CREATE TABLE turn_retrieval (
  turn_id    text NOT NULL REFERENCES turns(turn_id),
  kind       text NOT NULL CHECK (kind IN ('memory', 'tool', 'skill', 'composed')),
  seq        int  NOT NULL,             -- rank within (turn_id, kind)
  content    text NOT NULL,
  score      real,
  metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (turn_id, kind, seq)
);
```

`activities/activities/retrieval/staging.py` — `write_rows` / `read_rows`
helpers. `ComposeSkill` writes its output back as `kind = 'composed'`; the
planner and prompt assembly read `WHERE turn_id = $1`. Rows share the turn's
lifecycle.

### Open Questions

- Phase deadline value — deferred, numeric-tuning discipline.
- `RoutingResult` consumer wiring — lands with steps 8 / 9.
- Whether `RoutingWorkflow`'s reusability (mid-turn re-retrieval, subagent
  retrieval) is ever exercised, or it stays a once-per-turn child.
