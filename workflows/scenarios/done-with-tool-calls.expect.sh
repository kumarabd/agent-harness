#!/usr/bin/env bash
# done-with-tool-calls.json — a "done" step that also carries a real tool
# call must not drop that call. model_call.py coerces the contradictory
# status to "working" so turn.go dispatches it, observes its result on a
# second iteration, and only then ends the turn — never leaving the
# tool_calls row stuck at 'pending'.
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
[ "${n_asst:-0}" = "2" ] || fail "expected exactly 2 assistant messages (the loop must continue past the first 'done' to dispatch the pending tool call), got $n_asst"
ok "loop continued past the first 'done' step instead of breaking on it"

n_tc="$(pg "SELECT count(*) FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID'")"
[ "${n_tc:-0}" = "1" ] || fail "expected exactly 1 tool call, got $n_tc"
ok "the tool call paired with status=done was minted"

tc_status="$(pg "SELECT status FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' LIMIT 1")"
[ "$tc_status" = "ok" ] || fail "expected the tool call to be dispatched and resolved ('ok'), got '$tc_status' — it was silently dropped"
ok "the tool call was actually dispatched, not silently dropped at 'pending'"

ans="$(pg "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
echo "$ans" | grep -qi "done" || fail "final message is not the second step's done text: '$ans'"
ok "the model's real final message reached the user"

exit 0
