#!/usr/bin/env bash
# done-with-empty-content.json — an empty message with status=done must end
# the turn cleanly in one step, not retry and not fail. Silence is a valid
# outcome; the harness never second-guesses it.
set -euo pipefail

ROOT_TURN_ID="$2"
pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] \
  || fail "expected turns.status='completed' — silence is not a failure"
ok "turn completed normally, not marked failed"

n_asst="$(pg "SELECT count(*) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant'")"
[ "${n_asst:-0}" = "1" ] || fail "expected exactly 1 assistant message (no forced retry on empty done), got $n_asst"
ok "no retry was forced — one step, done"

ans="$(pg "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' LIMIT 1")"
[ -z "$ans" ] || fail "expected the persisted message to be empty, got: '$ans'"
ok "the empty message was persisted as-is, not rewritten"

exit 0
