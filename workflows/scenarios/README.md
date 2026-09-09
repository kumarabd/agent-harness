# Scenarios — Regression Suite

A growing suite of scripted Temporal scenarios, run against the
**already-deployed live cluster workers** (no local worker binaries, no
scaling anything down) — the thing to run after any change touching
`ModelCall`, `turn.go`'s dispatch loop, tool_calls minting, or the `lcm/`
package, instead of re-deriving verification from scratch every time.

> **Mostly zero-cost.** The scripted-fixture path replaces the reason-act
> loop's model calls, and there is no pre-LLM pipeline any more (classify /
> lane / routing / plan / skill retrieval were all removed) — a fixture turn
> is pure plumbing. The one exception is **`real-assembly`**, which
> deliberately omits fixtures so its one ModelCall runs `prompt.assemble` →
> `lcm.assemble` for real against a seeded multi-turn history — the assembly
> path every real turn takes. One real fast-tier call.
>
> `resolved-tool-dispatch` stays fully fixture-scripted — a scripted
> response's `tool_calls` still dispatch for real, so it scripts a
> `discover_tools` call and checks the real handler's `_persist_discovered`
> wrote what it found into `turn_retrieval` under the turn's own id, the
> mechanism that lets a mid-turn discovery be called by name on the next
> step.

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
- **Chained pairs** — `interrupt-*`, `subagent-merge-cancelled-*`, and
  `ask-user-*`. Each is two scripted runs against the *same* still-running
  session — run manually, two calls back to back against one explicit session
  key. `ask-user-*` verifies `ask_user` park/resume: the initial run parks the
  turn on an `ask_user` call, the follow-up run sends a message that pre-empts
  the wait (the `ask_user` `tool_calls` row → `cancelled`, the message folded in
  as the answer). See `ask-user-followup.expect.sh` for the exact invocation.
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

## Subagents

Every turn — top-level or subagent — is one flat `TurnWorkflow` running the
same reason-act loop. There is no classify / lane / routing / plan / skill
machinery any more (all removed over the turn-pipeline redesign — see
`docs/components/turn-pipeline.md`).

In `run_all.sh`:

- **`subagent-spawn`**, **`spawn-subagent-nested-valid`**,
  **`spawn-subagent-nested-rejected`**, **`subagent-full-agent`** — the
  `spawn_subagent` call sits directly in `turn:1`'s scripted responses. The
  subagent turn_id is `<turn:1>:sub:1`, a nested spawn's grandchild
  `<turn:1>:sub:1:sub:1`. `subagent-spawn` is the minimal spawn-plumbing case;
  `spawn-subagent-nested-*` are the recursion-termination guard;
  `subagent-full-agent` asserts a spawned subagent runs its own multi-step
  loop with its own `tool_calls`, and that **no** `:routing` / `:record-skill`
  child is ever started for it.
- **`lite-simple-task`** — a plain one-step answer turn: no tool calls,
  nothing staged to `turn_retrieval`, no children.

## Coverage notes

`spawn-subagent-nested-valid.json` / `spawn-subagent-nested-rejected.json`
— the recursion-termination guard (`components/temporal-workflow.md`,
"Resolved: Recursion Termination Guard"): a subagent delegating to a
further subagent with genuine `delegated_scope`/`kept_work` succeeds
end-to-end (`<turn:1>` → `:sub:1` → `:sub:1:sub:1`, all real child
workflows); one without them is rejected at mint time (no child workflow
ever starts, durably recorded as a real tool_calls error) and — the real
bug this suite caught while being built — the subagent correctly loops back
for a follow-up step to react to the rejection rather than silently ending
its turn (the `has_tool_calls` fix).

`lcm-retrieval.json` (+ `.setup.sql`) — `lcm_grep`/`lcm_describe`/
`lcm_expand` (`components/context-slot.md`'s Memory-Access Tools) against a
pre-seeded, already-folded two-level summary DAG, exercising the real
`TOOL_REGISTRY`-dispatched handler code path end to end, including the
`folded_into` chain resolution.

`subagent-spawn.expect.sh` — the minimal spawn-plumbing case; this is
exactly what the `caller_is_subagent` `NameError` would have caught
immediately instead of silently shipping to a live deploy.
