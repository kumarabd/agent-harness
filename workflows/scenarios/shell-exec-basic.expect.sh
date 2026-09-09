#!/usr/bin/env bash
# shell-exec-basic.json — a gated shell_exec (`echo && pwd` needs approval per
# permissions.py). The runner's auto-approver signals it; the tool then runs
# for real and the turn completes. Checks the approval round-tripped and the
# command actually executed.
set -euo pipefail

ROOT_TURN_ID="$2"
pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] \
  || fail "root turn not completed — the approval may not have round-tripped"
ok "turn completed after approval"

uir="$(pg "SELECT kind || '|' || status FROM user_input_requests WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$uir" = "permission|answered" ] || fail "user_input_requests row is '$uir', expected 'permission|answered'"
ok "a permission request was raised and answered (approve)"

tc="$(pg "SELECT tool_name || '|' || status FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'shell_exec'")"
[ "$tc" = "shell_exec|ok" ] || fail "shell_exec tool_call is '$tc', expected 'shell_exec|ok'"
ok "the gated shell_exec ran to completion after approval"

res="$(pg "SELECT result FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'shell_exec'")"
echo "$res" | grep -qi "hello" || fail "shell_exec result has no 'hello' — the command did not really run: '$res'"
ok "the command actually executed (output contains 'hello')"

exit 0
