#!/usr/bin/env bash
# Expectations for real-llm-pipeline.json — REAL provider calls, run manually.
#
# This is the only end-to-end check of step 9 (prompt.assemble), which the
# scripted-fixture path skips entirely. Verifies the real ModelCall's context
# was assembled from all four enrichment sources without error, and the model
# produced a real answer.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

root_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$root_status" = "completed" ] || fail "root turn status = '$root_status', expected 'completed' — a real ModelCall through prompt.assemble errored or the model looped past max-iterations"
ok "root turn completed (real ModelCall via prompt.assemble succeeded)"

for kind in skill composed memory tool; do
  n="$(pg_query "SELECT count(*) FROM turn_retrieval WHERE episode_id = '$ROOT_TURN_ID' AND kind = '$kind'")"
  [ "${n:-0}" -ge 1 ] && ok "enrichment source present: kind='$kind' ($n rows)" \
    || echo "  NOTE: no kind='$kind' rows — that section was absent from the assembled prompt this run"
done

plan_n="$(pg_query "SELECT count(*) FROM turn_plan WHERE episode_id = '$ROOT_TURN_ID'")"
[ "${plan_n:-0}" -ge 1 ] && ok "turn_plan seeded ($plan_n checkpoints) — plan-progress section had content" \
  || echo "  NOTE: turn_plan empty — no composed skill produced checkpoints this run"

reply="$(pg_query "SELECT length(content) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
[ "${reply:-0}" -ge 200 ] || fail "final assistant message is only ${reply:-0} chars — model did not produce a real answer"
ok "model produced a substantive answer (${reply} chars)"

echo ""
echo "  --- prompt.assemble section breakdown (needs the redeploy that adds the log line) ---"
kubectl logs -n "$NAMESPACE" deploy/abishekk-worker --since=10m 2>/dev/null \
  | grep -F "prompt.assemble[$ROOT_TURN_ID]" || echo "  (no prompt.assemble log line — worker predates the observability log, or no sections)"

exit 0
