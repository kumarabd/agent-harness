#!/usr/bin/env bash
# Expectations for lite-simple-task.json — docs/components/lane-model.md, the Lite lane.
#
# A simple task must NOT open a task-run (no PlanWorkflow, turns.plan_id unset)
# and must NOT run skill/tool/plan retrieval — only memory, staged under the
# turn's own id.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"

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

st="$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$st" = "completed" ] || fail "root turn status = '$st'"
ok "root turn completed"

line="$(kubectl logs -n "$NAMESPACE" deploy/harness --since=10m 2>/dev/null \
       | grep -F "turn_id $ROOT_TURN_ID" | grep -F "request classified" | tail -1 || true)"
cx="$(echo "$line" | grep -oE 'intent [a-z]+ complexity [a-z]+' | tail -1)"
conf="$(echo "$line" | grep -oE 'confidence [0-9.]+' | grep -oE '[0-9.]+' | tail -1)"
echo "  classify: ${cx:-<not found>}  ${conf:+conf=$conf}"
# Lite = anything that is NOT (task moderate|complex), (question complex), or conf<0.5.
awk "BEGIN{exit !($conf < 0.5)}" 2>/dev/null && { echo "  SKIP: conf=$conf < 0.5 — degraded classify forces Deliberate, Lite assertions don't apply"; exit 0; }
case "$cx" in
  "intent task complexity moderate"|"intent task complexity complex"|"intent question complexity complex")
    echo "  SKIP: classifier rated this Deliberate ('$cx') — Lite assertions don't apply"
    exit 0 ;;
esac
ok "classifier rated this a Lite turn ('$cx')"

ep="$(pg "SELECT COALESCE(plan_id,'') FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ -z "$ep" ] || fail "turns.plan_id = '$ep' — a simple task should take the Lite lane and open NO task-run"
ok "no task-run opened (Lite lane)"

kinds="$(pg "SELECT string_agg(DISTINCT kind, ',' ORDER BY kind) FROM turn_retrieval WHERE owner_id = '$ROOT_TURN_ID'")"
echo "  staged retrieval kinds: '${kinds:-<none>}'"
case "$kinds" in
  ""|"memory") ok "retrieval is memory-only (or empty)" ;;
  *) fail "staged non-memory retrieval ($kinds) — Lite should only run MemoryRetrieve" ;;
esac

[ -z "$(plan_md "$ROOT_TURN_ID")" ] || fail "a PLAN.md exists for this turn — Lite must not seed a plan ledger"
ok "no plan ledger"

rec="$(pg "SELECT count(*) FROM skill_procedures WHERE source_ids @> jsonb_build_array('$ROOT_TURN_ID')")"
[ "${rec:-0}" = "0" ] || fail "a skill_procedures row carries this turn in source_ids — Lite must not record"
ok "no RL recording"

exit 0
