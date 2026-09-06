#!/usr/bin/env bash
# Expectations for real-llm-pipeline.json — REAL provider calls, run manually.
#
# End-to-end plan-and-execute: real ClassifyRequest -> PlanWorkflow -> real
# planning turn -> PLAN.md -> real checkpoint turns. run_scenario.sh returns
# after the planning turn (no :cp: fixtures to detect); this script polls for
# the plan to finish itself.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
PLAN_ID="$2"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
plan_md() {
  kubectl exec -n "$NAMESPACE" deploy/abishekk-worker -- \
    cat "/sessions/session/$SESSION_KEY/plans/${1//:/_}/PLAN.md" 2>/dev/null || true
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$PLAN_ID'")" = "completed" ] \
  || fail "planning turn ($PLAN_ID) not completed — a real ModelCall through prompt.assemble errored, or the model never called propose_plan"
ok "real planning turn completed"

# SkillDiscover fed the planning turn
for kind in skill memory tool; do
  n="$(pg "SELECT count(*) FROM turn_retrieval WHERE owner_id IN ('$PLAN_ID') AND kind = '$kind'")"
  [ "${n:-0}" -ge 1 ] && ok "enrichment: kind='$kind' ($n rows)" \
    || echo "  NOTE: no kind='$kind' rows staged this run"
done

# PLAN.md seeded by the real propose_plan
PLAN="$(plan_md "$PLAN_ID")"
[ -n "$PLAN" ] || fail "no PLAN.md — the real planning turn did not call propose_plan"
n_cp="$(echo "$PLAN" | grep -cE '^- cp[0-9]+ ' || true)"
[ "${n_cp:-0}" -ge 1 ] || fail "PLAN.md has no checkpoints"
ok "real propose_plan seeded $n_cp checkpoints"

# wait for the plan to drive to completion (real checkpoint turns)
echo "  --- waiting for the plan to complete (real checkpoint calls) ---"
for _ in $(seq 1 300); do
  RUNNING="$(pg "SELECT count(*) FROM turns WHERE parent_id = '$PLAN_ID' AND parent_type = 'plan' AND status = 'running'")"
  DONE="$(plan_md "$PLAN_ID" | grep -cE '^status: complete' || true)"
  { [ "${RUNNING:-1}" = "0" ] && [ "${DONE:-0}" = "1" ]; } && break
  sleep 2
done
plan_md "$PLAN_ID" | grep -qE '^status: complete' || fail "plan never reached 'complete' — checkpoint turns stalled or failed"
n_done="$(plan_md "$PLAN_ID" | grep -cE '^- cp[0-9]+ \[[x-]\]' || true)"
ok "plan completed ($n_done/$n_cp checkpoints terminal)"

# RecordSkill fired
line="$(kubectl logs -n "$NAMESPACE" deploy/abishekk-worker --since=20m 2>/dev/null | grep -F "RecordSkill[$PLAN_ID]:" | tail -1 || true)"
[ -n "$line" ] && ok "RecordSkill fired: $line" || echo "  NOTE: no RecordSkill log line yet"

exit 0
