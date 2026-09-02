# Component: Episode Lifecycle

> STATUS: BUILT (2026-09-01). Migration `018`; `activities/episode.py` +
> `OpenEpisode` / `CompleteEpisode` / `CloseSubagentEpisode` /
> `CloseSessionEpisodes` activities; `CloseSessionEpisodesWorkflow`;
> `turn.go` / `coordinator.go` / `routing.go` wired; `ClassifyRequest` gains
> `continues_prior`; `RecordSkillOutcome` and the plan ledger / staged
> retrieval re-keyed on `episode_id`. Compile + `go test` clean; live
> verification via `workflows/scenarios/superpowers-b/`.
>
> **Pre-existing bug found during live verification (2026-09-01):** the Session
> Coordinator's idle-exit check (`coordinator.go`) fired on *every* turn
> completion, not only on the idle timer — so a coordinator terminated the
> instant each turn ended (with no queued follow-up) and was recreated by the
> next message. Harmless before episodes (no per-session state was lost);
> fatal for them, because `CloseSessionEpisodes` on that exit closed every
> episode before a follow-up turn could attach. Fixed: a `wasIdle` guard so
> the exit only triggers when the coordinator was genuinely idle-waiting —
> giving a real post-turn grace window (`idleTTL`), which is what episode
> attachment needs. The three scripted scenarios (`skill-plan-integration`,
> `plan-progress-lifecycle`, `subagent-full-agent`) pass against the deploy;
> the multi-turn attachment path is covered by the `episode-multiturn-*`
> chained pair.
>
> **Deviation from this doc as first written:** the tables were NOT renamed
> (`turn_retrieval` / `turn_plan` keep their names) — only the key column
> `turn_id` → `episode_id`, to keep the blast radius across code and docs
> small. `skill_candidates.turn_id` also kept its name (it holds the anchor
> turn_id, a valid `turns` FK). The "≥ half terminal" supersede-record rule
> was dropped: a superseded episode records with `outcome = failure` and
> `record.py`'s own `intent`/`complexity` gate decides whether it's worth
> recording at all.
>
> Fixes the multi-turn synthesis fragmentation found in the superpowers eval
> (see `skill-subsystem.md` Notes Log / the eval memory): an 8-turn teaching
> conversation produced 4 hyper-specific `learned:*` procedures instead of one
> general skill, because the pipeline treats every user turn as its own task —
> its own classify → route → discover → compose → **fresh `turn_plan`** → its
> own `RecordSkillOutcome`.
>
> Parent: [`request-pipeline.md`](request-pipeline.md),
> [`skill-subsystem.md`](skill-subsystem.md).
> Reshapes: [`request-pipeline/08-planning.md`](request-pipeline/08-planning.md)
> (the plan becomes episode-scoped), `skill-subsystem.md` "Recording" (one
> candidate per episode), `turn.go` / `coordinator.go` (episode lifecycle).

### The simple model this restores

> *"A plan was created for a particular task. That plan is executed across
> multiple turns. Once that is complete, we record it. Same for subagents."*

That is the correct model. It is **not** what the code does today. Today:

- `turn.go` runs the whole pre-LLM pipeline for **every** top-level turn where
  `intent == task` — including re-seeding `turn_plan` from a fresh `ComposeSkill`
  call. `turn_plan.turn_id` is a foreign key to *one* turn.
- `RecordSkillOutcome` fires at **every** task turn's end.

So the rate-limiter conversation created **5 separate plans** (turns 3, 5, 6, 7,
8) and **4 separate candidates** for one obvious task. Nothing links them. The
"living checkpoint ledger" that `08-planning.md` describes as the task's spine is
rebuilt from scratch each turn.

A subagent, by contrast, already works the right way — it is one `TurnWorkflow`,
handed a self-contained prompt, run to completion, recorded once. Its task and
its turn coincide. **A top-level task is a subagent the user steers
interactively** — same lifecycle, human turns interleaved. The code assumed
`turn == task`, which is true for subagents and false for interactive work.

### The unit: an episode

An **episode** is one task from first message to completion. It owns:

- the **plan** (`turn_plan`, the checkpoint ledger) — seeded once,
- the **staged retrieval** (`episode_retrieval`: composed skill, memory, tools,
  skill rows) — snapshotted once, refreshed on continuation,
- an accumulating **trajectory** — every turn's messages and tool calls,
- a **status**: `open` → `complete` | `abandoned` | `superseded`.

A session has **at most one open episode**. Every top-level turn either opens a
new episode or attaches to the open one. Recording happens **once**, when the
episode closes.

`episode_id` is the anchor (first) turn's `turn_id` — no new id scheme, and the
episode is trivially joinable from any of its turns.

### Data model

Migration `018_episodes.sql` (mirrored to
`deploy/helm/agent-harness-tenant/files/`):

```sql
CREATE TABLE episodes (
  episode_id      text PRIMARY KEY,                 -- = the anchor turn_id
  session_key     text NOT NULL REFERENCES sessions(session_key),
  status          text NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open','complete','abandoned','superseded')),
  intent          text NOT NULL DEFAULT 'task',
  retrieval_query text NOT NULL DEFAULT '',
  task_embedding  real[],                            -- anchor message embedding, for the degraded continuation check
  last_stop_reason text NOT NULL DEFAULT '',          -- the most recent turn's loop stop_reason — feeds the outcome reward
  opened_at       timestamptz NOT NULL DEFAULT now(),
  closed_at       timestamptz,
  close_reason    text                               -- 'plan_complete' | 'superseded' | 'idle' | 'turn_end' | 'abandoned'
);
CREATE INDEX episodes_session_open_idx ON episodes (session_key) WHERE status = 'open';

ALTER TABLE turns ADD COLUMN episode_id text REFERENCES episodes(episode_id);
CREATE INDEX turns_episode_idx ON turns (episode_id);
```

One column rename on each of the two staging tables — they were already
turn-tree-scoped in spirit, this makes the scope explicit (no back-compat, per
project policy). Table names kept:

- `turn_retrieval.turn_id` → `episode_id`.
- `turn_plan.turn_id` → `episode_id` (PK and `turn_plan_order_idx` follow).

The FK still targets `turns(turn_id)` — an `episode_id` IS the anchor turn's id.
`real[]` embeddings, consistent with `skill_procedures` — no pgvector here
either.

### Lifecycle

#### Open

A top-level turn arrives → `InsertMessage` (creates the `turns` row) →
`ClassifyRequest`. Then:

| classify result | episode action |
|---|---|
| `intent == conversational` | none — `turns.episode_id` stays null, fast path as today, the open episode is untouched |
| non-conversational, **no open episode** | **open** — `episode_id = turn_id`, run the full pipeline |
| non-conversational, **open episode exists**, classifier says this continues it | **attach** — `turns.episode_id = <open episode>`, skip route/discover/compose/**plan-seed**, refresh only (below) |
| non-conversational, **open episode exists**, classifier says this is a new task | **supersede** the old (close + record it), then **open** a new one |

**Continuation detection.** `ClassifyRequest` gains a `continues_prior` boolean.
It already reads a recent-conversation tail and resolves follow-up references
("yes do that" → the task); it is now also handed a one-line summary of the open
episode (its `retrieval_query` + the last assistant message) and asked whether
the new message continues that work or starts something new. The fast tier is
well-suited — it can tell "here's the Redis approach you asked about" (a
continuation) from "now help me with the deploy script" (a new task), which a
raw embedding threshold cannot (this is exactly why the eval's turns failed to
cluster at `ASSIGN_RADIUS`).

Degraded path (classifier returned `confidence == 0.0`): continue if an episode
is open and `intent != conversational` and
`cosine(new anchor embedding, episode.task_embedding) >= CONT_FLOOR` (start
`0.55`, biased toward continue — a false "new" re-fragments, a false "continue"
only adds a stale composed block the model can ignore). Numeric-tuning
discipline: revisit with real data.

#### Execute

- **New episode:** full routing keyed on `episode_id` (`startRouting` takes
  `episode_id`; for a new episode it equals the turn_id, so this is identical to
  today). `ComposeSkill` seeds `turn_plan` under `episode_id`. Tool discovery,
  plan seeding, and the initial `Route()` gate happen **once per episode, here.**
- **Continuation turn:** no full routing. Dispatches a detached
  `RoutingWorkflow` in `Mode = "reconcile"` — refreshes `episode_retrieval`
  `kind='memory'` / `kind='skill'` against the accumulated conversation and
  regenerates the composed block, but **does not** re-seed the plan or re-run
  `ToolDiscover` (`08-planning.md`'s reconcile mode, unchanged). Memory doesn't
  go stale across a long episode, and a genuinely new sub-need still surfaces.
- Either way the reason-act loop runs against the **episode's** plan and staged
  retrieval. `prompt.assemble` / `ModelCall` / `plan.apply_progress` resolve
  `episode_id` from `turns.episode_id` (or take it on the activity input) and
  read/write `turn_plan` + `episode_retrieval` by it. `plan_progress` advances
  the one ledger across every turn of the episode.

#### Close

| trigger | who | close_reason | record? |
|---|---|---|---|
| plan all checkpoints terminal (`done`/`skipped`) at a turn's end | `turn.go` | `plan_complete` | yes |
| a new task supersedes it | `turn.go` (open path) | `superseded` | yes, if the plan was ≥ half terminal or the last turn stopped clean; else close silently |
| coordinator idle-exit | `coordinator.go` → `CloseSessionEpisodesWorkflow` | `idle` | yes |
| subagent turn ends | `turn.go` (subagent) | `turn_end` | yes (unconditional — a subagent turn ending *is* its task ending) |

A **no-plan episode** (fast-pathed route, or `SkillDiscover` empty — no
checkpoint ledger) can't hit `plan_complete`; it closes on supersede or idle.
The up-to-`idleTTL` delay before its candidate is written is immaterial —
synthesis is already async and debounced.

A **brainstorming-shaped episode** never completes its (often ill-matched) plan,
so it stays open across all its turns and records once at idle — **exactly the
fix.** One candidate, whole-conversation trajectory, whole `plan.render_final`.

#### Record (once)

`RecordSkillOutcome` takes `episode_id`:

- messages + tool calls gathered across **every turn in the episode** —
  `... FROM messages m JOIN turns t ON m.parent_id = t.turn_id
   WHERE t.episode_id = $1 ORDER BY t.turn_seq NULLS FIRST, m.seq`
  (nested-subagent turns have their *own* `episode_id`, so they're excluded here
  and recorded under their own episode — no double-count).
- `task_text` = the episode's anchor (first) user message.
- `composed_from` = `episode_retrieval` `kind='skill'` rows.
- `outcome` = success when `close_reason == 'plan_complete'`, or
  (`idle`/`turn_end`/`superseded`) with the last turn's `stop_reason` clean and
  no tool errors; failure otherwise.
- `required_correction` = more than one user *turn* in the episode. (Brainstorming
  is inherently multi-turn Q&A — a later reward-model refinement should
  distinguish "clarifying dialogue" from "correction"; noted, not in scope here.)

Dispatched detached (`RecordSkillOutcomeWorkflow`, `ABANDON`) by whoever closes
the episode. Synthesis trigger + `"skill-synthesis"` debounce unchanged.

### Subagents

`08-planning.md` already made subagents full agents. Under episodes: a subagent
opens its own episode at turn start (`episode_id = subagent turn_id`), keyed the
same way, and closes it when its turn ends. Its lifecycle is a single turn —
which is why subagents already behaved correctly and continue to. One code path
for both.

### Reconciliation, unified

`08-planning.md`'s reconciliation trigger fired a `Mode="reconcile"`
`RoutingWorkflow` on a **mid-turn** follow-up (a second message while a turn is
still running). Episodes add a second caller: a **between-turn continuation**
fires the same reconcile. "Reconcile" becomes simply *refresh the episode's
memory/skill rows and composed block against the latest input, leave the plan
alone* — and it has two triggers now instead of one. `replace_rows` keys on
`episode_id`.

### What runs once per episode vs every turn

| once per episode (open) | every turn |
|---|---|
| `Route()` gate | `ClassifyRequest` (+ `continues_prior`) |
| `ToolDiscover` | reconcile refresh of memory + skill rows |
| `ComposeSkill` **plan seed** | `ComposeSkill` prose regen (reconcile) |
| `episodes` row + `task_embedding` | the reason-act loop, `plan_progress` |

The pipeline stops re-running wholesale on every follow-up — a latency and cost
win on top of the correctness fix.

### Scope

**Migration:** `018_episodes.sql` (+ helm mirror) — `episodes`, `turns.episode_id`,
two renames.

**Python — real new logic:**
- `activities/activities/episode.py` (NEW) — `open_or_attach` / `attach` /
  `complete_if_plan_done` / `close` / `close_session_open` / `read_open`, all
  taking a conn.
- activity defns `OpenEpisode` / `CompleteEpisode` / `CloseSessionEpisodes`
  (registered in `tenant_worker.py`).
- `classify.py` — `continues_prior` (prompt + parse + open-episode hint);
  `TaskRepresentation` field.
- `skills/record.py` — `episode_id` input, cross-turn trajectory gather.

**Python — mechanical (`turn_id` → `episode_id` keying):** `plan.py`,
`retrieval/staging.py`, `retrieval/{skills,memory,tools,compose,reconcile}.py`,
`model_call.py`, `llm.py`, `prompt.py`, `types.py`.

**Go:**
- `workflow/turn.go` — the open/attach/supersede block after `ClassifyRequest`;
  `startRouting(episodeID, …)`; continuation path; end-of-turn
  `CompleteEpisodeIfPlanDone` → dispatch record; subagent opens/closes its own.
- `workflow/routing.go` — key on `EpisodeID`; continuation → `Mode="reconcile"`.
- `workflow/coordinator.go` — idle-exit dispatches `CloseSessionEpisodesWorkflow`
  alongside `WriteMemoryWorkflow`.
- new `CloseSessionEpisodesWorkflow`; `RecordSkillOutcomeWorkflow` /
  `RecordSkillOutcomeInput` take `EpisodeID`; `types/types.go` mirrors.

**Scenarios:** update `plan-progress-lifecycle` / `skill-plan-integration` /
`subagent-full-agent` `.expect.sh` for `episode_id` keying; add a multi-turn
episode → single-candidate case.

**Docs:** rewrite `08-planning.md`'s plan-scope + reconciliation sections to
point here; `skill-subsystem.md` "Recording"; `request-pipeline.md` Notes Log.

### Deferred

- **Reward-model refinement** for clarifying dialogue vs correction (see Record).
- **Stale-episode reaping** on coordinator restart (close `open` episodes older
  than a threshold before starting the next turn) — a crash-safety valve, add if
  observed.
- **Eager `complete` for no-plan one-shot episodes** with a short "reopen on
  immediate continuation" window — rejected for v1 as more state than the
  idle-close delay is worth.
- **The mismatched-plan problem itself** (brainstorming got `implement-change`
  checkpoints) — a separate fix (confidence/similarity floor on what
  `ComposeSkill` will seed a plan from; `generalize` prompt describing task
  *shape* not topic). Episodes make it *harmless* (one unused plan, not five);
  it's still worth closing.

### Open Questions

- **`continues_prior` when the classifier is confident but wrong** — a false
  "new task" mid-episode fragments again. Watch the episode/turn ratio in the
  eval; the fix if it bites is a similarity guardrail that can veto a "new"
  verdict when the message is highly similar to the open episode.
- **`CONT_FLOOR`** and the "≥ half terminal" supersede-record rule — both
  numeric, both start at a guess.
- **Very long episodes** (a 30-turn debugging session) — the trajectory gather
  is capped at `_MAX_TRANSCRIPT_CHARS` today; episode-scale may want a smarter
  reduction (keep the plan-final + head + tail) rather than a hard truncate.
- **Explicit "new task" / "done" user signal** — a lightweight way for the user
  to say "different thing now" without relying on the classifier. Deferred until
  the classifier is shown insufficient.
