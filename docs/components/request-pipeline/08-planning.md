# Request Pipeline — Step 8: Planning

> STATUS: REVISED 2026-09-02 — **plan-and-execute orchestrator.** This replaces
> the "living ledger, not an upfront planner" design (2026-08-31).
>
> **Phase 3 slices A–D + 3C-ii approval gate + mid-plan fold-in + 3C-iii
> checkpoint recursion BUILT 2026-09-03** (branch `proactivity-substrate`; `go
> build ./...` + `go test ./internal/...` green, `activities/` compiles; **NOT
> deployed, NOT live-verified**). Wiring = **B** (the coordinator classifies + dispatches —
> the supervisor/entity pattern):
> - `workflow/dispatch.go` `dispatchWork` — `InsertMessage` → `ClassifyRequest`
>   (no fallback) → conversational ? plain `TurnWorkflow` : `ResolveOpenPlan` →
>   {Attach (signal the running `<plan_id>:plan`) | Supersede (`abandon` signal) |
>   Lite → plain `TurnWorkflow` | fresh Deliberate → `PlanWorkflow` id
>   `<turn_id>:plan`}. Top-level only; subagents keep the turn.go path (a
>   Deliberate subagent opens its own plan_id = its turn id, single loop).
> - `workflow/plan_workflow.go` `PlanWorkflow` — planning turn (turn_id ==
>   plan_id, `PlanningMode`) → **approval gate** → loop {**fold in a mid-plan
>   follow-up** → `NextCheckpoint` → checkpoint child `TurnWorkflow` id
>   `<plan_id>:cp:<n>`} → `dispatchRecordSkill` + `PlanDone` signal. Also
>   listens for the `abandon` signal (supersede). Registered on loop-worker.
> - **Approval gate** (`runApprovalGate`): pure reuse of `UserInputRequestWorkflow`
>   — the same primitive permission gating uses. `propose_plan`'s
>   `needs_approval` rides out on `ModelCallOutput.NeedsApproval` →
>   `TurnResult.NeedsApproval` (a control bool, no PLAN.md state, no new
>   activities). If set, PlanWorkflow parks on `UserInputRequestWorkflow` kind
>   `plan_approval` (options approve / reject, free text = a revision
>   instruction). approve → run. free-text → a `<plan_id>:replan:<n>` planning
>   turn seeded with the feedback, then re-gate (cap `planApprovalRevisionCap`
>   = 3, then proceed). reject / expire / cancel → wrap up, `RecordSkill`
>   close_reason `rejected`.
> - **Mid-plan follow-up fold-in** (`foldInFollowups`): at each checkpoint
>   boundary, if `NewMessage` signals queued to `pending`, drain them into one
>   `<plan_id>:followup:<n>` turn (`TurnInput.PlanHandling` — a normal
>   reason-act turn that also gets `propose_plan`); its answer is `Deliver`ed
>   and it may re-`seed` the plan (merge preserves completed checkpoints).
> - **The `episodes` table is GONE** (migration `025`): a Deliberate task-run
>   *is* a `PlanWorkflow`, Temporal's own execution state answers "is a run in
>   progress". `turns.episode_id` → `turns.plan_id`. `OpenEpisode` /
>   `CompleteEpisode` / `episode.py` deleted; `ResolveOpenPlan` (plan_resolve.py)
>   replaces the continuation check.
> - **`ComposeSkill` is GONE** (routing.go, `compose.py`, `reconcile.py`
>   deleted): `SkillDiscover` stages `kind='skill'` rows under the plan_id and
>   `prompt.assemble` feeds them to the planning turn.
> - **The meta-tools changed**: `plan_progress` → **`propose_plan`** (planning
>   turn / re-plan / fold-in: `{checkpoints:[{intent,done_when}], needs_approval}`
>   → `plan.seed`, which merges so completed checkpoints survive a re-plan) +
>   **`checkpoint_done`** (checkpoint turn only: `{checkpoint_id, status, note?,
>   revised_tail?}` → `plan.apply_checkpoint_done`; `revised_tail` replaces every
>   still-pending checkpoint after this one). `tools_schema_for` gives each turn
>   kind exactly the meta-tool it should see (base `TOOLS_SCHEMA` carries
>   neither); `ModelCall` peels both regardless and applies per turn mode.
>   `plan.py` is filesystem-backed (PLAN.md at
>   `$SESSION_ROOT/session/<key>/plans/<plan_id ':'→'_'>/PLAN.md`; `turn_plan`
>   dropped, migration `023`).
> - Slice A (`RecordSkill` collapse — one online activity, no `skill_candidates`,
>   migration `022`) also done.
> - **3C-iii checkpoint recursion** (`runCheckpoint` / `runNestedPlan`): the
>   planning model flags a checkpoint `complex:true` in `propose_plan`;
>   `NextCheckpoint` returns that flag; PlanWorkflow runs that checkpoint as a
>   nested `PlanWorkflow` (id `<plan_id>:cp:<n>:sub:plan`, `Depth+1`, cap
>   `maxPlanDepth=2`) instead of a flat turn. The nested plan is an **opaque
>   single task** — no approval gate, no `PlanDone` signal, no `RecordSkill` of
>   its own; on its return the parent calls the `MarkCheckpointDone` activity.
>   Every turn's id sits under the root's, so the root's one `RecordSkill`
>   prefix-sweeps the whole tree (`starts_with(t.plan_id, root || ':')`) — a
>   deep task is learned as one skill, deliberately. A follow-up / `abandon`
>   while a nested plan runs → the parent **cancels** the child (`runNestedPlan`
>   selects on a wake channel) and folds in / breaks at the boundary.
> - **STILL OPEN**: scenarios not reworked (the scenario starter can't script a
>   `PlanWorkflow`'s checkpoint turns); deploy + live-verify. The gateway
>   renders `plan_approval` requests via the kind-agnostic user-input path (no
>   bespoke UI yet).
>
> **Chain.** [`../episode-lifecycle.md`](../episode-lifecycle.md) REVISION
> (2026-09-02) removed the episode's staged retrieval bundle. This doc removes
> the in-loop advisory ledger *and* the `episodes` table. A task-run is now
> exactly a `PlanWorkflow` = **PLAN.md + trajectory + Temporal status**.
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md).
> Feeds: [`../proactivity.md`](../proactivity.md) — the `PlanWorkflow` is the
> thing a scheduled wake advances.

### Why the reversal

The 2026-08-31 design made the plan **context**: a checkpoint list spliced into
the reason-act loop's prompt, advanced by the model via a `plan_progress` tool,
inside one `TurnWorkflow` per user message. It rejected an upfront planner and
plan-and-execute for three reasons — the cost of an extra model call on every
complex turn, plans built on assumptions that the first executing step
invalidates, and model "plan inertia" (over-adhering to a written plan when
reality diverges).

Two things changed:

1. **The orchestrator vision** (`../04-architecture-orchestrator-vision.md`) and
   **proactivity** both need the plan to be a **real control structure** — a
   durable object that a scheduled wake, an idle resume, or a supervising user
   can *advance, pause, and revise*. A ledger the model narrates inside one loop
   is none of those.
2. The rejection's failure modes now have concrete mitigations the old design
   didn't weigh — see "How the old objections are handled".

### The model

The Lite lane is unchanged — one reason-act loop, no plan. **Deliberate lane:**

```
OpenEpisode (Deliberate, new episode)
      │
      ▼   CoordinatorWorkflow starts a PlanWorkflow  (not a plain TurnWorkflow)
PlanWorkflow:
  1. SkillDiscover ───────────────────▶ candidate procedures                 [S read]
  2. PLANNING TURN   (child TurnWorkflow, seed = "draft a plan for: <task>")
        reads task + retrieved skills + memory + discovered tools + lcm
        ends by calling  propose_plan({checkpoints:[{intent, done_when}], needs_approval})
  3. plan_write activity ─────────────▶ /sessions/<key>/plans/<episode_id>/PLAN.md (draft)
  4. APPROVAL GATE
        small / obvious / "just go"  ──▶ auto-approve
        otherwise                    ──▶ UserInputRequestWorkflow(kind="plan_approval")
                                         user: approve · edit · reject
  5. EXECUTION LOOP  — each non-terminal checkpoint, in order:
        a. pending user signal?  ──▶ handle in a turn  ──▶ re-plan the tail
        b. CHECKPOINT TURN  (child TurnWorkflow, seed = checkpoint + rendered plan)
              └─ its own ClassifyRequest says moderate|complex?
                    ──▶ child PlanWorkflow (auto-proceeds, own episode, own PLAN.md, own RecordSkill)
        c. turn calls checkpoint_done({status, note?, revised_tail?})
        d. plan_write applies it to PLAN.md
  6. all checkpoints terminal
        ──▶ dispatch RecordSkill  (async, ABANDON)
        ──▶ signal CoordinatorWorkflow: episode_complete
```

### PLAN.md — the store

One file per episode at `/sessions/<session-key>/plans/<episode_id>/PLAN.md` on
the tenant PV (the volume already mounted at `/sessions`). **It is the store, not
a mirror** — the same anti-redundancy rule the episode revision applied to
`turn_retrieval`. `turn_plan` (migration `017`) is dropped.

Why a file, not a table:

- greppable and editable by `shell_exec` and by any delegated Claude Code working
  the same task (`../software-engineering.md`);
- human-readable — the user reads and edits the exact artifact they approved;
- lives in the episode's workspace dir, naturally versioned alongside its work.

```markdown
# Plan — <one-line task statement>
status: executing          # draft | awaiting-approval | executing | complete | abandoned

- [x] 1. Locate the failing test and its last green commit
- [>] 2. Reproduce the failure locally
- [ ] 3. Bisect to the offending change
      done_when: the first bad commit is identified
- [ ] 4. Fix and confirm green
```

`[>]` marks the first non-terminal checkpoint — render-time only. A checkpoint
delegated to a child gets `→ episode <child_episode_id>` appended. The `episodes`
row keeps only `status` + `plan_path` — the pointer, nothing copied.

### PlanWorkflow

| aspect | |
|---|---|
| **one per** | Deliberate episode — top-level, or a recursing checkpoint |
| **workflow id** | `<episode_id>:plan` — discoverable from the open `episodes` row |
| **spawns** | plain child `TurnWorkflow`s: the planning turn, then one per checkpoint |
| **signals in** | `advance` (wake / resume — run the next checkpoint), `message` (user follow-up — to the active checkpoint turn, or handled at the next boundary), `revise` (re-plan the tail), `abandon` (supersede) |
| **signals out** | to `CoordinatorWorkflow`: `episode_complete`, `needs_user` (approval, or a blocking question) |
| **on completion** | dispatch `RecordSkill` (`ABANDON`), signal the coordinator, exit |
| **worker** | loop-worker — pure orchestration, no tenant credentials; PLAN.md I/O is in `plan_write` / `plan_read` activities and in the turns, never in the workflow (per the worker-placement rule: it's about touching tenant data, not workflow-vs-activity) |

**Why this is the proactivity seam.** A `PlanWorkflow` mid-execution and idle
(blocked on `advance`) is exactly what a scheduled wake targets — *"resume plan
`<episode_id>` at checkpoint N"*. Deliberation deciding to pick an abandoned task
back up just sends `advance`. No separate machinery — proactivity's substrate is
mostly "who sends the wake".

An idle `PlanWorkflow` outlives the coordinator's idle-exit (`ABANDON` child). A
later user message recreates the coordinator via `SignalWithStart`; it reads the
open episode, finds the running `PlanWorkflow` by id, and resumes signalling it.

### The planning turn

A normal child `TurnWorkflow`, `initiated_by='plan'`, seeded with a system-role
message: *"Draft a checkpoint plan for this task. Ground it in the suggested
procedures below where they fit; keep each checkpoint observable (`done_when`).
You'll present it for approval."* Its context is the retrieved skills, memory,
discovered tools, and the `lcm` conversation. It ends by calling
`propose_plan({checkpoints, needs_approval})` — peeled by `ModelCall` the same
way the old `plan_progress` was, handed back to the `PlanWorkflow`.

**Skills are input to a draft, never executed verbatim.** This is the core
mitigation of the old "decompose on assumptions" objection — a matched procedure
shapes the draft; it does not *become* the execution.

### Approval gate

- **Auto-approve** when the planning turn sets `needs_approval=false` — a
  small/obvious plan (few checkpoints, all low-risk, high planning confidence),
  or the user's message granted "just go" / standing approval for the session.
- **Otherwise** the `PlanWorkflow` signals `needs_user`; the coordinator raises a
  `UserInputRequestWorkflow` of kind `plan_approval` (the primitive already
  behind permission gates), showing the rendered PLAN.md. The user approves,
  edits (the edited file is written back and becomes the plan), or rejects (the
  episode closes `abandoned`; no `RecordSkill`).

This approval is the **first concrete instance of proactivity** — the agent
initiating a message and waiting on the user — and it rides machinery that
already exists.

### Checkpoint execution

Each non-terminal checkpoint → one child `TurnWorkflow`, `initiated_by='plan'`,
seeded with the checkpoint's `intent` + `done_when` + a rendered view of the
whole plan (so it knows where it sits). The turn runs the full reason-act loop —
`lcm.assemble` + per-turn `MemoryRetrieve` + per-turn `ToolDiscover`, i.e. the
"Lite lane after routing" behaviour.

- **Recursion.** If the checkpoint turn's own `ClassifyRequest` returns
  `moderate|complex`, it starts a child `PlanWorkflow` instead of running inline.
  That sub-plan **auto-proceeds — no approval gate** (the parent plan was
  approved; sub-plans are implementation detail). It opens its own episode, its
  own PLAN.md, and fires its own `RecordSkill` on completion.
- **Marking done.** The turn calls `checkpoint_done({status, note?})` —
  successor to `plan_progress`, same peel mechanism — and `plan_write` records it
  in PLAN.md.
- **Re-planning the tail.** The same call may carry
  `revised_tail: [{intent, done_when}]`, which replaces every *later*
  non-terminal checkpoint. **This is where "the first executing step invalidated
  its siblings" is fixed** — every checkpoint boundary is a re-plan opportunity
  with real execution results in hand.

### Interruption

| when | handling |
|---|---|
| mid-checkpoint-turn | the user message is forwarded into the running child turn (the existing interrupt path) |
| at a checkpoint boundary | `PlanWorkflow`'s `message` signal → spawn a handling turn → it may `revise` the tail → the loop resumes |
| "stop / different thing now" | `abandon` → the episode closes `superseded`; `RecordSkill` still fires on the completed portion (a partial procedure is still signal) |

### Completion → RecordSkill (collapsed)

One async activity, dispatched detached (`ABANDON`) when the plan reaches
`complete`. It **replaces** the `RecordSkillOutcome` → `skill_candidates` →
`SkillSynthesizeWorkflow` (debounced) chain with a single step:

- **input:** final PLAN.md + the episode trajectory (all its turns) + outcome.
- embeds the task, matches against existing `skill_procedures`:
  - **match** → reinforce (bump confidence / EMA, fold in new or revised
    checkpoints);
  - **no match** → insert a new procedure whose body **is** the final checkpoint
    list.
- no candidates table, no synthesis debounce — *"record the procedure that was
  actually followed"*, directly.

Match-or-insert keeps the "multiple similar runs converge on one procedure"
benefit the candidates table used to provide, without the staging.

### How the old objections are handled

| 2026-08-31 objection | now |
|---|---|
| an upfront planner call costs 2–10s on every complex turn | it is **one** planning turn per *episode*, not per user turn, and it replaces `ComposeSkill`'s prose generation — roughly net-even |
| plans built on assumptions; the first step invalidates its siblings | **per-checkpoint tail re-planning** (`revised_tail`) — the plan is re-derived with real results at every boundary |
| model plan inertia | checkpoints are `done_when`-observable and the turn is told "revise where the task diverges"; the tail is *expected* to change |
| decompose-to-leaves recursion explodes | recursion only when a checkpoint *classifies* `moderate|complex` — most checkpoints are one inline turn; depth is bounded by the existing nested-spawn guard |

### Data model

- **`turn_plan` — dropped** (a later migration reverts `017`); PLAN.md replaces
  it.
- **`turn_retrieval` — dropped** (per `../episode-lifecycle.md` REVISION).
- **`skill_candidates` — dropped**; `SkillSynthesizeWorkflow` — removed.
- `episodes` gains `plan_path text`.
- `turns.initiated_by text` ∈ `{user, plan, schedule, agent}`.
- **New:** `PlanWorkflow` (loop-worker); `RecordSkill` activity (tenant-worker);
  `plan_write` / `plan_read` activities (tenant-worker); `propose_plan` /
  `checkpoint_done` meta-tools replacing `plan_progress`.

### Temporal shape

| unit | where | does |
|---|---|---|
| `PlanWorkflow` | loop-worker | orchestrates planning turn → approval → checkpoint turns → RecordSkill; holds the episode's execution state; resumable via `advance` |
| planning turn / checkpoint turns | normal `TurnWorkflow` children | reason-act; `initiated_by='plan'` |
| approval | `UserInputRequestWorkflow`, kind `plan_approval` | existing primitive |
| `plan_write` / `plan_read` | tenant-worker activities | PLAN.md I/O on the tenant PV |
| `RecordSkill` | tenant-worker activity, detached `ABANDON` | final PLAN.md + trajectory → match-or-insert `skill_procedures` |

### Degradation (no fallback)

- The planning turn fails to produce a valid `propose_plan` → bounded retry →
  the episode fails and surfaces (same posture as `ClassifyRequest`).
- A checkpoint turn fails → the `PlanWorkflow` marks that checkpoint `failed` in
  PLAN.md, stops, and signals `needs_user` with the failure. No "skip and
  continue", no "retry forever".
- `RecordSkill` fails → logged; the episode is already closed; that run is lost
  to the skill store but nothing user-facing breaks.

### Deferred

- **Parallel checkpoints** — independent checkpoints fanned out as concurrent
  child turns (`depends_on` in PLAN.md). Execution stays sequential for now.
- **Procedure generalization (topic → shape)** — `RecordSkill` inserts the
  checkpoint list as-is; `trigger_text` stays topic-anchored (the deferred
  `generalize.py` altitude fix, unchanged).
- **High-confidence plan templating** — a strongly-matched procedure seeds the
  *draft* directly, skipping the planning turn.
- **Cross-session plan resumption** — picking an `abandoned` episode's
  `PlanWorkflow` back up days later; needs the workspace and the workflow to
  outlive the session. Folded into proactivity's deliberation work.

### Open Questions

- **`PlanWorkflow` reconnect after coordinator idle-exit** — confirm the
  recreated coordinator rediscovers the running `PlanWorkflow` from the open
  `episodes` row and resumes signalling cleanly.
- **The "small / obvious" threshold** for auto-approval — starts as the planning
  turn's `needs_approval` judgment; may need a hard checkpoint-count / risk gate
  on top.
- **Edited-plan trust** — when the user edits PLAN.md at the gate, is it
  re-validated (well-formed checkpoints) or taken verbatim?
- **`RecordSkill` match threshold** — the cosine floor for reinforce-vs-insert;
  start at the `skill_select` value, tune with data.
- **Sub-plan double-counting** — a recursing checkpoint records its own procedure
  *and* sits in the parent's trajectory; the child's distinct `episode_id`
  already excludes it from the parent's gather (as today) — confirm that holds.
