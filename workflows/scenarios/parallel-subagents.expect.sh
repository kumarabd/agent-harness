#!/usr/bin/env bash
# parallel-subagents.json — two spawn_subagent calls in one response fan out as
# concurrent child TurnWorkflows. Both must complete, both must be minted as
# real subagent dispatches (:sub:1 and :sub:2), and the parent's final answer
# must reflect both results.
set -euo pipefail

ROOT_TURN_ID="$2"
SUB1="${ROOT_TURN_ID}:sub:1"
SUB2="${ROOT_TURN_ID}:sub:2"
pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] || fail "root turn not completed"
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$SUB1'")" = "completed" ] || fail "$SUB1 not completed"
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$SUB2'")" = "completed" ] || fail "$SUB2 not completed"
ok "root + both sibling subagent turns completed"

both="$(pg "SELECT string_agg(tool_call_id || '|' || is_subagent::text, ',' ORDER BY tool_call_id) FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'spawn_subagent'")"
[ "$both" = "${SUB1}|true,${SUB2}|true" ] || fail "spawn_subagent tool_calls are '$both', expected both sub:1 and sub:2 as is_subagent=true"
ok "both spawn_subagent calls minted as real subagent dispatches"

ans="$(pg "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
echo "$ans" | grep -qi "session" && echo "$ans" | grep -qi "idempotent" \
  || fail "final answer does not reflect BOTH subagent results: '$ans'"
ok "parent folded both subagent results into its final answer"

exit 0
