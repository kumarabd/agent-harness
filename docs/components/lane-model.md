# Component: Lane Model

> STATUS: BUILT. `routing.go` `laneIsDeliberate(taskRep)` is the single source
> of truth for the lane split — `Route()` delegates to it, and `dispatch.go`
> uses it to decide `PlanWorkflow` vs plain `TurnWorkflow`. `record.py` gate is
> `intent ∈ {task, question}` + `complexity ∈ {moderate, complex}`.
>
> Names the two request lanes: **Lite** (memory-only retrieval, or nothing for
> conversational — a plain reason-act turn, no plan, no recording) and
> **Deliberate** (the full pipeline + a `PlanWorkflow` + RL recording; see
> [`request-pipeline/08-planning.md`](request-pipeline/08-planning.md)).
>
> Parent: [`request-pipeline.md`](request-pipeline.md),
> [`request-pipeline/03-routing.md`](request-pipeline/03-routing.md).

### The asymmetry this fixes

Without the split, a `task` of any complexity would get the full
`memory + skills + tools` retrieval fan-out and a plan, whether it needs one or
not. "Commit my changes" would pay almost the same pipeline cost as "migrate the
database", and every routine turn would try to teach the skill store a procedure
the model already knows.

### The principle

The skill store exists so a task **starts from a procedure instead of from
nothing** (`skill-subsystem.md`). That only helps when the model *wouldn't
otherwise know the shape* — which is exactly what `moderate` / `complex` means.
A `simple` task is *defined* as one whose steps are obvious; the model already
knows `git commit`, `restart nginx`, `run the build`.

The part of a simple task that **is** worth retrieving — "this project's commit
message carries a `Co-Authored-By` trailer", "tests run via `mise run test`" —
is **preferences and facts, not procedures**. That is the memory slot's job
(pipeline step 4), not the skill store's.

| store | holds | needed by |
|---|---|---|
| **memory** (agent-brain) | the user / project *parameters* | any task that has non-obvious params |
| **skill store** | the *shape* of non-obvious work | `moderate` / `complex` only |

### Two lanes

| lane | retrieval | execution | task-run | learning |
|---|---|---|---|---|
| **Lite** | **memory** — or nothing, for `conversational` | plain `TurnWorkflow`, one reason-act loop | no | none |
| **Deliberate** | memory + tools + **skills** (on the planning turn) | `PlanWorkflow` — planning turn → PLAN.md → approval gate → checkpoint turns | yes (spans turns; `ResolveOpenPlan` continues / supersedes) | `RecordSkill` — EMA + insert |

**Deliberate owns everything procedural**: skill discovery, the PLAN.md ledger,
the `PlanWorkflow` lifecycle ([`episode-lifecycle.md`](episode-lifecycle.md)),
and RL recording. Lite is just routing (memory at most) → reason-act.

`conversational` is not a third lane — it is Lite where `Route()` retrieves
nothing (the `FastPath`). Memory inside Lite is a conditionally-active
subsystem.

### Lane assignment — `(intent, complexity)`

`intent` picks the family; `complexity` is an escalation dial that only matters
for `question` and `task`.

| intent \ complexity | `trivial` | `simple` | `moderate` | `complex` |
|---|---|---|---|---|
| `conversational` | Lite | Lite | Lite | Lite |
| `meta` | Lite | Lite | Lite | Lite |
| `question` | Lite | Lite | Lite | **Deliberate** |
| `task` | Lite | Lite | **Deliberate** | **Deliberate** |

Deliberate is exactly three cells — `(task, moderate)`, `(task, complex)`,
`(question, complex)` — plus one override:

- **`confidence < 0.5`** (the classifier's own self-reported uncertainty —
  step 2 has no "wasn't classified" state; a failed classify fails the turn)
  → **Deliberate** regardless of cell, so a shakily-classified real task is
  never under-provisioned. Same conservative posture `Route()` already takes.

Within Lite, `Route()` still chooses how much memory: **nothing** for
`conversational` (the current `FastPath`), **memory** for everything else.

- **`question` + `complex`** is Deliberate: "walk me through how we'd redesign
  the auth system" is a design/brainstorm task wearing a question's clothes —
  it wants the brainstorming procedure, spans turns, and is worth learning from.
  A `moderate` question ("what are the trade-offs between optimistic and
  pessimistic locking?") is an explanation, not a procedure — Lite.
- **`meta`** stays Lite at every complexity — a question about the agent itself
  ("what's in your memory about project X", "what can you do?") is recall, never
  a procedure.

A **Lite turn that continues a running Deliberate plan** still attaches — a
follow-up to an in-progress task *is* that task (`ResolveOpenPlan` decides on
`continues_prior` + embedding similarity). A **new Lite task while a plan runs**
supersedes it (signals `abandon`) and then runs as a plain turn.

### Per-lane detail

#### Lite — everything except the three Deliberate cells

- **Route:** `{Memory: true}` for `meta` / `question` / `task`;
  `{FastPath: true}` for `conversational`. Memory (and, for a Deliberate turn,
  tools) run per turn; **skills never** on a Lite turn — `Route` returns
  `Skills: true` only for Deliberate, and `RoutingWorkflow` clears it anyway
  unless the turn is a planning turn (`PlanID` set).
- **Execution:** `dispatch.go` starts a plain `TurnWorkflow` — one reason-act
  loop, `fast` tier (`trivial`/`simple`) or `medium` tier. The model keeps full
  tool access and can call `search_tools` / `memory_search` mid-turn.
- **No task-run, no `RecordSkill`.** A simple task has nothing skill-shaped to
  contribute; the user-specific bits already live in memory.

#### Deliberate — `(task, moderate|complex)`, `(question, complex)`, `confidence < 0.5`

`dispatch.go` starts a `PlanWorkflow` (unless `ResolveOpenPlan` says this
message continues a running one — then it attaches, or supersedes it). Full
detail in [`08-planning.md`](request-pipeline/08-planning.md): planning turn →
PLAN.md → approval gate → checkpoint turns → one async `RecordSkill`.

### Degradation

- Misroute Lite → should have been Deliberate: the turn runs memory-only and the
  model can still `search_tools` / spawn a subagent itself; a less-prepared first
  step, no correctness loss.
- Misroute Deliberate → should have been Lite: a wasted planning turn + skill
  fan-out. `RecordSkill`'s intent/complexity gate keeps it from minting a junk
  procedure.
- `confidence < 0.5` always takes Deliberate — the expensive-but-safe direction.

### Deferred

- **Trivial single-shot execution** — skip the reason-act loop for
  `conversational`/`trivial`: one `ModelCall`, one optional tool round, hard cap
  ~3 iterations. Pure latency/cost optimization, no behavior change.
- **Per-lane iteration caps** — `maxIterations` is a turn-wide 20 today. Scale
  it: `trivial` 3, `simple` 6, `Lite moderate` 10, Deliberate 20.
  Numeric-tuning discipline — do it once there's usage data.
- **Lite → Deliberate escalation mid-turn** — if a Lite turn's model spawns a
  subagent or hits repeated tool failures, it's probably a misroute. Could
  promote it to a `PlanWorkflow`. Deferred until misroutes are observed being
  costly.

### Open Questions

- **`question/moderate` losing skills** — a moderate question that's really
  "how do I do X" (procedural) would benefit from a skill. The bet is that
  those classify as `task` or `complex`; watch whether real moderate questions
  are left under-served.
- **Does Lite need `retrieval_query` sharpening?** Step 2 produces one for every
  turn; Lite only uses it for the memory query. Probably fine as-is.
- **`meta` + "what can you do?"** — this lane can't answer it well today (see
  the capabilities gap, a separate thread). Not this doc's job to fix, but it's
  the clearest example of a Lite turn the current pipeline serves poorly.
