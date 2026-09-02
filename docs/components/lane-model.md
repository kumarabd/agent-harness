# Component: Lane Model

> STATUS: BUILT + DEPLOYED + VERIFIED (2026-09-01). `routing.go`
> (`laneIsDeliberate` + reworked `Route()`), `turn.go` (`OpenEpisode` is now the
> single lane decision point — always called for a non-conversational turn,
> returns `episode_id == ""` for a Lite turn), `episode.py` /
> `OpenEpisodeInput.want_new_episode`, `record.py` gate widened to
> `{task, question}`. `routing_test.go` covers the new table + `laneIsDeliberate`.
> Scenario messages retuned (`plan-progress-lifecycle`, `episode-upgrade-initial`
> to clearly-Deliberate; `episode-supersede-followup` classifier-agnostic).
> Compile + `go test` clean.
>
> **Live status (2026-09-01, redeployed):** Deliberate lane fully verified —
> `skill-plan-integration`, `plan-progress-lifecycle`, `subagent-full-agent`,
> `episode-plan-complete`, `episode-multiturn`, `episode-supersede` all pass.
> **Fully verified live 2026-09-01** (after the fast tier was moved off the
> thinking model `nvidia/Nemotron-3.5-Lightning` — whose default reasoning
> starved `classify.py`'s 400-token cap and forced every turn to Deliberate —
> to plain instruct models: fast `google/gemma-4-31b-it`, medium
> `Qwen/Qwen3-235B-A22B-Instruct-2507`, expert `deepseek-ai/DeepSeek-V4-Pro`,
> `deploy/helm/tenants/abishekk.yaml`, `helm upgrade` only):
> - **Lite**: `question/trivial` → no episode, `Route()` = memory-only
>   (`tools skipped skills skipped`), no plan, no candidate, fast tier.
> - **Deliberate**: `skill-plan-integration`, `plan-progress-lifecycle`,
>   `subagent-full-agent`, `episode-plan-complete` all pass.
> - **`episode-multiturn`**: `task/complex` then `task/moderate` follow-up →
>   attach → one episode, one candidate spanning both turns.
> - **`episode-supersede`**: `task/complex` then unrelated `task/simple` →
>   the simple task closes the abandoned episode (recorded `outcome=failure`)
>   and takes the Lite lane itself (`is Lite, no episode` in the log).
> - **`episode-upgrade`**: `question/complex` then `task/moderate` follow-up →
>   attach + `classification upgraded to intent=task` → episode records.
>
> Names the two request lanes and gives the lightweight one (**Lite**) the same
> first-class definition the heavyweight one (**Deliberate**) already has via
> `episode-lifecycle.md` / `request-pipeline/08-planning.md` /
> `skill-subsystem.md`. Today everything lighter than "complex task" is just the
> Deliberate path with subsystems skipped, decided ad hoc inside `Route()`.
>
> Parent: [`request-pipeline.md`](request-pipeline.md),
> [`request-pipeline/03-routing.md`](request-pipeline/03-routing.md).
> Reshapes: `Route()`'s rule table; the `turn.go` `OpenEpisode` gate
> (`episode-lifecycle.md`); the `RecordSkillOutcome` gate (`skill-subsystem.md`).

### The asymmetry this fixes

The heavyweight path is designed end to end: classify → route → skill discovery
→ compose → plan ledger → reason-act with `plan_progress` → subagents for
detail-heavy slabs → reconciliation on correction → record → synthesize → learn.

The lightweight path is "the heavyweight path minus recording." A `task` of any
complexity gets the full `memory + skills + tools` retrieval fan-out;
`ComposeSkill` seeds a `turn_plan` checkpoint ledger for a two-step task that
will never touch it; every non-conversational turn opens an episode. "Commit my
changes" pays almost the same pipeline cost as "migrate the database."

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

| lane | retrieval | execution | episode | learning |
|---|---|---|---|---|
| **Lite** | **memory** — or nothing, when there's nothing to retrieve (`conversational`) | routing → reason-act; low iteration cap; `trivial` → single-shot | no | none |
| **Deliberate** | memory + skills + tools + **plan ledger** | reason-act + `plan_progress` + subagents + reconciliation | yes (multi-turn: attach / supersede) | full — EMA **and** `_create` |

**Deliberate owns everything procedural**: skill discovery, `ComposeSkill`, the
`turn_plan` ledger, episodes (`episode-lifecycle.md`), and RL recording. Lite is
just routing (memory at most) → reason-act.

`conversational` is not a third lane — it is Lite where `Route()` retrieves
nothing (the existing `FastPath`). Memory inside Lite is a conditionally-active
subsystem, exactly like tool discovery inside Deliberate.

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

### Per-lane detail

#### Lite — everything except the three Deliberate cells

- **Route:** `{Memory: true}` for `meta` / `question` / `task`;
  `{FastPath: true}` for `conversational`. Never `SkillDiscover`, `ToolDiscover`,
  or `ComposeSkill` — so no `turn_plan` seed either (`RoutingWorkflow` only
  calls `ComposeSkill` when `SkillDiscover` returned rows; with Skills off it
  never runs). All of that falls out of the Route change — no separate gating.
- **Staging:** `MemoryRetrieve` stages `kind='memory'` rows under the turn's own
  `turn_id` (there is no episode). `prompt.assemble` already reads enrichment by
  `episode_id or turn_id`, so a null episode reads the turn's own rows —
  `episode-lifecycle.md`'s fallback, now load-bearing for this lane.
- **Execution:** the normal reason-act loop, `fast` tier (`trivial`/`simple`) or
  `medium` tier (a Lite `moderate` question), with a lower iteration cap than
  the heavyweight 20 (see Deferred). A `conversational`/`trivial` turn ideally
  skips the loop entirely — one `ModelCall` (see Deferred). The model keeps full
  tool access — it can call `search_tools` / `memory_search` itself mid-turn if
  it turns out to need more.
- **No episode, no `RecordSkillOutcome`.** A simple task has nothing
  skill-shaped to contribute; repeated routine work doesn't teach the skill
  store a procedure (the model already knows it), and the user-specific bits
  already live in memory.

#### Deliberate — the three cells (+ `confidence < 0.5`)

Unchanged — this is the path `episode-lifecycle.md` / `08-planning.md` /
`skill-subsystem.md` already describe. `OpenEpisode`, full retrieval, plan
ledger, `plan_progress`, subagents, reconciliation, `RecordSkillOutcome` on
episode close, synthesis.

### What changes

**`routing.go` — `Route()`'s table:**

| | before | after |
|---|---|---|
| `task` `trivial`/`simple` | `{Memory, Skills, Tools}` | `{Memory}` |
| `task` `moderate`/`complex` | `{Memory, Skills, Tools}` | `{Memory, Skills, Tools}` |
| `question` `moderate` | `{Memory, Skills}` | `{Memory}` |
| `question` `complex` | `{Memory, Skills}` | `{Memory, Skills, Tools}` |
| `question` `trivial`/`simple`, `meta`, `conversational`, `conf < 0.5` | (unchanged) | (unchanged) |

**`routing.go` — `laneIsDeliberate(taskRep)`:** the pure lane predicate, the
single source of truth. `Route()` delegates to it; `turn.go` passes its result
to `OpenEpisode`.

**`turn.go` — `OpenEpisode` is the one lane decision point.** It is called for
*every* non-conversational turn (a cheap indexed lookup when nothing is open),
given `WantNewEpisode = laneIsDeliberate(taskRep)`. It returns the episode to
work under:
- **attach** — any turn continuing an *open* episode, even a "yes, use Redis"
  one-liner that classifies Lite on its own (a follow-up to an in-progress task
  *is* that Deliberate task);
- **supersede + open fresh** — a new Deliberate task while one is open;
- **supersede only** — a new *Lite* task while one is open (closes the abandoned
  episode, opens none);
- **open fresh** — a Deliberate task, nothing open;
- **`episode_id == ""`** — a Lite turn, nothing to attach to.

`episodeID != "" ⟺ Deliberate`. Lite turns route memory-only under their own
`turn_id`; `conversational` skips `OpenEpisode` and `Route()` fast-paths.

**`skills/record.py` — the gate:** widen `intent == "task"` to
`intent ∈ {"task", "question"}` (still `AND complexity ∈ {moderate, complex}`),
so a `question/complex` episode records. Since only Deliberate opens an episode,
this is really just a defensive restatement of the lane rule.

`ComposeSkill` / `plan.seed` / `ToolDiscover` all gate on `Route()`'s plan
already; turning Skills/Tools off for a lane turns them off transitively — no
extra gating. `OpenEpisode`'s subagent branch also honours `WantNewEpisode`: a
Lite subagent takes the Lite lane.

### Degradation

- Misroute Lite → should have been Deliberate: the turn runs memory-only and
  the model can still `search_tools` / spawn a subagent itself; no correctness
  loss, just a less-prepared first step. Same self-healing argument `03-routing.md`
  already makes for the fast path.
- Misroute Deliberate → should have been Lite: a wasted skill/tool fan-out and
  an episode row that records a trivial candidate. `_create` stays gated so it
  won't mint a junk procedure from one lone simple success.
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
  promote (open an episode, run retrieval) the way `08-planning.md`'s
  reconciliation trigger re-retrieves. Deferred until misroutes are observed
  being costly.

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
