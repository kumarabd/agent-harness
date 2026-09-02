#!/usr/bin/env bash
# Expectations for lite-simple-task.json — docs/components/lane-model.md, the Lite lane.
#
# A simple task must NOT open an episode and must NOT run skill/tool/plan
# retrieval — only memory, staged under the turn's own id.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

ROOT_TURN_ID="$2"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
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

ep="$(pg "SELECT COALESCE(episode_id,'') FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ -z "$ep" ] || fail "turns.episode_id = '$ep' — a simple task should take the Lite lane and open NO episode"
ok "no episode opened (Lite lane)"

n_ep="$(pg "SELECT count(*) FROM episodes WHERE episode_id = '$ROOT_TURN_ID'")"
[ "$n_ep" = "0" ] || fail "an episodes row exists for this turn"
ok "no episodes row"

kinds="$(pg "SELECT string_agg(DISTINCT kind, ',' ORDER BY kind) FROM turn_retrieval WHERE episode_id = '$ROOT_TURN_ID'")"
echo "  staged retrieval kinds: '${kinds:-<none>}'"
case "$kinds" in
  ""|"memory") ok "retrieval is memory-only (or empty)" ;;
  *) fail "staged non-memory retrieval ($kinds) — Lite should only run MemoryRetrieve" ;;
esac

plan_n="$(pg "SELECT count(*) FROM turn_plan WHERE episode_id = '$ROOT_TURN_ID'")"
[ "${plan_n:-0}" = "0" ] || fail "turn_plan has $plan_n rows — Lite must not seed a plan ledger"
ok "no plan ledger"

cand="$(pg "SELECT count(*) FROM skill_candidates WHERE turn_id = '$ROOT_TURN_ID'")"
[ "${cand:-0}" = "0" ] || fail "a skill_candidates row was written — Lite must not record"
ok "no RL recording"

exit 0
