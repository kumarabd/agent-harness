#!/usr/bin/env bash
# Expectations for lite-simple-task.json — turn-pipeline.md (post-cutover).
#
# There are no lanes any more. This is just a plain single-step turn: the model
# answers in one reasoning step with no tool calls, and nothing runs before the
# loop (the pre-LLM retrieval fan-out and the skill subsystem are gone).
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

st="$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$st" = "completed" ] || fail "root turn status = '$st'"
ok "root turn completed"

ans="$(pg "SELECT string_agg(content, ' ') FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant'")"
[ -n "$ans" ] || fail "no assistant message on the turn"
ok "turn produced a response"

# --- no ClassifyRequest / RoutingWorkflow ran (Phase 8) ---
if command -v temporal >/dev/null 2>&1; then
  r="$(TEMPORAL_ADDRESS=localhost:17233 temporal workflow describe \
      --namespace abishekk --workflow-id "${ROOT_TURN_ID}:routing" -o json 2>/dev/null || true)"
  [ -z "$r" ] || fail "a ${ROOT_TURN_ID}:routing workflow exists — RoutingWorkflow should be gone"
  ok "no RoutingWorkflow child"
fi

n_staged="$(pg "SELECT count(*) FROM turn_retrieval WHERE owner_id = '$ROOT_TURN_ID'")"
[ "${n_staged:-0}" = "0" ] || fail "$n_staged turn_retrieval rows staged — nothing runs pre-LLM any more"
ok "nothing staged to turn_retrieval before the loop"

n_tools="$(pg "SELECT count(*) FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID'")"
[ "${n_tools:-0}" = "0" ] || fail "$n_tools tool calls on a plain answer turn"
ok "plain answer turn — no tool calls"

exit 0
