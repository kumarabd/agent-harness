#!/usr/bin/env bash
# shell-exec-slow.json — one gated `sleep 30` shell_exec. The auto-approver
# approves it; it runs to completion (heartbeats keep the activity alive).
set -euo pipefail
ROOT_TURN_ID="$2"
pg() { kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""; }
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] || fail "turn not completed"
tc="$(pg "SELECT status FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'shell_exec'")"
[ "$tc" = "ok" ] || fail "shell_exec status = '$tc', expected 'ok' (a long heartbeating tool ran to completion)"
ok "long-running shell_exec completed with heartbeats keeping it alive"
exit 0
