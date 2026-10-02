#!/usr/bin/env bash
# happy-path.json — two parallel search calls, then a summary. Baseline
# multi-tool plumbing: both tool calls dispatch, settle ok, and the turn
# produces a final answer.
set -euo pipefail
ROOT_TURN_ID="$2"
pg() { kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""; }
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] || fail "turn not completed"
ok "turn completed"
n_ok="$(pg "SELECT count(*) FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'search' AND status = 'ok'")"
[ "${n_ok:-0}" = "2" ] || fail "expected 2 successful search tool calls, got $n_ok"
ok "both parallel search calls dispatched and settled ok"
ans="$(pg "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role='assistant' ORDER BY seq DESC LIMIT 1")"
echo "$ans" | grep -qi "summary" || fail "no final summary message: '$ans'"
ok "turn produced a final answer"
exit 0
