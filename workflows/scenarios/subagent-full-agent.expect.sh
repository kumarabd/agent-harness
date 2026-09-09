#!/usr/bin/env bash
# Expectations for subagent-full-agent.json — temporal-workflow.md,
# "Subagents are full agents", on the flat reason-act loop (turn-pipeline.md
# Phase 8: no ClassifyRequest, no RoutingWorkflow, no lanes; skill subsystem
# removed).
#
# The subagent is spawned from turn:1, so its turn_id is <turn:1>:sub:1. It
# runs the same reason-act loop as a top-level turn — multiple tool-using
# steps, its own tool_calls under its own id, and completes cleanly. No
# pre-LLM pipeline, no plan_id, no RecordSkill.
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

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] \
  || fail "root turn ($ROOT_TURN_ID) not completed"
sub_status="$(pg "SELECT status FROM turns WHERE turn_id = '$SUB_TURN_ID'")"
[ "$sub_status" = "completed" ] || fail "subagent turn ($SUB_TURN_ID) status = '$sub_status', expected 'completed'"
ok "root + subagent turns completed"

# --- no pre-LLM pipeline ran (Phase 8): no :routing child, no plan_id ---
if command -v temporal >/dev/null 2>&1; then
  rstatus="$(TEMPORAL_ADDRESS=localhost:17233 temporal workflow describe \
    --namespace abishekk --workflow-id "${SUB_TURN_ID}:routing" -o json 2>/dev/null \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["workflowExecutionInfo"]["status"])' 2>/dev/null || true)"
  [ -z "$rstatus" ] || fail "a ${SUB_TURN_ID}:routing workflow exists ($rstatus) — RoutingWorkflow should be gone"
  ok "no RoutingWorkflow child (pre-LLM pipeline removed)"
else
  echo "  SKIP: temporal CLI not on PATH"
fi
sub_plan="$(pg "SELECT COALESCE(plan_id,'') FROM turns WHERE turn_id = '$SUB_TURN_ID'")"
[ -z "$sub_plan" ] || fail "subagent turns.plan_id = '$sub_plan', expected empty (plan_id is no longer written)"
ok "turns.plan_id unset"

# --- the subagent ran its own multi-step loop with its own tool calls ---
sub_tools="$(pg "SELECT count(*) FROM tool_calls WHERE parent_id = '$SUB_TURN_ID'")"
[ "${sub_tools:-0}" -ge 2 ] || fail "subagent made $sub_tools tool calls, expected >= 2"
ok "subagent ran its own multi-step loop ($sub_tools tool calls under its own id)"

# --- the skill subsystem is gone: no RecordSkill child, no skill_procedures ---
rec="$(pg "SELECT count(*) FROM skill_procedures WHERE source_ids @> jsonb_build_array('$SUB_TURN_ID')" 2>/dev/null || echo 0)"
[ "${rec:-0}" = "0" ] || fail "a skill_procedures row references this subagent — RecordSkill should be gone"
ok "no RecordSkill (skill subsystem removed)"

exit 0
