#!/usr/bin/env bash
# max-iterations.json — 25 scripted tool-calling steps against the base ceiling
# of 20 (no report_status raise). The turn must stop at exactly 20 iterations.
set -euo pipefail
ROOT_TURN_ID="$2"
pg() { kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""; }
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] || fail "turn not completed"
ok "turn completed"
n="$(pg "SELECT count(*) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role='assistant'")"
[ "${n:-0}" = "20" ] || fail "expected the base ceiling to stop the turn at 20 iterations, got $n"
ok "iteration ceiling held at the base 20"
exit 0
