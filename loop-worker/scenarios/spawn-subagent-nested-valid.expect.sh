#!/usr/bin/env bash
# Expectations for spawn-subagent-nested-valid.json — the recursion-
# termination guard's happy path, on the flat reason-act loop. A subagent
# that delegates further WITH genuine delegated_scope/kept_work must succeed
# at every level: turn:1 -> subagent -> grandchild, all real child workflows,
# all completed.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"
SUB_TURN_ID="${ROOT_TURN_ID}:sub:1"
GRANDCHILD_TURN_ID="${SUB_TURN_ID}:sub:1"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] \
  || fail "root turn ($ROOT_TURN_ID) not completed"
ok "root turn completed"

sub_status="$(pg "SELECT status FROM turns WHERE turn_id = '$SUB_TURN_ID'")"
[ "$sub_status" = "completed" ] || fail "first-level subagent turn ($SUB_TURN_ID) status = '$sub_status', expected 'completed'"
ok "first-level subagent turn completed"

grandchild_status="$(pg "SELECT status FROM turns WHERE turn_id = '$GRANDCHILD_TURN_ID'")"
[ "$grandchild_status" = "completed" ] || fail "grandchild turn ($GRANDCHILD_TURN_ID) status = '$grandchild_status', expected 'completed' — nested delegation with genuine delegated_scope/kept_work should be dispatched as a real child workflow, not rejected"
ok "grandchild (nested subagent) turn completed — the guard correctly allowed genuine delegation"

# The subagent's own spawn_subagent call must have been minted as a real
# subagent dispatch (is_subagent=true, tool_call_id == the grandchild turn_id).
# Postgres's `||` casts the boolean operand to "true"/"false", not "t"/"f".
spawn_row="$(pg "SELECT tool_call_id || '|' || is_subagent FROM tool_calls WHERE parent_id = '$SUB_TURN_ID' AND tool_name = 'spawn_subagent'")"
echo "$spawn_row" | grep -q "^${GRANDCHILD_TURN_ID}|true$" \
  || fail "subagent's nested spawn_subagent row not a real subagent dispatch: '$spawn_row'"
ok "subagent's nested spawn_subagent minted as a real subagent dispatch ($spawn_row)"

grandchild_result="$(pg "SELECT COALESCE(result::text,'') FROM tool_calls WHERE tool_call_id = '$GRANDCHILD_TURN_ID'")"
[ -n "$grandchild_result" ] || fail "grandchild's tool_calls.result is empty — expected a real manifest/result after completion"
ok "grandchild's tool_calls.result is populated (not empty)"

exit 0
