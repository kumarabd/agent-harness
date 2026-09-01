# Request Pipeline — Step 8: Planning

> STATUS: DESIGN (2026-08-31). Resolves the "step 8 not designed" open question
> in [`../request-pipeline.md`](../request-pipeline.md).
> **Built:** (1) the subagent gate fix — steps 2 + 3 (`ClassifyRequest`,
> `RoutingWorkflow`) and `RecordSkillOutcome` run for `ParentType == "turn"`
> too; `MemoryRetrieve` inherits the parent's staged `kind='memory'` rows when
> given a `parent_turn_id`. (2) the plan ledger — `turn_plan` table (migration
> `017`), `activities/activities/plan.py`, the `plan_progress` meta-tool
> (`llm.TOOLS_SCHEMA`, peeled out and applied by `ModelCall` — no separate
> activity, same shape as `declare_next_step_hint`), `ComposeSkill` seeds it,
> `build_conversation` renders the progress block, `RecordSkillOutcome` folds
> the final state into the synthesis trajectory. (3) the reconciliation trigger
> — a mid-turn follow-up dispatches a detached `RoutingWorkflow` in
> `Mode="reconcile"` (memory + skills only, re-keyed on the correction,
> replacing the stale bundle; `ComposeSkill` regenerates the composed block but
> not `turn_plan`). **Deferred:** the failure-run half of the reconciliation
> trigger; DAG/parallelism.
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md).
> Depends on: [`06-skill-composition.md`](06-skill-composition.md) /
> [`../skill-subsystem.md`](../skill-subsystem.md) (the plan comes from
> compose), [`03-routing.md`](03-routing.md) (subagent retrieval).
> Feeds: [`../skill-subsystem.md`](../skill-subsystem.md) "Recording" (the
> final ledger state is structured signal for synthesis).

### Role

Give the reason-act loop a **visible, progressing, self-correcting plan** to
execute against — so the harness (and later, synthesis) can tell what the agent
intended, where it is, and when it has diverged — **without** paying for a
separate upfront planning model call on every complex turn.

### The decision: a living ledger, not an upfront planner

Two shapes were considered and rejected:

- **A standalone `Plan` activity** — one expert-tier call after routing that
  turns (task + composed skill + memory + tools) into an ordered step list.
  Rejected: it adds 2–10s + tokens to the head of every moderate/complex turn,
  and for any task with a matching composed skill it mostly re-formats work
  steps 5–6 already did.
- **Recursive decompose-to-leaves** — classify → if complex, decompose into
  subtasks → recurse until subtasks are "simple", execute only at the leaves.
  Rejected: every level decomposes on **assumptions** (no tool has run, no file
  read, no state checked), so the first executing subtask routinely invalidates
  its siblings and the parent must detect that from a lossy summary and re-plan.
  This is the classic plan-and-execute failure mode. Models also over-adhere to
  a written plan even when reality diverges ("plan inertia").

What's built instead: **the plan is a checkpoint ledger the executor
maintains.** It is created cheaply (from compose, no extra call), carried in the
loop's context, advanced as work completes, and revised when execution
diverges. Decomposition into subagents happens **lazily, driven by execution** —
the model spawns a subagent when it *hits* a self-contained detail-heavy slab,
with real context the upfront planner never had.

### Plan representation — a sequential checkpoint list

```
Plan = ordered [ Checkpoint ]

Checkpoint = {
  id:            str,          # stable within the turn
  intent:        str,          # one line — what this step accomplishes
  done_when:     str,          # the observable condition that closes it
  status:        "pending" | "active" | "done" | "revised" | "skipped",
  note:          str | null,   # why revised/skipped, or a correction folded in
}
```

**Sequential, no dependency graph.** A checkpoint is done, active, or not
started; the loop walks them in order. (A DAG — `depends_on` edges, concurrent
independent branches — was considered and deferred: the value is parallelism,
and we're keeping subagent execution synchronous for now. See "Deferred".)

### Where the plan comes from

**From compose (step 6), gated on it.** When `SkillDiscover` found candidates
and `ComposeSkill` ran, compose emits the checkpoint list alongside the composed
prose — it already has the merged procedure's ordered steps, the bound tool
refs, and the filled slots; turning that into `{intent, done_when}` checkpoints
is a formatting step in the same model call, not a new one. The merge call now
returns `{"procedure": "<prose>", "checkpoints": [{intent, done_when}]}`;
`plan.seed` writes the rows as `cp1..cpN`. When the merge tier is unconfigured
or the call fails, checkpoints degrade to the top procedure's own `body` step
instructions (`done_when` blank).

**No skill → no plan.** If routing fast-pathed, or `SkillDiscover` came back
empty, there is no checkpoint ledger and the loop runs exactly as it does today.
Step 8 is strictly additive and never load-bearing — same posture as every other
pipeline phase.

### The loop tracks position

Prompt assembly (step 9, `09-prompt-assembly.md`) reads the current ledger
every `ModelCall` and splices a compact progress block after the composed-skill
block:

```
Plan progress — follow it where it fits, revise it where the task diverges:
  [x] 1. Locate the failing test and its last green commit
  [>] 2. Reproduce the failure locally
  [ ] 3. Bisect to the offending change
  [ ] 4. Fix and confirm green
```

`[>]` marks the first checkpoint not `done`/`skipped` — `plan.render_block`
owns "which is active", the model never reports it.

The model reports advancement via the `plan_progress` meta-tool included
alongside its response: `{"updates": [{checkpoint_id, status, note?, intent?}]}`,
`status ∈ {done, skipped, revised}`. It is handled exactly like
`declare_next_step_hint` — **`ModelCall` peels it out of the tool stream**
(`plan.split_progress_calls`) before minting `tool_calls` rows, applies the
updates to `turn_plan` in their own transaction (`plan.apply_progress`), and it
never counts toward `has_tool_calls`. No separate activity, no `turn.go`
change, no `ModelCallOutput` field — the update is read back by
`build_conversation` on the next call. A `plan_progress`-only response ends the
turn (`no_tool_calls`) after recording the progress — the model marking a final
checkpoint done and stopping. Missing/garbled calls degrade to "no advancement".

**Why a tool call, not parsed prose:** same reasoning as
`declare_next_step_hint` (model-registry.md, "Resolved: Selection Mechanism") —
it rides the existing API round-trip, it's structured, and the model has no
other reliable channel to signal "checkpoint 2 is done, moving to 3".

### Mutation — corrections and failures revise the ledger

- **A checkpoint's `done_when` is met** → the model reports `status: done`; the
  next non-terminal checkpoint renders as `[>]`.
- **The model adds a step** (`plan_progress` update with an unknown
  `checkpoint_id` + an `intent`) → appended at `MAX(checkpoint)+1`. (Insert-at-
  position with renumbering was considered and dropped for v1 — appending is
  enough to record that a step was needed.)
- **A user correction mid-turn** ("no, we deploy via the Makefile") → the model
  reports the affected checkpoint `revised` with the correction in `note`; the
  reconciliation trigger (below) also fires.
- **A failure run** (repeated tool errors on one checkpoint) → the model reports
  it `revised`, and the reconciliation trigger fires.

The ledger is the turn's evolving record of intent-vs-reality, not a fixed
script.

### Feeds synthesis

At turn end, `RecordSkillOutcome` reads the final `turn_plan` and **prepends
`plan.render_final()` to the trajectory transcript** (`PLAN (final state):` +
each checkpoint's ordinal, intent, terminal status, and note). The ledger is a
**cleaner signal than the raw transcript** for "what procedure did this
successful run actually follow" — the checkpoints, in final order, with
`revised`/`skipped`/added steps marked, *is* the effective procedure.
`generalize.py` still runs (the ledger has intents, not full step bodies), but
it now works from a structured skeleton + the transcript rather than
reconstructing structure from prose alone. This is how the skill subsystem's
"whatever the planner produced, including modifications, gets cached" (see
`skill-subsystem.md`, "The reward model") is actually satisfied — the ledger *is*
that artifact.

### Subagents are full agents (the related fix)

**Was:** classification (step 2), routing (step 3), and skill recording were
all gated `input.ParentType == "session"` in `turn.go` — a subagent skipped the
entire pipeline. That's wrong: a subagent handed "investigate why the deploy is
failing and fix it" is a complex task that wants skill discovery (the
`investigate-failure` seed procedure is written for exactly this), memory, and
its own plan.

**Done:** steps 2 + 3 run for `ParentType == "turn"` as well. The subagent's
seed message already exists — `InsertMessage` writes the spawn prompt as
`role='user', seq=0` (insert_message.py), which is precisely what
`ClassifyRequest` reads. `_recent_context` self-skips for a subagent
(`turn_seq == 0 → ""`), and the spawn prompt is self-contained by construction,
so that's correct, not a gap.

- **Classification** decides the subagent's own path. A subagent handed
  "reformat this file" classifies `simple` and fast-paths — same `Route()`
  fast-path bypass as a top-level turn. "Assume simple because subagent" is the
  bug; "this specific task, classified, is trivial" is fine.
- **Retrieval:** the subagent re-runs `SkillDiscover` + `ToolDiscover` (its task
  differs from the parent's), and **inherits the parent's staged
  `kind='memory'` rows** rather than re-querying agent-brain — memory is about
  the user's world, stable across a turn tree, and the front-loaded snapshot is
  a consistent point-in-time capture. `RoutingWorkflow` gets a
  `parent_turn_id` input; when set, it copies the parent's `kind='memory'` rows
  into the child's `turn_retrieval` and skips `MemoryRetrieve`.
- **Skill recording** extends to `moderate`/`complex` subagent turns. Subagent
  tasks are self-contained by construction — prime procedural material,
  currently lost. Their procedures join the **same-session** co-occurrence
  graph (`session_composed_procedure_ids` already keys on session, which the
  subagent shares), so "parent used skill X, its subagent used skill Y" becomes
  a real bundle signal.
- **A subagent can spawn its own subagents** — the `spawn_subagent` nested
  variant (`delegated_scope`/`kept_work` guard) already exists; this just makes
  the spawned agent capable of the full pipeline at each level.

**Spawn criterion (unchanged, worth restating):** a subagent is for a
**self-contained, detail-heavy slab** — a clear boundary, substantial enough to
justify the spawn overhead, loosely coupled to the main line. Two reasons it
pays off: **context isolation** (40 files of exploration never enter the main
window — this is the majority case) and, later, parallelism (deferred).
Sequential *dependent* steps — where step B needs step A's actual output, not a
summary — stay inline in one loop. The unit of delegation is a slab that may be
internally sequential, not a single checkpoint.

**Cost in deep trees:** each subagent adds one fast-tier classify + (if
non-trivial) a retrieval fan-out. Bounded by tree size; the nested-spawn guard
limits depth. A depth cap on *retrieval* (not spawning) is a cheap safety valve
if trees get deep — deferred until observed.

### Reconciliation trigger — mid-turn re-retrieval — BUILT

A mid-turn user follow-up is a course correction — a strong signal that the
front-loaded context no longer fits. When `turn.go`'s loop dequeues a follow-up
(the existing interrupt path: cancel in-flight calls, `InsertMessage`,
`continue loop`), it also dispatches a **detached (`ABANDON`) `RoutingWorkflow`
child with `Mode = "reconcile"`**, keyed `{turnID}:reconcile:{iteration}`.
Skipped when routing never enriched the turn (`routing.Plan.FastPath`).

Reconcile mode:

1. **Skips the `Route()` gate** — `plan = {Memory, Skills}`. No `ToolDiscover`
   (the available capability set didn't change).
2. **Re-keys inside the activities.** `MemoryRetrieve` / `SkillDiscover` get
   `Reconcile: true`; each reads the turn's latest user message itself
   (`retrieval/reconcile.py` — so no message content crosses the workflow
   boundary) and searches on `"{original retrieval_query} / {correction}"` —
   still covers the original task, now sharpened by the correction.
3. **Replaces, not appends.** `replace_rows` swaps all `kind='memory'` /
   `kind='skill'` rows atomically. Appending would bloat the rendered block
   across repeated corrections; a superset query keeps the original coverage
   without the growth. An empty reconcile result leaves the originals in place
   (likelier a noisier query than a staleness signal).
4. **`ComposeSkill` runs with `Reconcile: true`** — regenerates the
   `kind='composed'` block from the replaced rows, but does **not** re-seed
   `turn_plan` (the model has been tracking checkpoints via `plan_progress`;
   overwriting intents/positions mid-turn would desync those reports).
5. `build_conversation` picks the new rows up on the next `ModelCall`.

Detached and not awaited: there's no benefit to blocking the user's correction
on a ≤10s retrieval, and the next `ModelCall` uses whatever landed by the time
it assembles context. This is the only routing-like work that happens *inside*
the loop, and only on an explicit divergence signal. It's what makes the
correction path work *this turn* rather than only feeding next turn's synthesis
(which `record.py`'s `required_correction` flag already does). `RoutingWorkflow`
was built reusable for exactly this (`03-routing.md`, "Why a child workflow").

**Failure-run trigger — deferred.** The doc originally paired "user correction"
with "a run of tool failures on one checkpoint." Built only the correction half:
the correction carries a clean query (the user's own words); a failure run does
not (the error text never reaches the workflow, and "query = the stuck
checkpoint's intent" needs plan reads the reconcile path doesn't have yet).
Add it if failure loops are observed going unrecovered.

### Data model — `turn_plan`

The checkpoint ledger **mutates during the turn**, unlike the write-once
`turn_retrieval` rows. It gets its own small table (migration `017`), mirrored
to `deploy/helm/agent-harness-tenant/files/`.

```sql
CREATE TABLE turn_plan (
  turn_id     text NOT NULL REFERENCES turns(turn_id),
  cp_id       text NOT NULL,            -- stable id the model references in plan_progress ("cp1", "cp2", ...)
  checkpoint  int  NOT NULL,            -- ordinal position, 1-based; seeded contiguous, appended steps get MAX+1
  intent      text NOT NULL,
  done_when   text NOT NULL DEFAULT '',
  status      text NOT NULL DEFAULT 'pending'
              CHECK (status IN ('pending', 'active', 'done', 'revised', 'skipped')),
  note        text,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (turn_id, cp_id)
);
CREATE INDEX turn_plan_order_idx ON turn_plan (turn_id, checkpoint);
```

`activities/activities/plan.py` owns every read/write: `seed` (ComposeSkill),
`apply_progress` (ModelCall), `read` + `render_block` (build_conversation),
`read` + `render_final` (RecordSkillOutcome), `split_progress_calls` (ModelCall).
Rows share the turn's lifecycle (FK to `turns`, no separate cleanup). `status`
is stored as `pending` / `done` / `skipped` / `revised`; `active` is a render-
time marker only, never written.

### Temporal shape

| Unit | Where | Cadence | Does |
|---|---|---|---|
| checkpoint emission | `ComposeSkill` (step 6) | per turn, when a skill composed | merge call returns `{procedure, checkpoints}`; `plan.seed` writes `turn_plan` |
| progress application | inside `ModelCall` | per `ModelCall` whose response carried a `plan_progress` call | `plan.split_progress_calls` peels it; `plan.apply_progress` in its own txn |
| progress rendering | inside `ModelCall` → `build_conversation` | every `ModelCall` | `plan.render_block` splices the block after the composed-skill block |
| reconciliation | detached `RoutingWorkflow` (`Mode="reconcile"`), dispatched by `turn.go` | per mid-turn follow-up | re-keys memory + skills on the correction, `replace_rows` into `turn_retrieval`, `ComposeSkill` regenerates the composed block |
| ledger → synthesis | `RecordSkillOutcome` (skill phase 2) | end of moderate/complex turn | `plan.render_final` prepended to the trajectory transcript |

**Worker placement:** the ledger itself is all tenant-worker — `turn_plan` is
tenant Postgres, and `ComposeSkill` / `ModelCall` / `RecordSkillOutcome` already
run there; the ledger never crosses the workflow boundary. `turn.go`'s only role
is dispatching the reconcile child workflow on a follow-up.

**Degradation:**
- No composed skill → no ledger → loop runs as today.
- `plan_progress` never called / malformed → ledger just doesn't advance; the
  progress block still shows the initial plan, which is still useful scaffolding.
- `plan.apply_progress` raises → caught in `ModelCall`, logged, the model call
  itself still succeeds; the ledger is stale for a step, picked up next call.
- Reconciliation fails → the loop continues with the original context (= the
  current behavior with no reconciliation at all).

### Deferred

- **Failure-run reconciliation** — reconciliation fires only on a user
  follow-up, not on a run of tool failures (see "Reconciliation trigger").
- **Plan as a DAG** — `depends_on` edges, concurrent independent branches
  fanned out as parallel child workflows. The data-model cost is small
  (`depends_on text[]` on a checkpoint) but execution stays sequential/synchronous
  for now; build when there's a concrete parallelism win to measure.
- **An explicit re-plan call** — regenerating the whole ledger mid-turn via a
  model call when it's drifted badly. The incremental revise + reconciliation
  trigger should cover it; add only if ledgers are observed going stale wholesale.
- **Retrieval depth cap for deep subagent trees** — a constant, added when trees
  are observed getting deep enough to matter.
- **Debounced reconciliation** — one reconcile child per follow-up. Fine for the
  usual back-and-forth; if a burst of follow-ups fires several overlapping
  reconciles, dedupe on a fixed workflow id (the skill-synthesis pattern).

### Open Questions

- **A `plan_progress`-only response ends the turn** (`no_tool_calls`). Correct
  when the model marks the last checkpoint done and stops; a latent bug if the
  model ever reports progress *instead of* doing the next step. Watch for it;
  the fix if it bites is to keep the loop alive one more step when
  `plan_updates` is non-empty but `raw_tool_calls` is empty.
- **How compose decides checkpoint granularity** — one checkpoint per procedure
  step is the obvious default; whether to collapse trivial adjacent steps is a
  prompt-tuning question, deferred.
- **Insert-at-position for added steps** — v1 appends. If the model frequently
  adds steps that belong mid-plan, revisit with an `after` field + renumber.
- **Whether the progress block should show *only* the plan or also a running
  "steps taken" tail** — the LCM verbatim window already carries recent tool
  calls; duplicating them in the progress block is probably noise. Start with
  plan-only.
- **Failure-run threshold for the reconciliation trigger** — how many retries
  on one checkpoint before it fires. Numeric-tuning discipline; start at 2.
