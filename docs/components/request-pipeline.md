# Component: Request Pipeline

> STATUS: **All 9 pre-LLM phases (2–9) are built.** (2–4, 7 implemented; 5–6 =
> the skill subsystem, phases 1–5; 8 = the living plan ledger +
> subagents-as-full-agents + reconciliation trigger; 9 = prompt assembly's
> section model + budget arbitration.) Steps 1, 10, 11 already exist under
> other components. Every phase is additive and degrades cleanly to today's
> behavior with its inputs absent — see "Degradation is layered" below.
>
> ## REVISION (2026-09-02) — plan-and-execute. Design agreed, not built.
>
> Authority: [`08-planning.md`](request-pipeline/08-planning.md) (REVISED) and
> [`episode-lifecycle.md`](episode-lifecycle.md) (REVISION block). In brief, the
> whole staging model is being removed:
>
> - **`turn_retrieval` — dropped.** The staged bundle duplicated the
>   `lcm` → `WriteMemory` → agent-brain → `MemoryRetrieve` loop.
> - **`MemoryRetrieve` + `ToolDiscover` — per turn, fresh**, injected directly by
>   `prompt.assemble`. Not staged, not fanned out for staging.
> - **`SkillDiscover` — once per episode**, feeding a **planning turn**.
> - **`ComposeSkill` / step 6 — removed.** Merging retrieved procedures into a
>   coherent plan is the planning turn's job.
> - **`turn_plan` — dropped.** Replaced by **PLAN.md**
>   (`/sessions/<key>/plans/<episode_id>/`), executed by a new **`PlanWorkflow`**
>   (loop-worker). `plan_progress` → `propose_plan` + `checkpoint_done`.
> - **`RecordSkillOutcome` + `skill_candidates` + `SkillSynthesizeWorkflow` —
>   collapsed** into one async **`RecordSkill`** (embed task → match→reinforce /
>   no-match→insert `skill_procedures`).
> - **`RoutingWorkflow`'s fan-out/staging role and `Mode="reconcile"`** are
>   largely obsolete; its surviving shape is an open question for the next pass.
>
> Phase docs below still describe the staging model; the two authority docs win
> where they conflict.
>
> This file is the index and the cross-cutting contract. Each phase has its own
> doc:
> - [`02-request-understanding.md`](request-pipeline/02-request-understanding.md) — **implemented**
> - [`03-routing.md`](request-pipeline/03-routing.md) — the router + `RoutingWorkflow` orchestration
> - [`04-memory-retrieval.md`](request-pipeline/04-memory-retrieval.md)
> - [`05-skill-discovery.md`](request-pipeline/05-skill-discovery.md) — pipeline contract; design in `skill-subsystem.md`
> - [`06-skill-composition.md`](request-pipeline/06-skill-composition.md) — **REMOVED 2026-09-02** (job absorbed into the planning turn)
> - [`../skill-subsystem.md`](skill-subsystem.md) — **"The Skill Graph"**, the full design for step 5 (+ recording/synthesis, being collapsed into `RecordSkill`)
> - [`07-tool-discovery.md`](request-pipeline/07-tool-discovery.md)
> - [`08-planning.md`](request-pipeline/08-planning.md) — **REVISED 2026-09-02**; plan-and-execute orchestrator (`PlanWorkflow` + PLAN.md + approval gate)
> - [`09-prompt-assembly.md`](request-pipeline/09-prompt-assembly.md) — **built**; the section model + budget arbitration (composed-skill section removed)
> - [`../episode-lifecycle.md`](episode-lifecycle.md) — **built + REVISION 2026-09-02**; the episode owns only plan + trajectory + status (staged retrieval removed)
> - [`../lane-model.md`](lane-model.md) — **design**; two lanes — **Lite** (memory only) and **Deliberate** (full pipeline + episode + RL). Skills / tools / plan ledger / episodes / RL recording are all Deliberate-only; Deliberate is `(task, moderate|complex)` + `(question, complex)` + `conf<0.5`

### Role (one line)

Everything between "a user message arrives" and "the reason-act loop runs its
first `ModelCall`" — understanding the request, deciding which subsystems to
consult, retrieving from each, composing a task-scoped skill, and assembling the
final prompt — so the model is handed a specialized, self-contained package
rather than a generic prompt plus "figure it out."

### Why this exists (recap)

Today the harness does almost no pre-processing: `llm.build_conversation` is a
fixed four-part merge (system prompt + session-start memory block + LCM
conversation + full tool schema), and routing/planning happen implicitly inside
the model's own reason-act loop (`turn.go`). That works but leaves real gaps —
no task representation, no skill surface, no task-scoped memory or tool
retrieval, no composed procedure. This component is where those stages land,
added **one at a time, each additive and each degrading cleanly to today's
behavior when its inputs are absent.**

### The pipeline

```
              USER QUERY
                  │
                  ▼
    ┌─────────────────────────┐   1. Query arrives + session context
    │ Request Understanding   │   2. ← Task classification (fast-tier LLM)
    └───────────┬─────────────┘
                ▼
           ┌─────────┐             3. Routing — Route(taskRep) → RoutingPlan
           │ Routing │                (+ fast-path bypass for trivial turns)
           └────┬────┘
     ┌──────────┼───────────┐        RoutingWorkflow (child of TurnWorkflow)
     ▼          ▼           ▼         fans out the active subset in parallel:
  MEMORY      SKILL       TOOL        4. memory retrieval
 RETRIEVAL   DISCOVERY   DISCOVERY    5. skill discovery
     │          │           │         7. tool discovery
     │          ▼           │
     │      6. compose  ◄───┘         6. skill composition (needs memory + tools)
     └──────┬───┘
            ▼
     RoutingResult  → staged bundle + per-subsystem status
            │
            ▼
      PLAN LEDGER                     8. Planning — compose emits a checkpoint
            │                            ledger; the loop tracks + revises it
            ▼
      PROMPT ASSEMBLY                 9. prompt.assemble — ordered sections,
            │                            budget-shed capabilities/memory first
            ▼
           LLM                        10. Model execution (turn.go loop)
            │                             — reports plan_progress each step;
            │                               a correction/failure re-fires retrieval
            │
     ┌──────┴──────┐
     ▼             ▼
  TOOL CALL     RESPONSE
     │
     ▼
  MEMORY WRITE-BACK                   11. Persist what should become memory
```

### Phase status

| # | Phase | Doc | Status |
|---|---|---|---|
| 1 | Query arrives + session context | — (Gateway / Coordinator / LCM) | done |
| 2 | Request understanding | `02-request-understanding.md` | **implemented** |
| 3 | Routing + retrieval orchestration | `03-routing.md` | **implemented** (`RoutingWorkflow`; subsystems live) |
| 4 | Memory retrieval | `04-memory-retrieval.md` | **implemented** (`MemoryRetrieve` → agent-brain, stages `kind='memory'`) |
| 5 | Skill discovery | `05` + `skill-subsystem.md` | built (flat-cosine + scoring → `kind='skill'`). **REVISED: runs once per episode, feeds the planning turn; `turn_retrieval` staging dropped.** |
| 6 | Skill composition | `06` + `skill-subsystem.md` | ~~built~~ **REMOVED (2026-09-02)** — merging retrieved procedures into a plan is the planning turn's job. `ComposeSkill` deleted. |
| 7 | Tool discovery | `07-tool-discovery.md` | built (`ToolDiscover` → `discover_tools`). **REVISED: per turn, fresh, injected by `prompt.assemble`; staging dropped.** |
| 8 | Planning | `08-planning.md`, `episode-lifecycle.md` | **REVISED (2026-09-02) — plan-and-execute.** `PlanWorkflow` (loop-worker) runs a planning turn → PLAN.md (replaces `turn_plan`) → approval gate → one child turn per checkpoint w/ tail re-planning → one async `RecordSkill`. The 2026-08-31 living-ledger build (migration `017`, `plan.py`, `plan_progress`) is superseded. |
| 9 | Prompt assembly | `09-prompt-assembly.md` | built. **REVISED: no "composed skill" section (removed); "plan progress" → "current checkpoint" from PLAN.md; capabilities + memory always per-turn fresh.** |
| 10 | Model execution | `components/temporal-workflow.md` | done |
| 11 | Memory write-back | `components/memory-slot.md` | partial |

### Cross-cutting contract (applies to every phase)

**Worker placement — by data, not by primitive.** The real rule is *"does this
step touch tenant credentials or tenant data?"*, not "is it a workflow or an
activity." It happens that all orchestration (the `TurnWorkflow` /
`RoutingWorkflow` control flow, the `Route()` decision, fan-out, timers) is
tenant-agnostic and runs on the shared **loop-worker**, and all the real work
(every activity: `ClassifyRequest`, `MemoryRetrieve`, `ToolDiscover`,
`SkillDiscover`, `ComposeSkill`, and later `Plan` / `AssembleContext`) needs
per-tenant credentials (agent-brain, mcp-hub, model tiers) or tenant Postgres
and runs on the per-tenant **tenant-worker** (Python). A tenant-agnostic step
*could* live on loop-worker (a Go local activity, say) — none do yet only
because none are tenant-agnostic. The reason-act loop is not special in this
split: it is `TurnWorkflow` code on loop-worker dispatching `ModelCall` /
`ToolCall` activities to tenant-worker — the exact same shape as the retrieval
phase.

**Degradation is layered.** Every phase degrades to "today's behavior" when its
inputs or backends are absent:
- *Subsystem-level* — a retrieval activity settles to `empty` (nothing relevant
  / backend unconfigured) or `error` (configured, but failed after retries),
  never a silent gap; the status is carried forward.
- *Phase-level* — `RoutingWorkflow` itself failing, or its whole deadline
  blowing, is caught by `TurnWorkflow`, logged, and the turn proceeds with an
  empty bundle. Enrichment is never load-bearing.

**Reference-passing — two categories.** *Small derived routing signals* — step
2's `TaskRepresentation` (intent, complexity, `retrieval_query`, `entities`),
per-subsystem status, result counts — cross the workflow freely as activity I/O,
the same category as `ModelCallOutput.NextHintTier` and
`ToolCallRef.{Server,Tool}`. *Bulk retrieved content* — memory item text, the
composed skill, tool schemas — goes through a turn-scoped staging table
(`turn_retrieval`); workflows carry only references + status, and it never
enters loop-worker memory. See `03-routing.md`.

**Best-effort, never a gate.** No phase can fail or block a turn. The worst case
for any misroute or missing backend is the current, un-enriched harness.

### Open Questions / To Design

- **A pre-step-2 non-LLM filter** for obviously trivial messages ("hi",
  "thanks") — would save even the `ClassifyRequest` call. The deferred Tier-1
  trigger shared with `context-slot.md` / `memory-slot.md`.
- **Whether steps 2–9 eventually collapse into one `PrepareTurnWorkflow`** —
  deferred; all nine phases now exist as peers (2–3 as workflow/orchestration,
  4–9 as activities dispatched from `RoutingWorkflow` or `ModelCall`) with no
  friction observed yet that a merge would fix.

### Notes Log

- 2026-09-02: **Plan-and-execute revision.** See the REVISION banner at the top
  and `08-planning.md` / `episode-lifecycle.md`. Staging model (`turn_retrieval`,
  `turn_plan`) removed; `MemoryRetrieve`/`ToolDiscover` go per-turn;
  `SkillDiscover` feeds a planning turn once per episode; `ComposeSkill` deleted;
  a `PlanWorkflow` executes an approved PLAN.md checkpoint-by-checkpoint;
  recording collapses to one async `RecordSkill`. `RoutingWorkflow`'s future
  shape is open. Not built.
- 2026-09-01: **Episodes — `episode-lifecycle.md`. Fixes multi-turn synthesis
  fragmentation.** The plan ledger + staged retrieval (`turn_plan` /
  `turn_retrieval`, key column renamed `turn_id` → `episode_id`, migration
  `018`) and `RecordSkillOutcome` become **episode-scoped**: one episode = one
  task from first message to completion. New `episodes` table + `turns.episode_id`;
  `activities/episode.py` + `OpenEpisode` / `CompleteEpisode` /
  `CloseSubagentEpisode` / `CloseSessionEpisodes` activities;
  `CloseSessionEpisodesWorkflow` dispatched on the coordinator's idle-exit.
  `turn.go`: after classify, a non-conversational turn opens a new episode or
  attaches to the session's open one (`ClassifyRequest` gains `continues_prior`;
  degraded path uses embedding similarity). A new-episode turn runs the full
  pipeline once; a continuation turn only fires a reconcile refresh.
  `RecordSkillOutcome` fires once, at episode close, over the whole multi-turn
  trajectory. `08-planning.md` partly superseded; `skill-subsystem.md`
  "Recording" updated.
- 2026-09-01: **Step 9 built — `09-prompt-assembly.md`. All 9 pre-LLM phases
  now built.** New `activities/activities/prompt.py`: `assemble(conn, turn_id,
  system_prompt, context_window)` — explicit section model (composed skill,
  plan progress — never shed; capabilities hint, memory — shed in that order
  under budget pressure), replacing the ad-hoc stack of
  `conversation.insert(1, ...)` calls. **New capabilities section** renders
  staged `kind='tool'` rows as a plain hint block (`- server/tool — desc`) —
  closes the gap `07-tool-discovery.md` flagged (`ToolDiscover`'s output never
  reached the prompt except via a composed skill's tool_ref bindings; a
  `question` turn or a skill-less `task` got tool discovery for nothing).
  Budget: `ENRICHMENT_BUDGET_FRACTION=0.25` of `context_window`; `0`
  (unresolved tier / fixture path) means no shedding. `llm.build_conversation`
  is now a thin call-through kept as `model_call.py`'s stable call site.
  `model_call.py`: moved model-tier resolution (`model_config`/
  `context_window`) before the `build_conversation` call — small,
  behavior-preserving reorder so assembly can see the budget. No Go changes —
  runs entirely inside `ModelCall`. Verified via a monkeypatched end-to-end
  smoke test (section order; shedding under a tiny `context_window`); no
  Python test infra exists in this repo.
- 2026-08-31: **Step 8 built.** (a) *Subagent gate fix* — `turn.go` runs steps
  2 + 3 and `RecordSkillOutcome` for `ParentType == "turn"`; `MemoryRetrieve`
  gained `parent_turn_id` → copies the parent's staged `kind='memory'` rows
  (`_inherit`) instead of calling agent-brain; `RoutingWorkflowInput.ParentTurnID`
  threads it. (b) *Plan ledger* — `turn_plan` table (migration `017`, mirrored),
  `activities/activities/plan.py` (`seed` / `apply_progress` / `read` /
  `render_block` / `render_final` / `split_progress_calls`), `plan_progress`
  meta-tool in `llm.TOOLS_SCHEMA`. `ModelCall` peels `plan_progress` out of the
  tool stream (like the providers strip `declare_next_step_hint`) and applies it
  to `turn_plan` in its own transaction — no new activity, no `turn.go` change.
  `ComposeSkill`'s merge call now returns `{procedure, checkpoints}` and seeds
  the ledger (degrades to the top procedure's `body` steps). `build_conversation`
  renders the progress block after the composed-skill block. `RecordSkillOutcome`
  prepends `render_final` to the synthesis transcript. (c) *Reconciliation
  trigger* — `turn.go`'s follow-up branch dispatches a detached (`ABANDON`)
  `RoutingWorkflow` with `Mode="reconcile"`: skips `Route()` + `ToolDiscover`,
  `MemoryRetrieve`/`SkillDiscover` get `Reconcile:true` → re-key on
  `"{retrieval_query} / {latest user msg}"` (read in-activity via
  `retrieval/reconcile.py`, no content crosses the workflow), `replace_rows`
  swaps the stale bundle, `ComposeSkill` (`Reconcile:true`) regenerates the
  composed block but not `turn_plan`. `staging.replace_rows` added. Failure-run
  half of the trigger deferred (no clean query).
- 2026-08-31: **Step 8 (planning) designed — `08-planning.md`.** Resolved via a
  brainstorm. Rejected a standalone `Plan` activity (latency + tokens on every
  complex turn; re-formats compose's work) and recursive decompose-to-leaves
  (decomposes on assumptions, plan inertia). Landed on a **living checkpoint
  ledger**: `ComposeSkill` emits an ordered `{intent, done_when, status}` list
  (no skill → no ledger → loop as today), the loop carries a progress block and
  the model reports advancement via a `plan_progress` tool call, corrections /
  failure-runs revise it, and the final state is structured input to skill
  synthesis (this is the artifact `skill-subsystem.md`'s reward model means by
  "whatever the planner produced"). Sequential only — DAG/parallelism deferred.
  New table `turn_plan` (migration `017`, mutable, unlike write-once
  `turn_retrieval`). **Related:** subagents become full agents — steps 2 + 3 run
  for `ParentType == "turn"` too (a subagent classifies its own task, fast-paths
  if trivial, gets its own skill/tool discovery, inherits the parent's memory
  snapshot); `RecordSkillOutcome` extends to moderate/complex subagent turns.
  **Also:** a mid-turn correction or failure-run re-invokes `RoutingWorkflow` in
  a light "reconcile" mode (skill + memory only) and merges the results into
  `turn_retrieval` — the only routing work that happens inside the loop.
- 2026-08-30: **Introduced; step 2 implemented; steps 3–7 designed.** Split into
  per-phase docs under `request-pipeline/`. The 11-step framing comes from a
  design conversation working through how to give the harness procedural
  knowledge without a static skill library — skills as a task-scoped composition
  over authored procedure skeletons + mined memory, assembled per turn. The
  routing + retrieval phase is modelled Temporal-native: a `RoutingWorkflow`
  child of `TurnWorkflow` owns the routing decision, a parallel fan-out of
  subsystem activities, a phase deadline, and partial-result assembly.
- 2026-08-31: Step 2 grew `retrieval_query` + `entities` (the retrieval
  subsystems need a real query, not the raw message) and a light recent-context
  read for follow-up resolution. Kept as activity I/O — no Postgres — after
  settling that small derived routing signals cross the workflow freely (like
  `ToolCallRef.{Server,Tool}`) while only bulk retrieved *content* goes through
  `turn_retrieval`. `TaskClassification` renamed `TaskRepresentation`.
- 2026-08-31: **Skill subsystem phase 5 (recency + adaptive radius) — subsystem
  complete.** `select` now blends `w_rec=0.1 · exp(−days_since_used/30)`, with
  `days_since_used` fed from `Procedure.last_used_at` by `SkillDiscover`.
  `SkillSynthesize` derives each procedure's `cluster_radius` from the mean
  pairwise cosine of its own member trajectories (`_radius`: −0.05 slack, floor
  0.60, `None`/`ASSIGN_RADIUS` under 2 embeddings) and assigns candidates
  against that radius. Migration `014` already carried both columns. The
  `skill_clusters` **cluster hierarchy + pgvector are deferred** — built only
  when the flat cosine scan over the store is *profiled* slow; `cluster_radius`
  + `divergence` re-versioning cover drift incrementally until then.
- 2026-08-31: **Skill subsystem phase 4 (co-occurrence).** `skill_cooccurrence`
  table (migration `016`), `store.update_cooccurrence` from `RecordSkillOutcome`
  (this turn's procedures × each other + × earlier same-session procedures, EMA
  edge weight γ=0.15, prune below 0.02 / after 90d), `store.edge_weights` read by
  `SkillDiscover`, `select` blends the `w_co=0.5` term. Cross-session/project
  linking still needs project scope.
- 2026-08-31: **Skill subsystem phase 3 (synthesis).** `SkillSynthesisWorkflow`
  + `SkillSynthesize` (`skills/synthesize.py` + `generalize.py`) — write-triggered
  from `RecordSkillOutcomeWorkflow`, debounced by the fixed `"skill-synthesis"`
  workflow id. Assigns candidates to procedure clusters (fixed radius), then
  creates new `learned` procedures / refines existing ones (new version) /
  appends failure notes, via a medium-tier structured generalization call.
  Divergence folds into refinement; no schedule backstop, no `/skill` signal yet.
- 2026-08-31: **Skill subsystem phase 2 (recording) + step-2 bootstrap tier.**
  `RecordSkillOutcomeWorkflow` / `RecordSkillOutcome` (`skills/record.py`,
  migration `015_skill_candidates`) — detached at turn end for moderate/complex
  task turns, writes the trajectory + EMA-updates composed procedures'
  confidence/trigger-embedding (`skills/vectors.py`, `store.ema_update`).
  Separately: `model_registry.tier_for_complexity` + `ModelCallInput.complexity`
  now bootstrap the first `ModelCall`'s tier from step 2's estimate instead of
  hardcoded `medium`.
- 2026-08-31: **Skill subsystem phase 1 built.** `skill_procedures` table
  (migration `014`, embeddings as `real[]` — no pgvector; flat Python cosine),
  `activities/activities/skills/` (`store`, `embedding` reusing `EMBEDDING_*`,
  `select` pure scoring, `seed` startup loader), 4 authored seed procedures,
  real `SkillDiscover` + `ComposeSkill`, `build_conversation` splices the
  `kind='composed'` block into the prompt. Recording / synthesis / co-occurrence
  / cluster hierarchy are later phases.
- 2026-08-31: **Steps 5 & 6 redesigned** — `components/skill-subsystem.md`
  ("The Skill Graph"). Moved from "authored procedure skeleton hosted in
  agent-brain, composed agent-brain-side" to a **harness-owned procedural
  memory**: a flat `skill_procedures` store (source of truth), a co-occurrence
  graph and a nightly-rebuilt cluster hierarchy derived from it, offline
  synthesis that generalizes recorded turn transcripts into versioned
  procedures, a Beta–Bernoulli confidence model, and a retrieval scoring blend
  (`sim + co-occurrence + confidence + recency − diversity`) that surfaces a
  coherent bundle rather than N independent matches. agent-brain stays purely
  the adaptation layer. `05`/`06` are now pipeline-contract pointers. Nothing
  built — steps 5/6 remain stubs.
- 2026-08-31: **Step 7 built.** `retrieval/tools.py` — `ToolDiscover` calls a new
  ctx-free `tools.discover_tools` (extracted from `search_tools`, which is now a
  thin wrapper), sharpens the query with unmatched entities, dedups `(server,
  tool)`, stages `kind='tool'` rows with `input_schema` in `metadata`. Same
  raise-on-error posture as `MemoryRetrieve`.
- 2026-08-31: **Step 4 built.** `retrieval/memory.py` — `MemoryRetrieve` calls
  `agent_brain.memory_search`, extracts one line per fused source, dedups +
  relevance-floors + token-budgets (`_select`), stages `kind='memory'` rows.
  `llm.py` gutted of the old `turn_seq==1` path (`_session_start_memory_block`,
  `_render_memory_results`, the retry loop, the `agent_brain`/`model_registry`
  imports, the module logger — all deleted); `build_conversation` now reads the
  staged rows every ModelCall, so retrieved memory persists for the whole turn
  instead of only the first call. No backward compat. `memory-slot.md`'s
  "session-start trigger" marked superseded → per-turn step 4.
- 2026-08-31: **Step 3 built, Temporal-native, with stub subsystems.**
  `routing.go` — `Route()` (pure, unit-tested), `RoutingWorkflow` (child of
  `TurnWorkflow`, parallel fan-out via a `Selector` loop against a phase-deadline
  timer, per-subsystem `ok|empty|error|timed_out|skipped`, `ComposeSkill` gated
  on real skill candidates), `startRouting` (spawns + races the child against a
  follow-up message → cancels routing, proceeds un-enriched — "option b").
  `turn_retrieval` staging table (migration `013`), `retrieval/` Python package
  with four registered stub activities + a `staging.py` helper. `RoutingResult`
  logged and held in `turn.go`; consumer wiring lands with steps 8/9. Verified:
  `Route()` unit tests + `RoutingWorkflow` tests via `testsuite` (fast-path,
  full fan-out, subsystem error, compose gating); `go build`/`vet`/`test`,
  `py_compile` clean. Not live-verified against a real stack.
