#!/usr/bin/env bash
# claim-check-large-output.json — a shell_exec producing ~30KB stdout must be
# routed to the claim-check store (result carries claim_check_path, not inline
# text), and the model can read the tail back with a normal shell command.
set -euo pipefail
ROOT_TURN_ID="$2"
pg() { kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""; }
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] || fail "turn not completed"
ok "turn completed"
first="$(pg "SELECT result FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'shell_exec' ORDER BY tool_call_id LIMIT 1")"
echo "$first" | grep -q "claim_check_path" || fail "first shell_exec result has no claim_check_path — large output was not routed to the store: '$first'"
ok "large stdout routed to the claim-check store (claim_check_path present)"
exit 0
