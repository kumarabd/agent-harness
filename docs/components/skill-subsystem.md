# Component: Skill Subsystem — The Skill Graph

> STATUS: BUILT on branch `proactivity-substrate` (not deployed). This doc is
> the full design; the sections below carry the reward-model reasoning. Current
> mechanism:
>
> - **`skill_procedures`** (migration `014`, embeddings as `real[]`, no
>   pgvector) — the flat store. `skills/` = store / embedding / vectors / select
>   / seed / generalize. 4 authored seed procedures.
> - **Retrieval — `SkillDiscover`** (pipeline step 5): on a **planning turn**,
>   embed `retrieval_query`, flat cosine over the session's procedures, scored
>   selection (`sim + w_co + w_conf + w_rec`), stage the chosen procedures'
>   full renders to `turn_retrieval` `kind='skill'` under the plan_id.
>   `prompt.assemble` renders them into the planning turn's prompt.
> - **There is no composition step.** The planning turn drafts the checkpoint
>   plan from the retrieved procedures directly ([`request-pipeline/08-planning.md`](request-pipeline/08-planning.md)).
> - **Write — `RecordSkill`** (`skills/record.py`, `RecordSkillWorkflow`,
>   detached ABANDON, 5-min timeout): fires **once** when a task-run closes,
>   over its whole prefix-swept trajectory. Collapses the old
>   `RecordSkillOutcome` + `skill_candidates` (table dropped, migration `022`) +
>   `SkillSynthesizeWorkflow` chain. Embeds the task, EMA-updates the procedures
>   `SkillDiscover` retrieved into the run + the co-occurrence graph, then
>   match-or-inserts against `skill_procedures`: match+success+divergence →
>   `new_version`; match+success → positive EMA; no-match+success →
>   `generalize` + `insert_learned`; match+failure → caution note;
>   no-match+failure → dropped. `_MATCH_RADIUS = 0.82` (per-procedure
>   `cluster_radius` stays `None` — a single trajectory's radius can't be
>   measured).
> - **Co-occurrence graph** (`skill_cooccurrence`, migration `016`) — updated by
>   `RecordSkill`; `select` blends `w_co`. Cross-session / project-scoped
>   linking still needs project scope wired.
> - **Deferred:** the `skill_clusters` cluster hierarchy + pgvector — a flat
>   Python cosine is the retrieval path until that scan is *measurably* slow.
> - **`trigger_text` altitude:** `generalize.py`'s system prompt now forces
>   task-CLASS altitude with explicit bad/good examples ("root-causing a
>   regression a recent deploy introduced", not "debugging the checkout NPE").
>   Still unverified against a real eval run, and the match test still embeds
>   the raw `task_text` (topic-specific) against the process-shaped trigger — an
>   asymmetry a distilled task-shape embedding would close (deferred).

### Role (one line)

A harness-owned **procedural memory**: procedures the harness has executed —
seen succeed, fail, or be corrected — generalized offline and stored so the
next task of the same shape starts from one instead of from nothing.
Distinct from the context slot (within-session) and the memory slot (the
*user's* world); this holds the *agent's own how-to*.

### Why harness-owned, not agent-brain

- **No cross-repo dependency.** agent-brain stays purely the adaptation layer
  (pipeline step 4, built). Nothing about it changes for skills.
- **The record loop is already here.** The harness natively sees task → plan →
  tool calls → outcome — the input to a skill. Emitting that to another service
  and reading generalizations back is strictly more moving parts.
- **The hot path stays local.** Retrieval at turn start is a Postgres + pgvector
  query in the tenant-worker, not an MCP round-trip.

### Explicitly NOT this component

- User adaptation — corrections/preferences live in memory-slot; the skill graph
  *consumes* them at compose time, doesn't store them.
- A hand-authored catalog as the primary source. Authored procedures are
  welcome (they enter the same store with high initial confidence) but the
  design assumes most procedures are learned.
- Execution — a composed skill is guidance text spliced into the model's
  context; the reason-act loop still does the work.

---

### The one idea

Procedures are stored **flat** — the source of truth. Two structures are
**derived** from them and rebuilt offline:
- a **cluster hierarchy** — a disposable retrieval accelerator + a browsable map;
- a **co-occurrence graph** — which procedures were actually used together, and
  worked.

Retrieval narrows through the tree, then ranks within by a blend that pulls
co-occurring procedures in together — so a turn gets a *bundle* for the task,
not a list of independent nearest matches. "Dynamic / self-organizing" is
achieved by rebuilding the derived structures, never by node-by-node surgery on
a live hierarchy.

---

### The reward model — what gets cached, and when

A procedure is not "what the planner produced." It is **the effective procedure
reconstructed from a *successful* task's full trajectory** — the planner's
initial plan *plus every correction made during execution*, flattened into one
clean form. The intermediate broken states are never cached; their content
survives (a mid-task "use the test container, not a mock" that the task then
succeeded with becomes part of the body). Corrections are baked in, not
discarded.

This is **episodic self-imitation with a terminal 0/1 reward**, closer to
**prototype-based / episodic control** than to gradient RL. A procedure is a
**prototype** — the centroid of a cluster of successful trajectories for one
task-shape, with a step body reconstructed from them — and it carries an
**online value estimate** (“Confidence”). Both the prototype and the value estimate are
**EMA-updated** by each new successful run (see “Synthesis: EMA updates” and “Confidence”): recent evidence weighs
more, and a procedure that starts failing self-corrects within a few runs
regardless of a long clean history.

Consequences that shape the rest of the spec:

- **A procedure is a cluster, not a threshold.** There is no magic
  "same-procedure" cosine cutoff — clustering over the trajectories decides what
  groups together, and the nightly rebuild shifts the boundaries as the store
  grows (see “Synthesis: Assignment”).
- **Cache on the first success.** The first successful trajectory that falls
  outside every existing procedure's cluster *is* a new procedure, at the low
  prior confidence. Later successes in the same cluster *refine* it (a cleaner
  body, an EMA'd prototype), they don't create.
- **Only success produces a body. Failure produces annotations.** A fully-failed
  task never becomes a "here's how" procedure, but its transcript is kept and
  feeds synthesis as negative signal — "we tried X, it doesn't work" — attached
  to the relevant procedure as a `note`.
- **A later, separate correction is not a special case.** It is just another
  successful trajectory that started from an existing skill and diverged from
  it — an EMA update plus a body re-synthesis (“Synthesis”). Every successful task either
  creates a procedure or updates one.
- **Only `moderate` / `complex` tasks are recorded** (step 2's `complexity`).
  Trivial and simple tasks don't produce candidates — the store stays about
  procedures worth having.

**Two regimes** a given procedure can be in, distinguished by its `provenance`
and how many `divergence` re-versions it has taken:

| regime | what the cache holds | example |
|---|---|---|
| **Memoization** | what the LLM would produce anyway — the skill just saves the planning call | "organize a Go service", "write a conventional commit" |
| **Accumulated learning** | knowledge the LLM *cannot regenerate* from context+memory+tools — it only became correct through feedback | an undocumented env var the deploy needs; a team's arbitrary convention; an ordering constraint the harness only learned by getting burned |

For regime 2, at t=0 the wrong-but-plausible plan and the right one are
indistinguishable to the planner; the value is entirely in the corrections the
trajectory accumulated. This is the case the subsystem exists for.

---

### The lifecycle

Six moves in a loop. Two run online in the turn; the rest run offline on
Temporal schedules.

```
  Turn executes ──► Record candidate ──►  [ OFFLINE ]  Synthesize ──► Skill store
   (reason-act)      (§ Recording,          (§ Synthesis,             (flat, SoT)
        │             on success)            every ~2h)                    │
        │                                                                 │
        │ outcome: which pairs worked (accent)                            │ nightly
        ▼                                                                 ▼
  Co-occurrence graph  ◄─────────────────────────────────────  Cluster hierarchy
        │                                                                 │
        └────────────────┐                          ┌────────────────────┘
                         ▼                          ▼
                     Retrieve (step 5) ──► Compose (step 6) ──► into the next similar turn
```

---

### Data model

Four tables in the tenant's own `agent_harness` database (the skill graph
references `turns` and is driven by `turn.go` — it belongs with the
conversational state, not in a separate DB like mcp-hub's index). Adds the
`pgvector` extension to `agent_harness` (today only `mcp_hub` has it). Reuses
the existing `EMBEDDING_*` config.

#### `skill_procedures` — the flat store (source of truth)

One row per procedure *version*. Retrieval only sees `valid_to IS NULL`.

| field | notes |
|---|---|
| `id` | stable across versions of the same procedure |
| `version` | int, bumped on every re-synthesis |
| `title` | short human label |
| `trigger_text` | NL "when this applies" — what retrieval matches |
| `trigger_embedding` | `vector`, from `trigger_text` — retrieval key + cluster centroid; **EMA-updated** (see “Synthesis: EMA updates”) toward the tasks that actually match it |
| `cluster_radius` | float — how far a candidate task can be and still be "this procedure"; derived from the spread of its own member trajectories, not a global constant ("Assignment", below) |
| `body` | ordered steps: `{step_id, instruction, tool_ref (abstract), slots[]}` |
| `preconditions` | checked at compose time, unmet ones surfaced |
| `done_criteria` | how the model knows it's done |
| `provenance` | `learned` \| `authored` \| `corrected` + source candidate ids |
| `scope` | `global` \| `tenant` \| `project:<x>` \| `user:<x>` |
| `confidence` | float — the **EMA value estimate** (“Confidence”). Not derived from counts. |
| `run_count` | int — evidence volume only (a high-confidence procedure off 2 runs is shown more cautiously than off 40) |
| `member_candidate_ids` | the successful trajectories this version was synthesized from |
| `last_used_at` | recency signal |
| `valid_from` / `valid_to` | bi-temporal validity; `valid_to` set when superseded |
| `superseded_by` | id+version that replaced this row; null for current |

#### `skill_candidates` — raw material (written online, consumed by synthesis, not retrievable)

| field | notes |
|---|---|
| `id` / `turn_id` | the turn recorded from |
| `task_text` / `task_embedding` | step 2's `retrieval_query` + raw message; embedding clusters candidates pre-synthesis |
| `transcript` | the **full trajectory** — planner output, ordered tool calls + args + outcomes, model reasoning, *and any mid-task correction* the user made during the turn, plus the final result |
| `outcome` | `success` \| `failure` — a turn that got corrected mid-task and then succeeded is `success`; the correction lives in `transcript` |
| `required_correction` | bool — the task succeeded but only after the user pushed back mid-turn. A weaker success; feeds confidence and flags the trajectory for synthesis attention. |
| `composed_from` | skill ids retrieved into this turn — a "did the existing skill help" / "did the trajectory diverge from it" signal |
| `synthesized_at` | null until consumed |

#### `skill_cooccurrence` — the learned graph

| field | notes |
|---|---|
| `proc_a` / `proc_b` | unordered pair (`proc_a < proc_b`) |
| `edge` | float 0–1 — **EMA of joint success** (“Co-occurrence”); this is the retrieval `w_co` weight directly |
| `last_seen_at` | drop the edge after `T_forget` |

#### `skill_clusters` — the derived hierarchy (dropped + rebuilt nightly, no incremental mutation)

| field | notes |
|---|---|
| `cluster_id` / `parent_id` | tree edges; root has null parent |
| `level` | 0 = root, increasing toward leaves |
| `centroid` | `vector`, normalized mean of member trigger embeddings |
| `label` | LLM summary of members, carried forward from the prior rebuild's matching cluster |
| `member_ids` | procedure ids under this node |

---

### Recording — the reward model

> The mechanism is `RecordSkill` (see the STATUS block) — one online activity,
> no `skill_candidates` table, no separate synthesis workflow. This section and
> the next ("Synthesis") describe the **reward-model reasoning** — what counts
> as success, what gets cached and when, EMA vs re-version, the divergence
> signal. That reasoning is unchanged; where these sections say
> `RecordSkillOutcome` / "candidate" / "the synthesis pass", read `RecordSkill`
> doing the same judgement inline over the task-run's prefix-swept trajectory.

`RecordSkillOutcomeWorkflow` → `RecordSkillOutcome` (`skills/record.py`),
dispatched detached (`ABANDON`, same shape as `WriteMemoryWorkflow`) by whoever
closes the episode (`turn.go` on plan-complete / subagent-turn-end;
`CloseSessionEpisodesWorkflow` on idle-exit; `turn.go` again when a new task
supersedes an open one). The activity reads the episode row
(`intent`/`complexity`/`close_reason`/`last_stop_reason`) and gates itself:
`intent == "task"` **and `complexity` `moderate`|`complex`** — same bar as the
old per-turn gate, now on the episode's anchor classification.

The activity reads every turn in the episode (`messages` / `tool_calls` joined
on `turns.episode_id`), the staged `skill` rows, and the final `turn_plan`
ledger from Postgres itself. It writes one `skill_candidates` row (transcript =
`plan.render_final` + the messages in order, each assistant message's tool calls
after it) and, for every procedure composed into the episode, runs the EMA
update. Idempotent per episode (delete-then-insert the unsynthesized row).

Terminal reward (phase-2 approximations — refined later):
- **success** — `close_reason == "plan_complete"`, or (`idle`/`turn_end`) with
  the last turn's `stop_reason == "no_tool_calls"` and no errored tool call.
- **failure** — otherwise, and always for `superseded` (the user pivoted away
  before the plan finished).
- **`required_correction`** — more than one `user` message across the episode
  (clarifying dialogue or a correction). Distinguishing the two is future work
  (episode-lifecycle.md, Deferred). `reward` is `1.0` on plain success, `0.5` on
  a corrected success, `0.0` on failure.

Not yet: co-occurrence edge updates (phase 4); post-turn follow-up detection
(a later-session "that's still wrong" isn't seen).

A **success** candidate can become a procedure body. A **failure** candidate
never does — but its transcript is kept and feeds synthesis as negative signal
("we tried X, it doesn't work") to be attached as a `note` on the relevant
procedure.

Recording is cheap and dumb — it captures the trajectory, nothing more. All
judgment (is this repeatable? does it generalize? what did the corrections
teach?) is deferred to synthesis.

---

### Synthesis — folded into `RecordSkill`

> `SkillSynthesizeWorkflow` / `synthesize.py` are **deleted**. The
> assignment / creation / refinement / versioning logic below runs **inline**
> inside `RecordSkill` (match-or-insert against `skill_procedures`, one
> `generalize` call on an insert or divergence re-version). No candidate queue,
> no debounce, no offline pass. Read the rest for the assignment / EMA /
> versioning reasoning; the trigger is "every task-run close", synchronous.

_(historical description, kept for the reasoning:)_

`SkillSynthesisWorkflow` → `SkillSynthesize` (`skills/synthesize.py` +
`generalize.py`). **Write-triggered**, not scheduled: `RecordSkillOutcomeWorkflow`
starts it after every recording, with the fixed workflow id `"skill-synthesis"`
so an "already started" rejection debounces concurrent turns (the pattern
agent-brain's mining trigger uses). `ALLOW_DUPLICATE` reuse so a fresh run
starts once the last finished. A periodic backstop schedule (agent-brain keeps
its 6h cron alongside the trigger) is **not** set up yet — every candidate is
picked up on the next recording, which is enough at current volume.

The activity processes the whole `synthesized_at IS NULL` queue for the tenant
(≤ 200 rows/run, embeddable only), then marks every fetched row done.

Simplifications vs. the design below:
- **`SUBCLUSTER_SIM` (0.80)** stays a fixed constant for grouping brand-new
  candidates into provisional procedures. Existing procedures use their own
  `cluster_radius` (phase 5), falling back to `ASSIGN_RADIUS` (0.82) until
  synthesis has ≥ 2 member embeddings to measure a spread from.
- **Divergence has no separate trigger** — a candidate whose `composed_from`
  names its assigned procedure just forces refinement to fire immediately
  (instead of waiting for `N_REFINE = 3`), re-synthesizing from the current
  body + that trajectory.
- **Explicit `/skill` signal** — not wired. **DEFERRED (2026-09-01):**
  user-dictated procedures ("when you deploy a microservice: 1. build the image
  2. bundle the chart 3. `helm upgrade --install` 4. verify health") currently
  have no ingestion path — they only become a skill after the agent executes
  them once and the RL loop reverse-engineers one from the trajectory. A direct
  path (classify detects a dictated procedure → parse steps → write
  `skill_procedures` at `provenance='dictated'`, high starting confidence,
  RL loop refines from there) was sketched and shelved as not worth the
  complexity yet. It also needs a mining carve-out so agent-brain doesn't
  smear the step sequence across memory as disconnected "preferences" — the
  intended boundary is: **memory owns the parameters that fill a skill's slots
  and generalize across many skills ("we use Helm", "images → gcr.io/X");
  the skill owns the procedure-specific ordering and structure.** `ComposeSkill`
  already recombines the two; the gap is purely at ingestion.
- **Failure annotation** appends a raw "a previous attempt failed: <task>" note
  when the procedure isn't being refined this run; when it *is*, the failure
  transcripts feed the generalization pass as the `notes` source instead.

#### Assignment — a procedure is a cluster, not a threshold

Every un-synthesized candidate is assigned to an existing procedure's cluster or
starts a new one. **No pairwise `t_dedup` cutoff** — the assignment is:

```
for each un-synthesized success candidate c:
    p* = argmax over current procedures of cos(c.task_embedding, p.trigger_embedding)
    if cos(...) >= p*.cluster_radius:      # inside p*'s cluster — see below
        assign c to p*
    else:
        c starts a new provisional procedure (one-member cluster)
```

`cluster_radius` is not a global constant — it is derived per procedure from the
spread of its own member trajectories — `_radius` in `synthesize.py`: mean
pairwise cosine among the group's candidate embeddings, minus a `0.05` slack,
floored at `0.60`, `None` (→ `ASSIGN_RADIUS`) with fewer than 2 embeddings. It
is recomputed on every `_create` / refine and carried forward when a refine
group is too small to remeasure. A tight, well-established procedure ("write a
conventional commit") has a small radius and rejects loosely-related tasks; a
young one-member procedure has a generous radius and absorbs neighbours until it
tightens.

The **nightly rebuild** (“Cluster hierarchy”, deferred) would re-cluster all
trajectories from scratch so a procedure that has drifted or should split does
so without per-row surgery. Until it exists, per-`_radius` tightening plus the
`divergence` re-version cover drift incrementally.

#### Triggers

1. **Creation** — a `success` candidate that started a new one-member cluster.
   Reconstruct a procedure from its single trajectory; it enters at the low
   prior confidence (“Confidence”).
2. **Refinement** — a procedure's cluster gained ≥ `N_refine` (start 3) new
   members since its last synthesis. Re-synthesize a cleaner body from the
   cluster's trajectories; EMA the prototype and value estimate (see “Synthesis: EMA updates”).
3. **Divergence** — a `success` candidate assigned to procedure `p` whose
   `composed_from` names `p` *and* whose trajectory departed from `p`'s body
   (user corrected a step mid-turn, or the model deviated and still succeeded).
   Re-synthesize immediately with the divergence folded in — the later, separate
   correction, handled as just another successful trajectory.
4. **Explicit signal** — user says "remember how to do this" / `/skill`. Same as
   creation, forced.
5. **Failure annotation** — `failure` candidates assigned to an existing
   procedure: extract "tried X here, it failed" into its `notes`, body
   untouched.

#### The generalization pass

One medium-tier model call per trigger group. Input: the current procedure body
(refinement / divergence), the cluster's trajectories, and any assigned
`failure` transcripts. Structured output:

```
{
  title, trigger_text,                       # trigger_text kept general
  body: [ { step_id, instruction,
            tool_ref,   # ABSTRACT — "a container-registry tool", not a concrete id
            slots: [str] }, ... ],
  preconditions: [str], done_criteria: [str],
  notes: [str]          # cautions from mid-task corrections + matched failures
}
```

Prompt instructs: reconstruct the *effective* procedure the successful
trajectories actually followed (initial plan **as amended by** every mid-task
correction), **weight recent trajectories more heavily than old ones**, keep
tool refs abstract, factor shared sub-sequences into their own steps, and turn
every correction and matched failure into a `note` or a changed step — never
leave the superseded version in the body.

#### EMA updates — prototype and value

Two things about a procedure are updated incrementally rather than recomputed
from scratch, so recent evidence dominates:

```
# trigger_embedding drifts toward the tasks that actually match it
#   (Rocchio / online-centroid update)
trigger_embedding  <-  normalize( (1 - β) · trigger_embedding  +  β · c.task_embedding )
    β ~ 0.15

# confidence is an EMA of the terminal reward (see “Confidence”)
confidence         <-  (1 - α) · confidence  +  α · reward
    α ~ 0.2 ,  reward = 1 (success) | 0 (failure) | 0.5 (success with required_correction)
```

The body itself can't be numerically averaged — its "weighted update" is the
generalization prompt above being told to prefer recent trajectories, plus the
divergence trigger re-synthesizing on demand.

#### Versioning

A synthesis that changes the body writes a new `version` row: `valid_to = now()`
on the old, `superseded_by` linking them. `trigger_embedding` and `confidence`
carry forward (they were being EMA'd all along, not reset). Old versions are
never deleted — a full audit trail and a rollback path if a synthesis makes a
procedure worse.

---

### Confidence

The procedure's **online value estimate** — how likely following it is to lead
to a successful task, weighted toward recent evidence. Used in ranking and as an
injection gate. An **exponential moving average** over the terminal reward:

```
confidence  <-  (1 - α) · confidence  +  α · reward           # α ~ 0.2
    reward = 1  (success)
           | 0.5 (success with required_correction)
           | 0  (failure)

confidence(new procedure) = 0.25                              # skeptical prior
```

Updated by the same end-of-turn activity that writes the candidate, once per
task turn the procedure was composed into.

- **Why EMA, not a success-count ratio.** A ratio remembers a procedure's whole
  history equally — 30 old successes drown out 3 recent failures, so a procedure
  whose environment just changed still reads as trustworthy. An EMA at `α = 0.2`
  drops a suddenly-failing procedure below the injection floor in ~4 runs. This
  is the answer to staleness (“Open Questions”) — no recency rule needed.
- `run_count` is still tracked, purely as an **evidence-volume** signal: a
  procedure with `confidence 0.8` off 2 runs is shown more cautiously than one
  off 40.
- **Authored** procedures seed at `confidence = 0.7`, `run_count = 0` — trusted
  from day one, still moving with real evidence.
- Below `~0.35`: retrievable but composed in with a visible "unverified — check
  each step" label.

---

### Retrieval (pipeline step 5 — `SkillDiscover`) — BUILT (phases 1–5)

Input: the turn's `retrieval_query`. Output: staged `turn_retrieval`
`kind='skill'` rows — a ranked, budget-bounded set that belongs together.

**Built:** flat cosine over every current in-scope procedure, then the greedy
scoring/selection below with all five terms — `w_sim`, `w_co`, `w_conf`,
`w_rec` (phase 5), `−w_div`. **Deferred:** the cluster-tree beam descent in the
candidate-pool step below — the flat scan *is* the pool until it is profiled
slow (see "Cluster hierarchy").

#### Candidate pool

```
q    = embed(retrieval_query)
beam = [root]
for level in 1..L:
    children = flatten(c.children for c in beam)
    beam     = top_B(children, key = cos(q, child.centroid))        # B ~ 3
pool  = { p : p in leaf.member_ids for leaf in beam }
pool |= top_M(all current procedures, key = cos(q, p.trigger_embedding))  # M ~ 15 — the safety net
pool  = { p in pool : scope(p) applies to this session }
```

The cluster tree is an **accelerator, not a gate** — the flat top-M NN pool runs
alongside it so a mis-clustered procedure is never invisible.

#### Scoring

`S` = the set already selected (initially empty). Each candidate `p`:

```
score(p, S) =   w_sim  · cos(q, p.trigger_embedding)
             + w_co   · cooccur_boost(p, S)
             + w_conf · confidence(p)
             + w_rec  · recency(p)
             − w_div  · max( cos(p.trigger_embedding, s.trigger_embedding) for s in S )

cooccur_boost(p, S) = mean over s in S of  edge[p][s]                 # 0 when S empty; edge is the co-occurrence EMA weight
recency(p)          = exp( −λ_r · days_since(p.last_used_at) )

# starting weights, all tunable, deferred to real data
w_sim = 1.0   w_co = 0.5   w_conf = 0.3   w_rec = 0.1   w_div = 0.3
```

#### Selection

```
S = []
seed = argmax_p ( w_sim·cos(q, p.trigger_embedding) + w_conf·confidence(p) )
S.append(seed);  budget -= tokens(seed)

while budget > 0 and len(S) < max_count:
    p* = argmax over (pool − S) of score(p, S)
    if score(p*, S) < floor:  break
    if tokens(p*) > budget:   break
    S.append(p*);  budget -= tokens(p*)
```

**Why this shape:** the `w_co` term is the whole point. Without it, retrieval
returns the *n* procedures individually closest to the query. With it, once
"organize a Go service" is selected, "write a conventional commit" gets a boost
*because they succeeded together in real runs*, even though its own query
similarity is middling. The bundle emerges from usage, not an authored
hierarchy.

---

### The co-occurrence graph — BUILT (phase 4)

`skill_cooccurrence` (migration `016`), updated by `RecordSkillOutcome` and read
by `SkillDiscover`. `store.update_cooccurrence(this_turn_ids, recent_ids, reward)`;
`store.edge_weights(ids)`. `select` blends the `w_co` term (`W_CO = 0.5`).

Phase-4 scope: **`R` = procedures composed into earlier turns of the *same
session*** (`store.session_composed_procedure_ids`). Cross-session /
project-window linking — the long-horizon "the release over 4 days" case —
needs project scope wired, which isn't yet. A new edge starts at `γ·reward`
(EMA from zero — a pairing proves itself over repeated co-occurrence); the
prune sweep drops edges below `COOCCUR_FLOOR = 0.02` or unseen for
`COOCCUR_FORGET_DAYS = 90`.

Original design:

Each edge weight is itself an **EMA of the pair's joint success** — no separate
count-and-decay bookkeeping:

```
# at turn end: P = procedures composed into THIS turn,
#              R = procedures composed into earlier turns of the same session
#                  or the same project within a window
for {a, b} in unordered_pairs(P) ∪ { {p, r} : p in P, r in R }:
    edge[a][b]  <-  (1 - γ) · edge[a][b]  +  γ · reward         # γ ~ 0.15
    last_seen[a][b] = now()
drop edges where edge < ε or now() - last_seen > T_forget
```

- **`edge[a][b]` is the retrieval `w_co` weight directly** — a smoothed "when
  these two ran together, how often did the task succeed", 0–1, recency-biased.
- **The `R` term is the long-horizon answer.** "Cut a release branch",
  "promote staging to prod", "monitor the rollout" are three turn-bounded
  procedures. If they keep co-occurring across a project's sessions, the graph
  links them and retrieval surfaces them as one bundle — a multi-day procedure
  that never needed a multi-day reward. The window (`~a few days`, project-scoped)
  is the only new knob.
- "Used together" is approximated as "composed into the same turn / session /
  project window". A sharper signal (the model's tool calls matching a
  procedure's steps) is in the transcript — not worth parsing for v1. The graph
  is also raw material for a later move: clusters by co-occurrence community
  detection rather than embedding proximity. Deferred.

---

### The cluster hierarchy — DEFERRED (not built)

**Build trigger:** the flat cosine scan in `SkillDiscover` over every current
in-scope procedure is measurably slow in profiling — not before. At seed-set /
few-hundred-procedure scale it is not, so this whole section (the
`skill_clusters` table, `RebuildSkillIndexWorkflow`, the nightly per-tenant
Temporal schedule, the beam descent in the retrieval candidate pool) is
designed and left unbuilt. Per-procedure `cluster_radius` (phase 5) plus the
`divergence` re-version handle drift incrementally in the meantime.

Per-tenant Temporal schedule, nightly. `RebuildSkillIndexWorkflow` drops
`skill_clusters` and rebuilds from current `skill_procedures`. Fully derived —
no migration, no node surgery.

1. **Agglomerative clustering** over trigger embeddings — average linkage,
   cosine distance, full dendrogram.
2. **Cut at L distance thresholds** `{t₁ < t₂ < t₃}` for L levels.
3. Per node: `centroid` = normalized mean of member embeddings; `member_ids`.
4. **Label** — one small model call per internal node, *or* carried forward:
   match each new cluster to the nearest prior-rebuild cluster by centroid
   cosine; above `t_match` (~0.9) reuse the old label. Keeps the map stable.

**Cost ceiling:** average-linkage HAC is O(n²) time and memory. Fine to
~5–10k procedures per tenant. Past that, switch construction to bisecting
k-means (O(n log n)) or build levels over an HNSW graph's layers — the
retrieval algorithm doesn't change. Named, not built.

---

### Composition — removed

There is no composition step. The staged `kind='skill'` rows are the full
rendered procedures; the **planning turn** inside the `PlanWorkflow`
([`request-pipeline/08-planning.md`](request-pipeline/08-planning.md)) reads
them from its prompt (alongside memory and discovered tools) and drafts the
checkpoint plan — ordering, tool binding, and slot-filling are the model's job
at draft time, and each checkpoint turn re-plans the tail with real results in
hand. `compose.py`, `CompositionError`, `kind='composed'` are all gone.

---

### Temporal shape

| Unit | Kind | Cadence | Does |
|---|---|---|---|
| `SkillDiscover` | activity | per task-run (step 5, in `RoutingWorkflow` on the planning turn) | retrieval, stages `kind='skill'` under the plan_id |
| `RecordSkill` | activity + `RecordSkillWorkflow` | **once, when a task-run closes**, detached (`ABANDON`), 5-min timeout | EMA-updates each retrieved procedure's `confidence` + `trigger_embedding`, updates co-occurrence edges, then match-or-inserts against `skill_procedures` (inline `generalize` on an insert / re-version) |
| `RebuildSkillIndexWorkflow` | workflow + schedule | nightly per tenant | **deferred** — re-clusters all trajectories → `skill_clusters`; builds only when the flat scan is profiled slow |

**Degradation** — retrieval is best-effort and additive: no procedures →
`SkillDiscover` returns `empty`, the planning turn drafts from scratch. Embedding
outage → `error`, `RoutingWorkflow` records it, the turn proceeds un-enriched.
`RecordSkill` failing → logged; the run is already closed, nothing user-facing
breaks.

**Worker placement** — all tenant-worker (tenant Postgres, embedding creds,
medium-tier model). `RoutingWorkflow` on the loop-worker only dispatches by name.

---

### Build phases

Each phase leaves something concrete to test. Never build a later phase's
mechanism against an empty store.

1. **Retrieve + compose over a seed set.** Hand-write 10–30 `authored`
   procedures. `skill_procedures`, flat `cos` search (no hierarchy, no
   co-occurrence), `SkillDiscover` → `ComposeSkill` → into the prompt. Prove the
   hot path helps.
2. **Recording + online updates.** `RecordSkillOutcome`, `skill_candidates`, the
   EMA updates to `confidence` / `trigger_embedding`. Now the seed set adapts to
   real use even before synthesis exists.
3. **Synthesis.** `SkillSynthesisWorkflow` — candidate assignment, first-success
   creation, then refinement, then divergence, then failure annotation. The hard
   part; most iteration here.
4. **Co-occurrence.** The graph + the `w_co` term + the cross-session/project
   window. Measure bundle coherence and whether multi-turn procedures link up.
5. **Recency + adaptive radius.** BUILT. `select`'s `w_rec` term
   (`exp(−λ·days_since(last_used_at))`) and a per-procedure `cluster_radius`
   derived from each procedure's own trajectory spread at synthesis time.
6. **Cluster hierarchy.** DEFERRED. `RebuildSkillIndexWorkflow` + beam descent —
   built only once the flat cosine scan over the store is *profiled* slow or
   noisy, not before.

---

### Resolved stances / accepted costs

- **Synthesis quality — the user is the judge, not a metric.** Procedure quality
  is subjective; there is no automated eval and none is planned. The quality
  mechanism *is* the correction path: a bad procedure produces a `divergence`
  re-version or a `failure` annotation, and its EMA confidence falls. The floor
  is held by the **seeded authored set** (`confidence 0.7` — synthesis then only
  ever *refines* the common procedures, never invents them cold), the
  schema-constrained prompt, and the low-confidence "unverified" gate. Above
  that floor, procedures are taught by use.

- **Cluster tuning — standard values, revisit with data.** Start with:
  agglomerative, average linkage, cosine distance; `cluster_radius` = mean
  pairwise member cosine minus a fixed slack; a small fixed set of cut
  thresholds for the hierarchy levels. A bad cut merging "deploy the API" with
  "deploy the frontend" is possible — accepted for v1, tuned once there's real
  usage. Inspecting what synthesis produced is just querying `skill_procedures`
  directly (`body`, `provenance`, `member_candidate_ids`, `confidence`) for v1;
  a dedicated admin view can follow if the raw table proves too coarse.

- **Stale skill — an accepted, bounded cost.** The EMA confidence
  (`α = 0.2`, "Confidence") drops a suddenly-failing procedure below the
  injection floor in ~4 runs. The only cost is those first few turns are
  degraded before the correction propagates. That is the price of learning; a
  `divergence` re-version fixes the body. No recency rule.

- **Long-horizon, multi-session tasks — co-occurrence chains.** "The release" is
  three turn-bounded tasks, each with its own reward and procedure; the
  cross-session/project co-occurrence window ("Co-occurrence") links them into
  one bundle. Window length: **standard best-effort** (~a few days,
  project-scoped), tuned later. An explicit "task done, remember this" signal is
  a possible future addition, not v1.

- **Cross-episode numeric calibration ("batch when N > 50") — known gap, marked
  future.** A threshold comes from aggregate observation, not any one
  trajectory; this design cannot learn it. Out of scope for v1+. Revisit only if
  it proves to matter, via a separate statistical pass over transcripts.

- **The small ones — standard / best-effort defaults:**
  - *Cluster-label churn across rebuilds* — accepted; labels are for human
    browsing only, churn doesn't touch retrieval. Carry-forward heuristic is
    best-effort.
  - *`scope` shadowing* — done in **retrieval** (filter + rank), the simplest
    place.
  - *Cross-procedure contradiction* — composition places both and lets the model
    adjudicate (same "placement, not reconciliation" stance as memory
    adaptation); the conflict is noted in the composed block's provenance.
  - *Co-occurrence community detection as the tree* — deferred; embedding
    clusters are the default.

### Notes Log

- 2026-08-31: Introduced. The design conversation moved skills from
  "authored + hosted in agent-brain, composed agent-brain-side" (the earlier
  `05`/`06` framing) to a harness-owned procedural memory that learns from its
  own execution — prompted by "skill discovery and composition needs to be a
  harness responsibility ... the harness tries to record the skills during its
  lifecycle and also retrieve and reuse", then "a tree of skills" which was
  reshaped into flat store + derived cluster hierarchy + co-occurrence graph
  (a strict tree forces single-parenting and node-surgery thrash; a *derived*
  hierarchy gives the same compositional retrieval without either).
- 2026-08-31 (later): **Reward model sharpened.** A procedure is the effective
  trajectory of a *successful* task (plan + mid-task corrections, flattened),
  not the planner's raw output — episodic self-imitation with a terminal 0/1
  reward, confidence as the value estimate. Cache on the first success
  (repetition → refinement, not creation); only success produces a body, failure
  produces annotations; a later separate correction is just a `divergence`
  re-version. Two regimes named (memoization vs. accumulated learning).
- 2026-08-31 (even later): **Open questions given dispositions.** Synthesis
  quality → the user teaches, no automated metric planned (the correction path
  is the mechanism). Cluster params → standard values now, revisit with data;
  inspecting synthesis output is just querying `skill_procedures` for v1, a
  dedicated admin view later. Numeric calibration → known gap, marked future.
  Small ones → standard/best-effort defaults (scope shadowing in retrieval;
  contradiction = placement not reconciliation; label churn accepted; community
  detection deferred). Section retitled "Resolved stances / accepted costs" —
  no genuine to-dos remain, only a marked-future gap.
- 2026-08-31 (later still): **Five refinements from a review pass.**
  (1) A procedure is a *cluster* of trajectories with a per-procedure
  `cluster_radius`, not a `t_dedup` threshold; the nightly rebuild re-clusters.
  (2) Recording gated to `complexity` `moderate`/`complex` (step 2) — trivial
  tasks never enter the store. (3) Long-horizon multi-session tasks resolved as
  a co-occurrence *chain* of turn-bounded procedures, via a new
  cross-session/project co-occurrence window — no monolithic multi-day reward.
  (4) `confidence`, `trigger_embedding`, and co-occurrence edges are all
  **EMA-updated** online (`α`/`β`/`γ` ≈ 0.2/0.15/0.15); the confidence EMA is
  what makes staleness self-correct in ~4 runs, so the recency-cap mitigation
  was dropped. Body updates are "weighted" via the generalization prompt
  preferring recent trajectories + on-demand divergence re-synthesis.
  (5) Seeded authored procedures reframed as an ongoing quality floor, not just
  a bootstrap. Prompted by the user's point-by-point response to the open
  questions ("its ok to learn from every run ... ideally weighted learning").
