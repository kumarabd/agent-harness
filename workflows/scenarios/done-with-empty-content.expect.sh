#!/usr/bin/env bash
# done-with-empty-content.json — a "done" step with no message content must
# not end the turn with nothing delivered. model_call.py coerces the
# contradictory status to "working" so the no-progress guard gives the model
# one real retry; the turn only completes once a real message exists.
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
[ "${n_asst:-0}" = "2" ] || fail "expected exactly 2 assistant messages (the loop must retry past the empty 'done' step), got $n_asst"
ok "loop retried past the empty 'done' step instead of ending on it"

ans="$(pg "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
[ -n "$ans" ] || fail "final message is empty — nothing was delivered to the user"
echo "$ans" | grep -qi "done" || fail "final message is not the retry's real content: '$ans'"
ok "a real, non-empty message reached the user"

exit 0
