# Scenarios — Regression Suite

A growing suite of scripted Temporal scenarios, run against the
**already-deployed live cluster workers** (no local worker binaries, no
scaling anything down) — the thing to run after any change touching
`ModelCall`, `turn.go`'s dispatch loop, tool_calls minting, the `lcm/`
package, or the pre-LLM request pipeline, instead of re-deriving
verification from scratch every time.

> **Not zero-cost any more.** The scripted-fixture path replaces only the
> reason-act loop's own model calls. Since the request pipeline landed
> (`docs/components/request-pipeline.md`), `turn.go` runs steps 2–8 for
> every turn regardless of fixtures — `ClassifyRequest` (fast tier),
> `RoutingWorkflow` → `MemoryRetrieve` (agent-brain), `SkillDiscover`
> (embeddings), `ToolDiscover` (mcp-hub), `ComposeSkill` (medium tier). So
> each scenario turn now spends a few cents of real fast/medium-tier LLM +
> a handful of backend calls. Cheap, but real — this is deliberate: those
> steps are exactly what the newer scenarios verify.
>
> The reason-act model calls are still fixture-scripted in every scenario
> **except `real-assembly`**, which deliberately omits fixtures so its one
> ModelCall runs step 9 (`prompt.assemble` → `lcm.assemble`) for real
> against a seeded multi-turn history — the assembly path every real turn
> takes and every other scenario short-circuits. One extra real fast-tier
> call; it's the only place `prompt_assemble_latency_seconds` comes from.

## Running it

```
kubectl port-forward -n core svc/temporal-frontend 17233:7233 &
workflows/scenarios/run_all.sh
```

That's the whole regression suite: every scenario in `run_all.sh`'s
`SCENARIOS` list runs via `run_scenario.sh`, each checked against its own
`<name>.expect.sh` real Postgres assertions (not "did it not crash" — actual
expected end-state), with a final PASS/FAIL/NO-ASSERTIONS summary.

To run just one scenario while iterating on something specific:
```
workflows/scenarios/run_scenario.sh <scenario-name>
# or against a specific, reusable session key:
workflows/scenarios/run_scenario.sh <scenario-name> test:my-debug-session
```

No local Postgres port-forward is needed — every Postgres read/write goes
through `kubectl exec ... psql` server-side; the password never touches a
local shell variable, same convention this whole project uses.

## Adding a new case

1. Write `<name>.json` — a scripted scenario (see any existing file for the
   shape: `{"message": {...}, "scripted_model_responses": [...]}`).
2. If it needs Postgres state to exist *before* the scenario's own turn
   starts (e.g. pre-folded `context_summaries` for an `lcm_*` tool test —
   see `lcm-retrieval.setup.sql`), write `<name>.setup.sql` with
   `{{SESSION_KEY}}` placeholders where the real session key goes.
3. Write `<name>.expect.sh` — real assertions against Postgres, given
   `$1`=session_key, `$2`=root turn_id, using the `pg_query` helper pattern
   every existing `.expect.sh` uses. Must `exit 0` on pass, nonzero on
   fail, printing what it checked as it goes (see any existing
   `.expect.sh` for the pattern). **A scenario with no `.expect.sh` isn't
   real coverage** — `run_scenario.sh` reports it `NO-ASSERTIONS`, and
   `run_all.sh` treats that as a suite failure, not a pass.
4. Add `<name>` to `run_all.sh`'s `SCENARIOS` list.

That's the whole process — no other registration needed.

## What's NOT in the automatic suite, and why

- **`real-llm-basic.json`** — needs a real provider API key and spends real
  money. Run manually (`run_scenario.sh real-llm-basic`, with the real
  provider env vars set) when verifying real-provider integration
  specifically, not part of the free regression run.
- **Chained pairs** — `interrupt-initial.json`/`interrupt-followup.json`
  and `subagent-merge-cancelled-initial.json`/`-cancelled-followup.json`.
  Each pair is two scripted scenarios run against the *same*
  still-running session (the second targets the first's active turn as a
  follow-up), which `run_scenario.sh`'s one-shot-per-call shape doesn't
  support yet. Run manually — two calls back to back against the same
  explicit session key — until a chained-pair mode is added here. A real,
  named gap, not silently dropped from coverage.
- **`deep-conversation/`** — a separate, heavier real-LLM validation suite
  (memory writes, real context compression, tier escalation, mcp-hub
  discovery) with its own `run.sh` that scales cluster Deployments to 0
  and runs local worker binaries against a real model. Different purpose
  (full-stack validation with real cost) from this directory's free
  regression suite — see its own `README.md`.
- **`superpowers-b/`** — an **eval flow** for the skill subsystem: no seeds,
  the agent is taught a process ([superpowers](https://github.com/obra/superpowers)
  brainstorming/writing-plans/…) by conversation and the RL loop turns those
  runs into `learned:*` procedures it then reuses. Driven live (a human — or
  the developer via the `starter` binary against a real web session key —
  holds real multi-turn conversations), not scripted. See its `README.md`.
- **`shell-exec-basic.json`, `shell-exec-parallel.json`,
  `multi-step-task.json`, `subagent-merge-happy.json`,
  `subagent-merge-conflict.json`** — each scripts a `shell_exec` call
  against `echo`, which `permissions.py`'s real gating rules require
  approval for. That dispatches a real `UserInputRequestWorkflow` that
  blocks for an hour waiting for a response nothing in this runner sends
  (no gateway exists in this slice to answer it). **Confirmed live, not
  assumed** — running the full list once left four of these genuinely
  stuck for 10+ minutes before being found and manually
  `temporal workflow terminate`-d. Run manually and answer the approval
  yourself until auto-approval support is added to this runner (see
  `run_all.sh`'s own comment for the exact `temporal workflow signal`
  invocation).

## Updated 2026-09-01 — episodes (docs/components/episode-lifecycle.md)

The plan ledger and the staged retrieval are now **episode-scoped**, not
turn-scoped: `turn_plan` / `turn_retrieval` key on `episode_id` (the anchor
turn_id — renamed column, migration `018`), and `RecordSkillOutcome` fires
**once when the episode closes**, not per turn. For a single-turn scripted
scenario `episode_id == root turn_id`, so the assertions below just swap
`turn_id` → `episode_id` in the `turn_plan` / `turn_retrieval` queries.

Consequence for these scenarios: a scripted turn whose `plan_progress` does
**not** drive every checkpoint terminal leaves the episode *open* at turn end —
it closes (and records) on the coordinator's idle-exit (~`idleTTL`, 30s here)
via `CloseSessionEpisodesWorkflow`. `skill-plan-integration.expect.sh`'s
candidate poll is sized (~55s) to wait that out. A subagent's episode still
closes at its turn end (`CloseSubagentEpisode`), so `subagent-full-agent`
records promptly. `cleanup_test_data.sh` also clears the new `episodes` table.

The multi-turn episode → **single** candidate behaviour (the whole point of
the change) has its own chained pair, **`episode-multiturn-initial.json` /
`episode-multiturn-followup.json`** (run manually, same shape as `interrupt-*`
— back to back, same session key, within `idleTTL` so the coordinator hasn't
closed the episode):

```
KEY="test:episode-mt:$(date +%s)"
workflows/scenarios/run_scenario.sh episode-multiturn-initial  "$KEY"
workflows/scenarios/run_scenario.sh episode-multiturn-followup "$KEY"   # run_scenario waits on turn:1, so
                                                                        # turn:2 may still be running when its
                                                                        # expect.sh fires — re-run the expect
                                                                        # by hand once turn:2 completes, or just
                                                                        # read episode-multiturn-followup.expect.sh
```

`episode-multiturn-followup.expect.sh` asserts: both turns carry the **same**
`episode_id`, exactly **one** `episodes` row, and exactly **one**
`skill_candidates` row whose transcript spans both turns (after the ~`idleTTL`
idle close records it). Verified 2026-09-01 against the `agents` deploy — one
candidate, `outcome=success`, `required_correction=true`.

`superpowers-b/` remains the broader live eval (a real teaching conversation,
no scripting).

### Lane model (added 2026-09-01 — docs/components/lane-model.md)

- **`episode-plan-complete`** and the retuned `plan-progress-lifecycle` /
  `episode-upgrade-initial` messages are all deliberately Deliberate-lane.
- **`lite-simple-task.json`** (NOT in `run_all.sh`) — checks a simple task takes
  the Lite lane: no episode, `turn_retrieval` memory-only, no `turn_plan`, no
  `skill_candidates`. Its `expect.sh` reads the `classify` log line and **skips**
  (rather than fails) if the classifier rated the turn moderate/complex — so
  it's only real coverage while the fast-tier classifier is healthy. Run it by
  hand once `classify[...] conf` is back above 0.

### Episode close-trigger coverage (added 2026-09-01, verified against the deploy)

- **`episode-plan-complete.json`** (in `run_all.sh` — single turn) — the
  scripted `plan_progress` drives every seeded checkpoint terminal, so the
  episode closes AT TURN END with `close_reason='plan_complete'` and the
  candidate lands within seconds (not the ~`idleTTL` idle sweep).
- **`episode-supersede-initial/-followup`** (run manually, chained) — turn:1 a
  task with an unfinished plan, turn:2 an unrelated task. The classifier
  returns `continues_prior=false` → OpenEpisode closes turn:1's episode
  `superseded` (recording it, `outcome=failure`) and opens a fresh one.
- **`episode-upgrade-initial/-followup`** (run manually, chained) — turn:1 a
  pure question (episode `intent='question'`, not recordable), turn:2 an
  actionable continuation of the topic. OpenEpisode attaches AND
  `_upgrade_classification` bumps the episode `question→task`, so it records.

Run the chained ones like `episode-multiturn` above; each `*-followup.expect.sh`
carries its own manual invocation line.

## Coverage added 2026-09-01 — request pipeline steps 8 & 9

In `run_all.sh` (standalone, real steps 2–8, scripted loop):

- **`plan-progress-lifecycle.json`** — the `plan_progress` meta-tool
  (`request-pipeline/08-planning.md`). `ModelCall` peels `plan_progress` out
  of the scripted tool stream, `plan.apply_progress` writes `turn_plan`:
  appends a new checkpoint for an unknown id + intent, updates status when
  the id exists, handles `done` / (no status →) `pending` / `skipped`.
  Asserts on the ids this fixture chose (`c1`/`c2`/`c3`) so it doesn't care
  whether the real pipeline also seeded a `cp1..cpN` plan from a matched
  skill (it usually does).
- **`skill-plan-integration.json`** — the whole pre-LLM pipeline end to end
  against the live deploy. A task matching the seeded `investigate-failure`
  procedure → `SkillDiscover` stages `kind='skill'`, `ComposeSkill` stages
  `kind='composed'` **and** seeds `turn_plan` (`cp1..cpN` from the merged
  procedure), the scripted `plan_progress` calls advance those seeded
  checkpoints, and `RecordSkillOutcome` writes a `skill_candidates` row. The
  `skill_candidates` check reads the `classify` log line to distinguish "the
  classifier said `simple`, correctly skipped" from a real bug.
- **`subagent-full-agent.json`** — `request-pipeline/08-planning.md`,
  "Subagents are full agents". A spawned subagent with a clearly-complex
  task now runs its own `RoutingWorkflow` (`{sub}:routing` completes) and
  gets its own `skill_candidates` row via `RecordSkillOutcome` — neither
  happened for subagents before the `ParentType=='session'` gate came off.
  Also checks `plan_progress` lands on a non-root turn_id.

Run manually (not in `run_all.sh`):

- ~~`reconcile-*`~~ — **removed 2026-09-02** (episode-lifecycle.md REVISION):
  `MemoryRetrieve` / `ToolDiscover` run per-turn now, so there is no stale
  bundle to reconcile. `RoutingWorkflow` lost its `Mode="reconcile"` path. A
  mid-turn follow-up just lands in the conversation.
- **`real-llm-pipeline.json`** — spends real money. The only end-to-end
  check of step 9 (`prompt.assemble`), which the scripted path skips
  entirely. A real ModelCall gets the composed skill + plan progress +
  capabilities + memory sections; the model's answer visibly follows the
  composed procedure. `run_scenario.sh real-llm-pipeline`.

## Coverage added 2026-08-29

`spawn-subagent-nested-valid.json` / `spawn-subagent-nested-rejected.json`
— the recursion-termination guard (`components/temporal-workflow.md`,
"Resolved: Recursion Termination Guard"): a subagent delegating to a
further subagent with genuine `delegated_scope`/`kept_work` succeeds
end-to-end (root → subagent → grandchild, all real child workflows); one
without them is rejected at mint time (no child workflow ever starts,
durably recorded as a real tool_calls error) and — the real bug this suite
caught while being built — the subagent correctly loops back for a
follow-up step to react to the rejection rather than silently ending its
turn (the `has_tool_calls` fix, see the same Notes Log entry).

`lcm-retrieval.json` (+ `.setup.sql`) — `lcm_grep`/`lcm_describe`/
`lcm_expand` (`components/context-slot.md`'s Memory-Access Tools) against a
pre-seeded, already-folded two-level summary DAG, exercising the real
`TOOL_REGISTRY`-dispatched handler code path end to end, including the
`folded_into` chain resolution.

`subagent-spawn.expect.sh` — added for the pre-existing scenario; this is
exactly what the `caller_is_subagent` `NameError` (found and fixed the same
day, alongside the guard above) would have caught immediately instead of
silently shipping to a live deploy.
