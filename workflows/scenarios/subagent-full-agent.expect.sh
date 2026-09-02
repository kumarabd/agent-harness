#!/usr/bin/env bash
# Expectations for subagent-full-agent.json — request-pipeline/08-planning.md,
# "Subagents are full agents".
#
# The subagent turn ({root}:sub:1) must now run the pre-LLM pipeline itself:
#   - RoutingWorkflow child ({subagent}:routing) executes
#   - RecordSkillOutcome fires -> a skill_candidates row keyed to the subagent
#     turn_id (this NEVER happened for subagents before the gate removal)
#   - the scripted plan_progress lands in turn_plan under the subagent episode_id
#
# The RoutingWorkflow check uses `temporal workflow describe` against the
# port-forward run_scenario.sh already set up on localhost:17233.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

ROOT_TURN_ID="$2"
SUB_TURN_ID="${ROOT_TURN_ID}:sub:1"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }
poll_for() { # <sql count> <min> <label> <tries>
  local sql="$1" min="$2" label="$3" tries="${4:-20}" v
  for _ in $(seq 1 "$tries"); do
    v="$(pg_query "$sql")"
    [ "${v:-0}" -ge "$min" ] 2>/dev/null && { echo "  ok: $label ($v)"; return 0; }
    sleep 1
  done
  fail "$label — got '${v:-<none>}', expected >= $min after ${tries}s"
}

root_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$root_status" = "completed" ] || fail "root turn status = '$root_status', expected 'completed'"
sub_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$SUB_TURN_ID'")"
[ "$sub_status" = "completed" ] || fail "subagent turn ($SUB_TURN_ID) status = '$sub_status', expected 'completed'"
ok "root and subagent turns both completed"

# --- steps 2+3 ran for the subagent: its RoutingWorkflow child executed ---
if command -v temporal >/dev/null 2>&1; then
  rstatus="$(TEMPORAL_ADDRESS=localhost:17233 temporal workflow describe \
    --namespace abishekk --workflow-id "${SUB_TURN_ID}:routing" -o json 2>/dev/null \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["workflowExecutionInfo"]["status"])' 2>/dev/null || true)"
  [ -n "$rstatus" ] || fail "no ${SUB_TURN_ID}:routing workflow — the subagent did not run step 3 (gate not removed?)"
  ok "subagent ran its own RoutingWorkflow (status: $rstatus)"
else
  echo "  SKIP: temporal CLI not on PATH — cannot check the subagent's RoutingWorkflow directly"
fi

# --- RecordSkillOutcome fired for the subagent (new behavior) ---
poll_for "SELECT count(*) FROM skill_candidates WHERE turn_id = '$SUB_TURN_ID'" 1 \
  "skill_candidates row written for the subagent turn (RecordSkillOutcome gate widened to ParentType=='turn')" 25

sub_cand="$(pg_query "SELECT outcome || '|' || (task_embedding IS NOT NULL) FROM skill_candidates WHERE turn_id = '$SUB_TURN_ID'")"
echo "  (subagent candidate: outcome|has_embedding = $sub_cand)"

# --- plan_progress landed on the subagent turn ---
sub_cp="$(pg_query "SELECT checkpoint || '|' || status FROM turn_plan WHERE episode_id = '$SUB_TURN_ID' AND cp_id = 'audit-1'")"
[ -n "$sub_cp" ] || fail "no turn_plan row 'audit-1' for the subagent turn — plan_progress not applied on a non-root turn"
echo "$sub_cp" | grep -q "|done$" || fail "subagent's audit-1 = '$sub_cp', expected status done"
ok "plan_progress applied on the subagent turn (audit-1 -> $sub_cp)"

exit 0
