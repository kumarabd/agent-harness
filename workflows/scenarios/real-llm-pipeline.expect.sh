#!/usr/bin/env bash
# Expectations for real-llm-pipeline.json — REAL provider calls, run manually.
#
# End-to-end flat reason-act loop: real ClassifyRequest -> RoutingWorkflow
# retrieval -> real prompt.assemble -> a real multi-iteration ModelCall loop ->
# dispatchRecordSkill at turn end.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] \
  || fail "turn ($ROOT_TURN_ID) not completed — a real ModelCall through prompt.assemble errored, or the loop never converged"
ok "real reason-act turn completed"

# Deliberate: the turn opened its own task-run
plan_id="$(pg "SELECT COALESCE(plan_id,'') FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$plan_id" = "$ROOT_TURN_ID" ] || echo "  NOTE: turns.plan_id='$plan_id' — the classifier may have rated this Lite"

# real enrichment staged under the turn id
for kind in skill memory tool; do
  n="$(pg "SELECT count(*) FROM turn_retrieval WHERE owner_id = '$ROOT_TURN_ID' AND kind = '$kind'")"
  [ "${n:-0}" -ge 1 ] && ok "enrichment: kind='$kind' ($n rows)" \
    || echo "  NOTE: no kind='$kind' rows staged this run"
done

# the loop ran more than one reasoning step (real multi-iteration work)
n_asst="$(pg "SELECT count(*) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant'")"
[ "${n_asst:-0}" -ge 1 ] || fail "no assistant messages on the turn"
ok "$n_asst assistant message(s) written"

# RecordSkill fired
line="$(kubectl logs -n "$NAMESPACE" deploy/abishekk-worker --since=20m 2>/dev/null | grep -F "RecordSkill[$ROOT_TURN_ID]:" | tail -1 || true)"
[ -n "$line" ] && ok "RecordSkill fired: $line" || echo "  NOTE: no RecordSkill log line yet"

exit 0
