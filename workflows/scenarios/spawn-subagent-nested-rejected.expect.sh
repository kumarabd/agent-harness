#!/usr/bin/env bash
# Expectations for spawn-subagent-nested-rejected.json — a subagent that
# tries to delegate everything (no delegated_scope/kept_work) must be
# rejected at mint time: no child workflow ever starts, the rejection is
# durably recorded as a real tool_calls error, and — the bug found and
# fixed 2026-08-29 while building this suite — the subagent's turn must NOT
# silently end right there; it has to loop back for a follow-up reasoning
# step so the model can actually see and react to the rejection message.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"
SUBAGENT_TURN_ID="${ROOT_TURN_ID}:sub:1"
REJECTED_GRANDCHILD_TURN_ID="${SUBAGENT_TURN_ID}:sub:1"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}

fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

root_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$root_status" = "completed" ] || fail "root turn status = '$root_status', expected 'completed'"
ok "root turn completed"

sub_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$SUBAGENT_TURN_ID'")"
[ "$sub_status" = "completed" ] || fail "subagent turn ($SUBAGENT_TURN_ID) status = '$sub_status', expected 'completed' — a rejected nested spawn must not have crashed or stalled the subagent's own turn"
ok "subagent turn completed despite the rejected nested delegation"

# The would-be grandchild must never have been dispatched at all — no turns
# row, no child workflow ever started for it.
grandchild_count="$(pg_query "SELECT count(*) FROM turns WHERE turn_id = '$REJECTED_GRANDCHILD_TURN_ID'")"
[ "$grandchild_count" = "0" ] || fail "a turns row exists for the rejected grandchild ($REJECTED_GRANDCHILD_TURN_ID) — it should never have been dispatched"
ok "no child workflow was ever started for the rejected delegation (0 turns rows)"

# The subagent's own spawn_subagent tool_calls row must show the rejection:
# is_subagent=false (never became a real subagent), status='error', and a
# real error result containing the rejection reason.
reject_row="$(pg_query "SELECT tool_call_id || '|' || is_subagent || '|' || status FROM tool_calls WHERE parent_id = '$SUBAGENT_TURN_ID' AND tool_name = 'spawn_subagent'")"
echo "$reject_row" | grep -qE '\|f\|error$' || fail "rejected spawn_subagent tool_calls row not is_subagent=false/status=error: '$reject_row'"
ok "rejected spawn_subagent recorded as is_subagent=false, status=error ($reject_row)"

result_text="$(pg_query "SELECT result::text FROM tool_calls WHERE parent_id = '$SUBAGENT_TURN_ID' AND tool_name = 'spawn_subagent'")"
echo "$result_text" | grep -qi "delegated_scope" || fail "rejection result doesn't mention delegated_scope/kept_work: '$result_text'"
ok "rejection result names the missing delegated_scope/kept_work requirement"

# The has_tool_calls fix: the subagent must have looped back for a REAL
# follow-up ModelCall after the rejection, not silently ended its turn on
# the same step. Confirmed by the subagent's own final assistant message
# being the SECOND scripted response's content, not the first.
last_message="$(pg_query "SELECT content FROM messages WHERE parent_id = '$SUBAGENT_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
echo "$last_message" | grep -qi "performing the task directly" || fail "subagent's last message doesn't show it reacted to the rejection: '$last_message' (this is exactly the has_tool_calls bug this suite was built to catch — a rejected call with no other tool calls used to silently end the turn instead of looping back)"
ok "subagent looped back after the rejection and produced its follow-up response (has_tool_calls fix confirmed live)"

exit 0
