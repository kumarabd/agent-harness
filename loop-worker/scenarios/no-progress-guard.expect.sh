#!/usr/bin/env bash
# no-progress-guard.json — two consecutive empty "working" steps must end the
# turn (stopReason='no_progress') instead of running to the ceiling. The
# fixture scripts 5 such steps; the turn must stop after exactly 2.
set -euo pipefail

ROOT_TURN_ID="$2"
pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] \
  || fail "root turn not completed"
ok "turn completed"

n_asst="$(pg "SELECT count(*) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant'")"
[ "${n_asst:-0}" = "2" ] || fail "expected the guard to stop at 2 iterations, got $n_asst assistant messages (ceiling is 20)"
ok "no-progress guard stopped the turn at iteration 2, not the ceiling"

exit 0
