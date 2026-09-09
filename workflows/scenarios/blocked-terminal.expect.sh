#!/usr/bin/env bash
# blocked-terminal.json — the status:"blocked" + nothing-to-wait-on branch.
# turn.go must deliver the model's message and END (stopReason='blocked'),
# not loop. One iteration, one assistant message, and it is the blocked text.
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
ok "turn completed (did not hang on a 'blocked' status)"

n_asst="$(pg "SELECT count(*) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant'")"
[ "${n_asst:-0}" = "1" ] || fail "expected exactly 1 assistant message (one iteration), got $n_asst"
ok "exactly one iteration — the loop did not continue past a blocked step"

n_tc="$(pg "SELECT count(*) FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID'")"
[ "${n_tc:-0}" = "0" ] || fail "expected no tool calls, got $n_tc"
ok "no tool calls dispatched"

ans="$(pg "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' LIMIT 1")"
echo "$ans" | grep -qi "which thing" || fail "final message is not the blocked text: '$ans'"
ok "the blocked message reached the user"

exit 0
