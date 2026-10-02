#!/usr/bin/env bash
# ceiling-raise.json — the model's report_status est_remaining_steps raises the
# iteration ceiling above the base 20. On step 20 est=4 → ceiling = 20+4*2 = 28.
# The fixture would keep going forever; the turn must stop at exactly 28.
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
[ "${n_asst:-0}" -gt 20 ] || fail "turn stopped at $n_asst iterations — the ceiling was NOT raised past the base 20"
[ "${n_asst:-0}" = "28" ] || fail "expected exactly 28 iterations (20 + est*2), got $n_asst"
ok "ceiling raised to 28 by est_remaining_steps=4 on step 20 ($n_asst iterations)"

exit 0
