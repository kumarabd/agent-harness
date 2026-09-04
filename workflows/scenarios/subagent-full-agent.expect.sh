#!/usr/bin/env bash
# Expectations for subagent-full-agent.json — request-pipeline/08-planning.md,
# "Subagents are full agents", under plan-and-execute.
#
# The subagent is spawned from the one checkpoint turn (<plan>:cp:1), so its
# turn_id is <plan>:cp:1:sub:1. It runs the pre-LLM pipeline itself:
#   - its own RoutingWorkflow child (<sub>:routing) executes
#   - being Deliberate it opens its own single-turn task-run: plan_id == its
#     turn_id (openedFresh, turn.go) — NOT a nested PlanWorkflow
#   - dispatchRecordSkill fires for it -> a skill_procedures row in source_ids
#     (this NEVER happened for subagents before the gate removal)
#
# run_scenario.sh has already waited for the planning turn + every checkpoint
# turn (a checkpoint turn stays 'running' until its subagent subtree finishes).
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
PLAN_ID="$2"
CP_TURN_ID="${PLAN_ID}:cp:1"
SUB_TURN_ID="${CP_TURN_ID}:sub:1"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }
poll_for() { # <sql count> <min> <label> <tries>
  local sql="$1" min="$2" label="$3" tries="${4:-20}" v
  for _ in $(seq 1 "$tries"); do
    v="$(pg "$sql")"
    [ "${v:-0}" -ge "$min" ] 2>/dev/null && { echo "  ok: $label ($v)"; return 0; }
    sleep 1
  done
  fail "$label — got '${v:-<none>}', expected >= $min after ${tries}s"
}

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$PLAN_ID'")" = "completed" ] \
  || fail "planning turn ($PLAN_ID) not completed — did the message classify Lite instead of Deliberate?"
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$CP_TURN_ID'")" = "completed" ] \
  || fail "checkpoint turn ($CP_TURN_ID) not completed"
sub_status="$(pg "SELECT status FROM turns WHERE turn_id = '$SUB_TURN_ID'")"
[ "$sub_status" = "completed" ] || fail "subagent turn ($SUB_TURN_ID) status = '$sub_status', expected 'completed'"
ok "planning + checkpoint + subagent turns all completed"

# --- the subagent opened its own task-run (Deliberate, openedFresh) ---
sub_plan="$(pg "SELECT COALESCE(plan_id,'') FROM turns WHERE turn_id = '$SUB_TURN_ID'")"
[ "$sub_plan" = "$SUB_TURN_ID" ] || fail "subagent turns.plan_id = '$sub_plan', expected its own turn_id (openedFresh) — did it classify Lite?"
ok "subagent opened its own task-run (turns.plan_id == turn_id)"

# --- it is a single reason-act loop, not a nested PlanWorkflow ---
nested_cp="$(pg "SELECT count(*) FROM turns WHERE parent_id = '$SUB_TURN_ID' AND parent_type = 'plan'")"
[ "${nested_cp:-0}" = "0" ] || fail "subagent spawned $nested_cp nested checkpoint turn(s) — a Deliberate subagent is a single-turn task-run, not a PlanWorkflow"
ok "subagent ran a single reason-act loop (no nested checkpoint turns)"

# --- steps 2+3 ran for the subagent: its RoutingWorkflow child executed ---
if command -v temporal >/dev/null 2>&1; then
  rstatus="$(TEMPORAL_ADDRESS=localhost:17233 temporal workflow describe \
    --namespace abishekk --workflow-id "${SUB_TURN_ID}:routing" -o json 2>/dev/null \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["workflowExecutionInfo"]["status"])' 2>/dev/null || true)"
  [ -n "$rstatus" ] || fail "no ${SUB_TURN_ID}:routing workflow — the subagent did not run step 3"
  ok "subagent ran its own RoutingWorkflow (status: $rstatus)"
else
  echo "  SKIP: temporal CLI not on PATH — cannot check the subagent's RoutingWorkflow directly"
fi

# --- RecordSkill fired for the subagent (dispatchRecordSkill at turn end) ---
poll_for "SELECT count(*) FROM skill_procedures WHERE source_ids @> jsonb_build_array('$SUB_TURN_ID')" 1 \
  "skill_procedures row written/versioned for the subagent's task-run (RecordSkill)" 40

exit 0
