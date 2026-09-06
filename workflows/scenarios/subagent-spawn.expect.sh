#!/usr/bin/env bash
# Expectations for subagent-spawn.json — basic subagent-spawn plumbing, now
# under plan-and-execute (request-pipeline/08-planning.md).
#
# run_scenario.sh has already waited for the planning turn AND, seeing the
# :cp: fixtures, for every checkpoint turn to finish.
#
# The regression this guards: the caller_is_subagent NameError (temporal-
# workflow.md's Notes Log) once crashed ModelCall for ANY fixture that
# spawned a subagent. Here the spawn happens inside the one checkpoint turn.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
PLAN_ID="$2"                      # == the planning turn id
CP_TURN_ID="${PLAN_ID}:cp:1"
SUB_TURN_ID="${CP_TURN_ID}:sub:1" # subagent spawned from the checkpoint turn

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
plan_md() {
  kubectl exec -n "$NAMESPACE" deploy/abishekk-worker -- \
    cat "/sessions/session/$SESSION_KEY/plans/${1//:/_}/PLAN.md" 2>/dev/null || true
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

# --- the plan ran: planning turn + one checkpoint turn, both completed ---
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$PLAN_ID'")" = "completed" ] \
  || fail "planning turn ($PLAN_ID) not completed — did the message classify Lite instead of Deliberate?"
ok "planning turn completed"

cp_status="$(pg "SELECT status FROM turns WHERE turn_id = '$CP_TURN_ID'")"
[ "$cp_status" = "completed" ] || fail "checkpoint turn ($CP_TURN_ID) status = '$cp_status', expected 'completed'"
cp_plan="$(pg "SELECT COALESCE(plan_id,'') FROM turns WHERE turn_id = '$CP_TURN_ID'")"
[ "$cp_plan" = "$PLAN_ID" ] || fail "checkpoint turn plan_id = '$cp_plan', expected '$PLAN_ID'"
ok "checkpoint turn completed and carries the plan_id"

# --- the checkpoint turn dispatched a real subagent child workflow ---
sub_status="$(pg "SELECT status FROM turns WHERE turn_id = '$SUB_TURN_ID'")"
[ "$sub_status" = "completed" ] || fail "subagent turn ($SUB_TURN_ID) status = '$sub_status', expected 'completed'"
ok "subagent turn completed (spawned from the checkpoint turn)"

sub_row="$(pg "SELECT is_subagent FROM tool_calls WHERE tool_call_id = '$SUB_TURN_ID' AND tool_name = 'spawn_subagent'")"
[ "$sub_row" = "t" ] || fail "spawn_subagent tool_calls row for $SUB_TURN_ID not found / not is_subagent=true ('$sub_row')"
ok "spawn_subagent minted as a real subagent dispatch (is_subagent=true, tool_call_id == subagent turn_id)"

# --- the subagent's result was folded back (real dispatch, not a placeholder) ---
sub_result="$(pg "SELECT COALESCE(result::text,'') FROM tool_calls WHERE tool_call_id = '$SUB_TURN_ID'")"
[ -n "$sub_result" ] || fail "subagent's tool_calls.result is empty — expected a real manifest/result after completion"
ok "subagent's tool_calls.result is populated"

# --- the plan completed ---
PLAN="$(plan_md "$PLAN_ID")"
[ -n "$PLAN" ] || fail "no PLAN.md at the plan path"
echo "$PLAN" | grep -qE '^status: complete' || fail "PLAN.md status is not 'complete':\n$PLAN"
echo "$PLAN" | grep -qE '^- cp1 \[[x-]\]' || fail "PLAN.md: cp1 not terminal:\n$PLAN"
ok "PLAN.md complete, cp1 done"

exit 0
