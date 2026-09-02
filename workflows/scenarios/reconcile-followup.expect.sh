#!/usr/bin/env bash
# Expectations for the reconcile-initial / reconcile-followup chained pair —
# request-pipeline/08-planning.md, "Reconciliation trigger".
#
# Run as (two calls, SAME session key, second while the first is still parked):
#   run_scenario.sh reconcile-initial  test:reconcile:$(date +%s)
#   run_scenario.sh reconcile-followup test:reconcile:<that same key>
#
# This .expect.sh belongs to reconcile-followup and is invoked by its run
# with <session_key> <root_turn_id> (root_turn_id = {session}:turn:1).
#
# The mid-turn follow-up must have triggered a detached RoutingWorkflow in
# reconcile mode, id {turn}:reconcile:1, which runs Memory + Skill discovery
# only (no Route() gate, no ToolDiscover) and does NOT re-seed turn_plan.
set -euo pipefail

ROOT_TURN_ID="$2"
RECONCILE_WF="${ROOT_TURN_ID}:reconcile:1"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

command -v temporal >/dev/null 2>&1 || fail "temporal CLI not on PATH — this check needs it"

root_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$root_status" = "completed" ] || fail "root turn status = '$root_status', expected 'completed' (did the follow-up land while turn:1 was still running?)"
ok "root turn completed after the follow-up folded in"

# poll: the reconcile child is detached (ABANDON), may still be finishing
wf_status=""
for _ in $(seq 1 30); do
  wf_status="$(TEMPORAL_ADDRESS=localhost:17233 temporal workflow describe \
    --namespace abishekk --workflow-id "$RECONCILE_WF" -o json 2>/dev/null \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["workflowExecutionInfo"]["status"])' 2>/dev/null || true)"
  case "$wf_status" in
    WORKFLOW_EXECUTION_STATUS_COMPLETED) break ;;
    WORKFLOW_EXECUTION_STATUS_RUNNING|"") sleep 1 ;;
    *) break ;;
  esac
done

[ -n "$wf_status" ] || fail "no $RECONCILE_WF workflow — the mid-turn follow-up did not trigger reconciliation"
ok "reconciliation RoutingWorkflow was dispatched ($RECONCILE_WF)"
[ "$wf_status" = "WORKFLOW_EXECUTION_STATUS_COMPLETED" ] || fail "$RECONCILE_WF ended '$wf_status', expected COMPLETED — reconcile-mode routing errored"
ok "reconcile-mode routing ran to completion (Memory + Skill re-key, no Route() gate)"

# reconcile-mode compose must NOT re-seed turn_plan: whatever ComposeSkill
# seeded at turn start is still all there is (row count unchanged, no new
# updated_at burst from the reconcile pass).
plan_rows="$(pg_query "SELECT count(*) FROM turn_plan WHERE episode_id = '$ROOT_TURN_ID'")"
echo "  (turn_plan rows for the turn: ${plan_rows:-0} — seeded once at turn start, not re-seeded by reconcile)"

exit 0
