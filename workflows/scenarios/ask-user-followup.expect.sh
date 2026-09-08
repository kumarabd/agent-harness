#!/usr/bin/env bash
# Expectations for the ask-user-* pair (docs/components/turn-pipeline.md, Phase 5).
# Run manually:
#   KEY="test:scenario:ask-user:$(date +%s)"
#   workflows/scenarios/run_scenario.sh ask-user-initial "$KEY" &   # parks on ask_user
#   sleep 5
#   workflows/scenarios/run_scenario.sh ask-user-followup "$KEY"
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] \
  || fail "root turn not completed — the parked ask_user turn never resumed"
ok "root turn resumed and completed after the follow-up message"

# --- the ask_user tool call was minted and then cancelled by the message ---
ask_row="$(pg "SELECT tool_name || '|' || status FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'ask_user'")"
[ "$ask_row" = "ask_user|cancelled" ] || fail "ask_user tool_calls row is '$ask_row', expected 'ask_user|cancelled'"
ok "ask_user call was minted and cancelled when the follow-up arrived"

# --- a user_input_requests row was created with the model's question ---
uir="$(pg "SELECT kind || '|' || (prompt LIKE '%30 days%')::text FROM user_input_requests WHERE request_id IN (SELECT tool_call_id FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'ask_user')")"
[ "$uir" = "question|true" ] || fail "user_input_requests row is '$uir', expected 'question|true' (question read from tool_calls.arguments)"
ok "the question was persisted from tool_calls.arguments (kind=question)"

# --- the follow-up message landed in the conversation ---
n_user="$(pg "SELECT count(*) FROM messages m JOIN turns t ON m.parent_id = t.turn_id WHERE t.turn_id = '$ROOT_TURN_ID' AND m.role = 'user' AND m.content ILIKE '%30 days%'")"
[ "${n_user:-0}" -ge 1 ] || fail "no user message mentioning '30 days' on the turn — the follow-up wasn't folded in"
ok "the follow-up answer was folded in as a user message"

# --- the model acted on the answer ---
last="$(pg "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
echo "$last" | grep -qi "30 days" || fail "final assistant message doesn't reflect the answer: '$last'"
ok "the turn's final response acted on the user's answer"

exit 0
