#!/usr/bin/env bash
# Expectations for lcm-retrieval.json — lcm_grep/lcm_describe/lcm_expand,
# scripted as real (non-subagent) tool calls, must produce real results
# from the DAG lcm-retrieval.setup.sql seeded: a message folded through
# leaf -> condensed, recovered end to end. Exercises the exact same
# ToolCall-activity/TOOL_REGISTRY dispatch path a real model would use.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

ROOT_TURN_ID="$2"
CONDENSED_ID="c0000000-1111-2222-3333-444444444444"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}

fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

root_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$root_status" = "completed" ] || fail "root turn status = '$root_status', expected 'completed'"
ok "root turn completed"

grep_result="$(pg_query "SELECT result::text FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'lcm_grep'")"
echo "$grep_result" | grep -qi "durian" || fail "lcm_grep result doesn't contain 'durian': '$grep_result'"
echo "$grep_result" | grep -q "$CONDENSED_ID" || fail "lcm_grep result doesn't resolve covered_by_summary_id to the condensed id: '$grep_result'"
ok "lcm_grep found the seeded message and resolved covered_by_summary_id through the folded_into chain to the condensed id"

describe_result="$(pg_query "SELECT result::text FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'lcm_describe'")"
echo "$describe_result" | grep -qi '"summary_kind"[: ]*"condensed"' || fail "lcm_describe didn't identify the id as a condensed summary: '$describe_result'"
ok "lcm_describe correctly identified the condensed summary"

expand_result="$(pg_query "SELECT result::text FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'lcm_expand'")"
echo "$expand_result" | grep -qi "durian" || fail "lcm_expand did not recover the original 'durian' message content: '$expand_result'"
echo "$expand_result" | grep -qi "noted, durian it is" || fail "lcm_expand did not recover BOTH original messages: '$expand_result'"
ok "lcm_expand recovered both original messages through condensed -> leaf -> messages"

final_message="$(pg_query "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
echo "$final_message" | grep -qi "durian" || fail "final assistant message doesn't reference the recovered fact: '$final_message'"
ok "final response references the recovered fact"

exit 0
