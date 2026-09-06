#!/usr/bin/env bash
# Expectations for spawn-subagent-nested-valid.json — the recursion-
# termination guard's happy path, under plan-and-execute. A subagent that
# delegates further WITH genuine delegated_scope/kept_work must succeed at
# every level: checkpoint turn -> subagent -> grandchild, all real child
# workflows, all completed.
#
# run_scenario.sh has already waited for the planning turn + every checkpoint
# turn (a checkpoint turn stays 'running' until its whole subagent subtree
# finishes, so by now the grandchild is terminal too).
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
PLAN_ID="$2"
CP_TURN_ID="${PLAN_ID}:cp:1"
SUB_TURN_ID="${CP_TURN_ID}:sub:1"
GRANDCHILD_TURN_ID="${SUB_TURN_ID}:sub:1"

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

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$PLAN_ID'")" = "completed" ] \
  || fail "planning turn ($PLAN_ID) not completed — did the message classify Lite instead of Deliberate?"
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$CP_TURN_ID'")" = "completed" ] \
  || fail "checkpoint turn ($CP_TURN_ID) not completed"
ok "planning + checkpoint turn completed"

sub_status="$(pg "SELECT status FROM turns WHERE turn_id = '$SUB_TURN_ID'")"
[ "$sub_status" = "completed" ] || fail "first-level subagent turn ($SUB_TURN_ID) status = '$sub_status', expected 'completed'"
ok "first-level subagent turn completed"

grandchild_status="$(pg "SELECT status FROM turns WHERE turn_id = '$GRANDCHILD_TURN_ID'")"
[ "$grandchild_status" = "completed" ] || fail "grandchild turn ($GRANDCHILD_TURN_ID) status = '$grandchild_status', expected 'completed' — nested delegation with genuine delegated_scope/kept_work should be dispatched as a real child workflow, not rejected"
ok "grandchild (nested subagent) turn completed — the guard correctly allowed genuine delegation"

# The subagent's own spawn_subagent call must have been minted as a real
# subagent dispatch (is_subagent=true, tool_call_id == the grandchild turn_id),
# not a rejected :act: activity.
# Postgres's `||` implicitly casts a boolean operand to the full word
# ("true"/"false"), not the "t"/"f" a bare `SELECT is_subagent` renders as
# (verified directly against this cluster) — matches spawn-subagent-nested-
# rejected.expect.sh's own `\|false\|error$`, this just wrongly expected "t".
spawn_row="$(pg "SELECT tool_call_id || '|' || is_subagent FROM tool_calls WHERE parent_id = '$SUB_TURN_ID' AND tool_name = 'spawn_subagent'")"
echo "$spawn_row" | grep -q "^${GRANDCHILD_TURN_ID}|true$" \
  || fail "subagent's nested spawn_subagent row not a real subagent dispatch: '$spawn_row'"
ok "subagent's nested spawn_subagent minted as a real subagent dispatch ($spawn_row)"

grandchild_result="$(pg "SELECT COALESCE(result::text,'') FROM tool_calls WHERE tool_call_id = '$GRANDCHILD_TURN_ID'")"
[ -n "$grandchild_result" ] || fail "grandchild's tool_calls.result is empty — expected a real manifest/result after completion"
ok "grandchild's tool_calls.result is populated (not empty)"

PLAN="$(plan_md "$PLAN_ID")"
echo "$PLAN" | grep -qE '^status: complete' || fail "PLAN.md status is not 'complete':\n$PLAN"
ok "PLAN.md complete"

exit 0
