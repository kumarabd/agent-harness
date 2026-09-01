# Suite B — Superpowers chain, LEARNED FROM NOTHING

The counterpart to Suite A. Same chain, but **no seeds** — the agent is handed a
process doc inline, follows it, and the skill subsystem generalizes that
trajectory into a stored `learned:*` procedure it then reuses on its own. This
is the purest test of "the agent learns a skill by doing it."

**All real-LLM. Run manually, after Suite A, in a fresh setup.**

## Fresh setup

Suite B must run against a skill store with **no `sp-*` seeds and no leftover
`learned:*` procedures**, so that anything learned is attributable to these
runs:

```sh
# 1. Remove the sp-* seed files so the next deploy doesn't reload them
mkdir -p /tmp/sp-seeds-parked && mv activities/activities/skills/seeds/sp-*.json /tmp/sp-seeds-parked/

# 2. Rebuild + redeploy the tenant-worker (seeds reconcile at startup — but
#    seed.py only ADDS/updates, it never deletes, so also:)

# 3. Drop the sp-* rows and any learned procedures from the live store
psql ... <<'SQL'
DELETE FROM skill_cooccurrence WHERE proc_a LIKE 'sp-%' OR proc_b LIKE 'sp-%' OR proc_a LIKE 'learned:%' OR proc_b LIKE 'learned:%';
DELETE FROM skill_procedures WHERE id LIKE 'sp-%' OR provenance = 'learned';
SQL

# verify: only the 4 base seeds remain
psql ... -c "SELECT id, provenance FROM skill_procedures WHERE valid_to IS NULL"
```

Base seeds stay (`commit-changes`, `investigate-failure`, `implement-change`,
`research-and-report`) — they're the intended quality floor and none of them is
a design/brainstorming procedure, so `brainstorming` genuinely has to be
learned.

## The flow

| # | File | Checks |
|---|---|---|
| b1 | `b1-follow-external` | The agent is handed the brainstorming process inline + a design task. It follows the process → `RecordSkillOutcome` writes a candidate → `SkillSynthesize` generalizes the trajectory into a `learned:*` procedure whose body reflects the brainstorming shape (clarify / approaches / design / approval / spec). Prints the new procedure's id. |
| b2 | `b2-learned-retrieval` | Run after b1. A **new** design task with **no** process doc. `SkillDiscover` now retrieves the `learned:*` procedure from b1 and `ComposeSkill` uses it — the agent reusing a skill it taught itself, unprompted. |
| b3 | *(documented below)* | Chain learning: teach `brainstorming`, then ask for the plan (no `writing-plans` doc). A second `learned:*` procedure is synthesized, and a `skill_cooccurrence` edge forms between the two learned procedures. |

```sh
export TEMPORAL_ADDRESS=localhost:17233   # kubectl port-forward -n core svc/temporal-frontend 17233:7233 &

bash ../run_scenario.sh superpowers-b/b1-follow-external   test:spB:1:$(date +%s)
sleep 90    # let SkillSynthesize drain
bash ../run_scenario.sh superpowers-b/b2-learned-retrieval test:spB:2:$(date +%s)
```

## b3 — chain learning (manual)

1. Pick a session key: `KEY=test:spB:chain:$(date +%s)`
2. `run_scenario.sh superpowers-b/b1-follow-external "$KEY"` — let it complete.
3. Send a second message to the **same session** — a plain
   `run_scenario.sh` with a tiny inline scenario, or via the chat page:
   > "The design's approved. Now write the implementation plan for it — I don't
   >  have a plan-writing process to give you, use your judgement."
4. `sleep 90` for synthesis.
5. Check:
   ```sql
   SELECT id, title, provenance, created_at FROM skill_procedures WHERE provenance='learned' AND valid_to IS NULL ORDER BY created_at;
   SELECT proc_a, proc_b, round(edge::numeric,3) FROM skill_cooccurrence WHERE proc_a LIKE 'learned:%' OR proc_b LIKE 'learned:%';
   ```
   Expect **two** learned procedures (a brainstorm-shaped one and a
   plan-writing-shaped one) and an **edge between them** — the co-occurrence
   graph forming under a chain the agent was never seeded with.

## What "success" looks like across A + B

- **A** proves: given good seeds, the system picks the right one per task
  phase, merges overlapping ones coherently, and the chain's steps co-occur.
- **B** proves: given nothing, the system can *acquire* a procedure from one
  good run and reuse it — and acquire a second, linked one from the next step.

Run A, then B on the same cluster (fresh store), and you've exercised the whole
skill lifecycle: seed → retrieve → compose → plan → record → synthesize →
re-retrieve → co-occur.
