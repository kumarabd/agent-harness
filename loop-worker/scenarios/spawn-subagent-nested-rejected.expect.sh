#!/usr/bin/env bash
# Expectations for spawn-subagent-nested-rejected.json — the recursion-
# termination guard's rejection path, on the flat reason-act loop. A subagent
# that tries to delegate everything (no delegated_scope/kept_work) must be
# rejected at mint time: no child workflow ever starts, the rejection is
# durably recorded as a tool_calls error, and — the has_tool_calls fix — the
# subagent's turn must NOT silently end there; it loops back for a follow-up
# reasoning step so the model sees and reacts to the rejection.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"
SUB_TURN_ID="${ROOT_TURN_ID}:sub:1"
REJECTED_GRANDCHILD_TURN_ID="${SUB_TURN_ID}:sub:1"

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
[ "$sub_status" = "completed" ] || fail "subagent turn ($SUB_TURN_ID) status = '$sub_status', expected 'completed' — a rejected nested spawn must not crash or stall the subagent's own turn"
ok "subagent turn completed despite the rejected nested delegation"

# The would-be grandchild must never have been dispatched — no turns row.
grandchild_count="$(pg "SELECT count(*) FROM turns WHERE turn_id = '$REJECTED_GRANDCHILD_TURN_ID'")"
[ "$grandchild_count" = "0" ] || fail "a turns row exists for the rejected grandchild ($REJECTED_GRANDCHILD_TURN_ID) — it should never have been dispatched"
ok "no child workflow was ever started for the rejected delegation (0 turns rows)"

# The subagent's own spawn_subagent row shows the rejection: is_subagent=false,
# status='error'. `||` casts the boolean to "false"/"true" (not psql's "f"/"t").
reject_row="$(pg "SELECT tool_call_id || '|' || is_subagent || '|' || status FROM tool_calls WHERE parent_id = '$SUB_TURN_ID' AND tool_name = 'spawn_subagent'")"
echo "$reject_row" | grep -qE '\|false\|error$' || fail "rejected spawn_subagent row not is_subagent=false/status=error: '$reject_row'"
ok "rejected spawn_subagent recorded as is_subagent=false, status=error ($reject_row)"

result_text="$(pg "SELECT COALESCE(result::text,'') FROM tool_calls WHERE parent_id = '$SUB_TURN_ID' AND tool_name = 'spawn_subagent'")"
echo "$result_text" | grep -qi "delegated_scope" || fail "rejection result doesn't mention delegated_scope/kept_work: '$result_text'"
ok "rejection result names the missing delegated_scope/kept_work requirement"

# has_tool_calls fix: the subagent looped back for a REAL follow-up ModelCall
# after the rejection — its final assistant message is the SECOND scripted
# response, not the first.
last_message="$(pg "SELECT content FROM messages WHERE parent_id = '$SUB_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
echo "$last_message" | grep -qi "performing the task directly" \
  || fail "subagent's last message doesn't show it reacted to the rejection: '$last_message'"
ok "subagent looped back after the rejection and produced its follow-up response (has_tool_calls fix)"

exit 0
