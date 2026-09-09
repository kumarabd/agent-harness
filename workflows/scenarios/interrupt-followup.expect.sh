#!/usr/bin/env bash
# Second half of the interrupt-* pair (run manually or via run_all.sh's
# chained-pair mode). interrupt-initial's turn is mid `shell_exec sleep 15` when this
# follow-up arrives: turn.go must cancel the in-flight tool, fold the message
# into the SAME turn, and let the model act on it.
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

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] \
  || fail "root turn not completed after the follow-up"
ok "the interrupted turn resumed and completed (one turn, not two)"

# the in-flight shell_exec was cancelled by the interrupt
tc="$(pg "SELECT tool_name || '|' || status FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'shell_exec'")"
[ "$tc" = "shell_exec|cancelled" ] || fail "shell_exec tool_call is '$tc', expected 'shell_exec|cancelled'"
ok "the in-flight tool call was cancelled by the follow-up"

# the follow-up landed in THIS turn as a user message
n_user="$(pg "SELECT count(*) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'user' AND content ILIKE '%never mind%'")"
[ "${n_user:-0}" -ge 1 ] || fail "the follow-up message was not folded into the turn"
ok "the follow-up was folded in as a user message on the same turn"

# the model acted on the follow-up
last="$(pg "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
echo "$last" | grep -qi "new request" || fail "final assistant message did not act on the follow-up: '$last'"
ok "the turn's final response acted on the follow-up"

exit 0
