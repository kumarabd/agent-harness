# Scenarios — Regression Suite

A growing suite of scripted Temporal scenarios, run against the
**already-deployed live cluster workers** (no local worker binaries, no
scaling anything down) — the thing to run after any change touching
`ModelCall`, `turn.go`'s dispatch loop, tool_calls minting, the `lcm/`
package, or the pre-LLM request pipeline, instead of re-deriving
verification from scratch every time.

> **Not zero-cost any more.** The scripted-fixture path replaces only the
> reason-act loop's own model calls. The pre-LLM request pipeline
> (`docs/components/request-pipeline.md`) runs for real regardless of
> fixtures — `ClassifyRequest` (fast tier), `RoutingWorkflow` →
> `MemoryRetrieve` (agent-brain), `SkillDiscover` (embeddings),
> `ToolDiscover` (mcp-hub). So each scenario turn spends a few cents of real
> fast-tier LLM + a handful of backend calls. Cheap, but real — those steps
> are exactly what the newer scenarios verify.
>
> The reason-act model calls are still fixture-scripted in every scenario
> **except `real-assembly` and `resolved-tool-dispatch`**, which deliberately
> omit fixtures so their one ModelCall runs for real. `real-assembly` exercises
> step 9 (`prompt.assemble` → `lcm.assemble`) against a seeded multi-turn
> history — the assembly path every real turn takes and every other scenario
> short-circuits; it's the only place `prompt_assemble_latency_seconds` comes
> from. `resolved-tool-dispatch` (docs/components/tool-registry.md, "Resolved:
> Three-Layer Tool Taxonomy & Per-Task Resolution") exercises per-task tool
> resolution against a seeded `turn_retrieval` row, checking a real model calls
> the resolved tool **by name** (not `search_tools`/`call_tool`) and that
> `ModelCall` mints the right `resolved_server`/`resolved_tool` for `ToolCall`
> to route on. Two extra real fast-tier calls total.

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
- **Chained pairs** — `interrupt-*`, `subagent-merge-cancelled-*`, and the
  `plan-continuation-*` / `plan-supersede-*` pairs (see the plan-and-execute
  section below). Each is two scripted runs against the *same* still-running
  session — run manually, two calls back to back against one explicit session
  key. The `plan-*` pairs additionally need the second run to land inside the
  first's `slow_tool` window (~12s).
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

## Plan-and-execute scenarios (docs/components/request-pipeline/08-planning.md)

A Deliberate task is a **`PlanWorkflow`**, not one reason-act turn: `turn:1` is
the *planning turn* (`PlanningMode`) whose scripted response calls
`propose_plan`; the PlanWorkflow then runs one *checkpoint turn* per checkpoint
at the deterministic id `<turn:1>:cp:<n>`, each scripted to call
`checkpoint_done`; then one async `RecordSkill` over the whole trajectory.

The starter scripts checkpoint turns from a `checkpoint_responses` array in the
scenario JSON (`checkpoint_responses[i]` → `<turn:1>:cp:<i+1>`). The
`propose_plan` call must list exactly that many checkpoints and must **not** set
`needs_approval` (the approval gate would park the plan waiting for a signal
nothing here sends). `run_scenario.sh`, seeing the `:cp:` fixtures, waits for
every checkpoint turn to finish (not just `turn:1`) before running `expect.sh`.

In `run_all.sh`:

- **`plan-lifecycle`** — the happy path. A Deliberate debugging task →
  `propose_plan` (3 checkpoints, no approval) → 3 checkpoint turns each
  `checkpoint_done` → PLAN.md `complete` → `RecordSkill` fires once
  (`close_reason=plan_complete`). Also checks `SkillDiscover` staged
  `kind='skill'` rows under the plan_id (the planning-turn feed).
- **`plan-checkpoint-revise`** — `checkpoint_done`'s `revised_tail`. cp2 replaces
  the still-pending tail with two new steps; the final PLAN.md keeps cp1/cp2
  (and cp2's note) untouched and carries the *revised* cp3/cp4, all done — not
  the originals, not 6 checkpoints.
- **`subagent-spawn`**, **`spawn-subagent-nested-valid`**,
  **`spawn-subagent-nested-rejected`**, **`subagent-full-agent`** — all four are
  now plan scenarios: the top-level message is clearly Deliberate, so the spawn
  happens inside the plan's **one checkpoint turn** (`<plan>:cp:1`), not a
  planning turn (which is one-shot and would never dispatch it). The subagent
  turn_id is therefore `<plan>:cp:1:sub:1`, a nested spawn's grandchild
  `<plan>:cp:1:sub:1:sub:1`. `subagent-spawn` is the minimal spawn-plumbing
  case (the regression the `caller_is_subagent` `NameError` once broke);
  `spawn-subagent-nested-*` are the recursion-termination guard (below);
  `subagent-full-agent` additionally asserts the spawned subagent, being
  Deliberate, opens its **own** single-turn task-run (`turns.plan_id == its
  turn_id`, `openedFresh`), runs its own `RoutingWorkflow`, and gets a
  `dispatchRecordSkill` at turn end — no *nested* planning/checkpoint turns
  (a Deliberate subagent is one reason-act loop, not a `PlanWorkflow`).
- **`lite-simple-task`** — the Lite lane: a simple task → plain `TurnWorkflow`,
  `turns.plan_id` NULL, memory-only retrieval, no PLAN.md, no `RecordSkill`.
  Its `expect.sh` reads the classify log line and **skips** (not fails) if the
  classifier rated the turn Deliberate.

Run manually (a chained pair — timing-sensitive: the initial's cp2 does three
sequential `slow_tool` calls (~15s of blocking) and the follow-up must be
submitted while cp2 is still running):

```
KEY="test:scenario:plan-continuation:$(date +%s)"
workflows/scenarios/run_scenario.sh plan-continuation-initial "$KEY" &   # backgrounds
sleep 6                                                                  # planning turn + cp1
workflows/scenarios/run_scenario.sh plan-continuation-followup "$KEY"
```

- **`plan-continuation-{initial,followup}`** — `foldInFollowups`. While a plan
  runs the coordinator forwards *any* new message straight to the `PlanWorkflow`
  (it's `workActive`), which folds it in at the next checkpoint boundary as a
  `PlanHandling` turn `<plan_id>:followup:1`. `followup.json` sets
  `plan_followup: true` so the starter writes its fixture there. Asserts: no
  `turn:2`; the follow-up ran under the same plan; the plan still completed;
  `RecordSkill` fired once over a trajectory that includes the follow-up turn.
  (`ResolveOpenPlan`'s attach/supersede branch only fires across a coordinator
  restart mid-plan — a crash-recovery path, not scripted here.)
- **`real-llm-pipeline`** — spends real money. End-to-end plan-and-execute with
  a **real** provider: real ClassifyRequest → PlanWorkflow → real planning turn
  (`propose_plan`) → real checkpoint turns. `run_scenario.sh real-llm-pipeline`.

`superpowers-b/` remains the broader live eval (a real teaching conversation,
no scripting).

## Coverage added 2026-08-29 (rewritten 2026-09-03 for plan-and-execute)

`spawn-subagent-nested-valid.json` / `spawn-subagent-nested-rejected.json`
— the recursion-termination guard (`components/temporal-workflow.md`,
"Resolved: Recursion Termination Guard"): a subagent delegating to a
further subagent with genuine `delegated_scope`/`kept_work` succeeds
end-to-end (`<plan>:cp:1` → `:sub:1` → `:sub:1:sub:1`, all real child
workflows); one without them is rejected at mint time (no child workflow
ever starts, durably recorded as a real tool_calls error) and — the real
bug this suite caught while being built — the subagent correctly loops back
for a follow-up step to react to the rejection rather than silently ending
its turn (the `has_tool_calls` fix, see the same Notes Log entry). The spawn
originates from the plan's one checkpoint turn — a top-level Deliberate
message is a `PlanWorkflow`, and its planning turn is one-shot.

`lcm-retrieval.json` (+ `.setup.sql`) — `lcm_grep`/`lcm_describe`/
`lcm_expand` (`components/context-slot.md`'s Memory-Access Tools) against a
pre-seeded, already-folded two-level summary DAG, exercising the real
`TOOL_REGISTRY`-dispatched handler code path end to end, including the
`folded_into` chain resolution.

`subagent-spawn.expect.sh` — the minimal spawn-plumbing case; this is
exactly what the `caller_is_subagent` `NameError` (found and fixed the same
day, alongside the guard above) would have caught immediately instead of
silently shipping to a live deploy. Now a plan scenario (see the
plan-and-execute section) — the spawn is in the one checkpoint turn.
