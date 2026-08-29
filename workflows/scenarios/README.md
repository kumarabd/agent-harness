# Scenarios — Regression Suite

A growing suite of scripted, zero-cost Temporal scenarios, run against the
**already-deployed live cluster workers** (no local worker binaries, no
scaling anything down, no real LLM API spend) — the thing to run after any
change touching `ModelCall`, `turn.go`'s dispatch loop, tool_calls minting,
or the `lcm/` package, instead of re-deriving verification from scratch
every time.

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
