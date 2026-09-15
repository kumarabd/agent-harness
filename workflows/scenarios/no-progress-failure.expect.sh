#!/usr/bin/env bash
# no-progress-failure.json — two consecutive empty/no-tool-call steps must
# fail the turn loudly (turns.status='failed' + a visible failTurn notice),
# never complete silently with nothing delivered.
set -euo pipefail

ROOT_TURN_ID="$2"
pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "failed" ] \
  || fail "expected turns.status='failed' — a stuck model must not be reported as a normal completion"
ok "turn is honestly marked failed, not completed"

ans="$(pg "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
[ -n "$ans" ] || fail "no visible notice reached the user — silent failure"
echo "$ans" | grep -qi "something went wrong" || fail "final message is not the failure notice: '$ans'"
ok "a visible failure notice reached the user instead of silence"

exit 0
