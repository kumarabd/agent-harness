#!/usr/bin/env bash
# Expectations for subagent-spawn.json — basic subagent-spawn plumbing on the
# flat reason-act loop (no planning workflow).
#
# The regression this guards: the caller_is_subagent NameError once crashed
# ModelCall for ANY fixture that spawned a subagent.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"
SUB_TURN_ID="${ROOT_TURN_ID}:sub:1"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

# --- the root turn ran the reason-act loop and completed ---
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] \
  || fail "root turn ($ROOT_TURN_ID) not completed"
ok "root turn completed"

# --- the turn dispatched a real subagent child workflow ---
sub_status="$(pg "SELECT status FROM turns WHERE turn_id = '$SUB_TURN_ID'")"
[ "$sub_status" = "completed" ] || fail "subagent turn ($SUB_TURN_ID) status = '$sub_status', expected 'completed'"
ok "subagent turn completed"

sub_row="$(pg "SELECT is_subagent FROM tool_calls WHERE tool_call_id = '$SUB_TURN_ID' AND tool_name = 'spawn_subagent'")"
[ "$sub_row" = "t" ] || fail "spawn_subagent tool_calls row for $SUB_TURN_ID not found / not is_subagent=true ('$sub_row')"
ok "spawn_subagent minted as a real subagent dispatch (is_subagent=true, tool_call_id == subagent turn_id)"

# --- the subagent's result was folded back (real dispatch, not a placeholder) ---
sub_result="$(pg "SELECT COALESCE(result::text,'') FROM tool_calls WHERE tool_call_id = '$SUB_TURN_ID'")"
[ -n "$sub_result" ] || fail "subagent's tool_calls.result is empty — expected a real manifest/result after completion"
ok "subagent's tool_calls.result is populated"

exit 0
