#!/usr/bin/env bash
# exploration-summary-json.json — a large shell_exec output is routed to the
# claim-check store with a type-aware exploration_summary (type='json').
set -euo pipefail
ROOT_TURN_ID="$2"
pg() { kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""; }
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] || fail "turn not completed"
ok "turn completed"
r="$(pg "SELECT result FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'shell_exec' ORDER BY tool_call_id LIMIT 1")"
echo "$r" | grep -q "exploration_summary" || fail "no exploration_summary in the first shell_exec result: $r"
echo "$r" | grep -q "claim_check_path" || fail "large output not routed to the claim-check store"
ok "large output carries a type-aware exploration_summary + claim_check_path"
exit 0
