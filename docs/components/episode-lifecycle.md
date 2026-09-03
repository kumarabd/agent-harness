# Component: Task-Run Lifecycle

> STATUS: folded into [`request-pipeline/08-planning.md`](request-pipeline/08-planning.md),
> which is authoritative. There is no `episodes` table and no "episode" noun in
> the schema — a Deliberate task-run *is* a `PlanWorkflow`. This page keeps only
> the rationale for why a task-run is the unit; read `08-planning.md` for the
> mechanism and `skill-subsystem.md` for the write path.

## Why a task-run, not a turn

The skill subsystem learns a procedure from a *task* — the whole arc of solving
something, however many messages it took. The bug this unit fixes: the pipeline
used to treat every user turn as its own task (its own classify → route →
discover → plan → record). A multi-turn teaching conversation about one obvious
task ("add an idempotency layer") produced **five** disconnected plans and
**four** hyper-specific `learned:*` procedures instead of one general skill,
because nothing linked the turns.

A subagent already worked the right way — one prompt, run to completion,
recorded once; its task and its turn coincide. **A top-level Deliberate task is
a subagent the user steers interactively**: same lifecycle, human turns
interleaved. The old code assumed `turn == task`, true for subagents and false
for interactive work.

## The unit

A Deliberate task-run is one `PlanWorkflow` execution (workflow id
`<plan_id>:plan`, where `plan_id` is the planning turn's id). It owns a PLAN.md
checkpoint ledger, an accumulating trajectory (every turn's messages and tool
calls, `turns.plan_id = <plan_id>`), and a status (the workflow's own execution
state). `RecordSkill` fires **once**, when the run closes, over the whole
trajectory — the fragmentation fix.

- **Is a run in progress for this session?** → is there a running
  `<plan_id>:plan` workflow (`ResolveOpenPlan`, off the latest `turns.plan_id`).
- **Does this message continue it?** → `ClassifyRequest.continues_prior`,
  cross-checked against embedding similarity when confidence is low.
- **New unrelated task while one runs?** → signal the running plan `abandon`,
  start a fresh one.
- **Lite / conversational turn** → a plain `TurnWorkflow`, no plan, no recording.
- **Nested plan** (a `complex` checkpoint, 3C-iii) → its turn ids sit under the
  root's, so the root's one `RecordSkill` prefix-sweeps them; the nested plan
  records nothing of its own.

## What the context loop already covers (so this unit doesn't)

Memory and discovered tools run **per turn**, fresh — `lcm` fold → WriteMemory →
agent-brain (forward) and `MemoryRetrieve` (backward) already keep them current,
so there's no frozen per-run retrieval bundle. Only the plan ledger + trajectory
+ status are run-scoped: a task-run record, not a parallel context system.

Parents: [`request-pipeline.md`](request-pipeline.md),
[`skill-subsystem.md`](skill-subsystem.md),
[`request-pipeline/08-planning.md`](request-pipeline/08-planning.md).
