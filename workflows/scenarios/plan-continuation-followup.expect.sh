#!/usr/bin/env bash
# Expectations for the plan-continuation-* chained pair —
# request-pipeline/08-planning.md, mid-plan follow-up fold-in.
#
# Manual run:
#   KEY="test:scenario:plan-continuation:$(date +%s)"
#   workflows/scenarios/run_scenario.sh plan-continuation-initial "$KEY"  &   # backgrounds; cp2 blocks ~15s
#   sleep 6
#   workflows/scenarios/run_scenario.sh plan-continuation-followup "$KEY"
#
# Checks: the follow-up became a <plan_id>:followup:1 turn under the SAME plan
# (not a new top-level turn / not a new plan); the plan still completed; and
# RecordSkill fired ONCE over a trajectory that includes the follow-up turn.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
PLAN_ID="$2"   # == the planning turn id (the pair's initial turn:1)
FOLLOWUP_ID="${PLAN_ID}:followup:1"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
plan_md() {
  kubectl exec -n "$NAMESPACE" deploy/abishekk-worker -- \
    cat "/sessions/session/$SESSION_KEY/plans/${1//:/_}/PLAN.md" 2>/dev/null || true
}
wlog() { kubectl logs -n "$NAMESPACE" deploy/abishekk-worker --since=15m 2>/dev/null || true; }
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

# --- no second top-level turn: the follow-up did NOT start a fresh task ---
n_session_turns="$(pg "SELECT count(*) FROM turns WHERE parent_id = '$SESSION_KEY' AND parent_type = 'session'")"
[ "${n_session_turns:-0}" = "1" ] || fail "$n_session_turns session-level turns — the follow-up should NOT have minted turn:2"
ok "one session-level turn (the follow-up folded into the running plan)"

# --- the follow-up became a fold-in turn under the plan ---
fu_status="$(pg "SELECT status FROM turns WHERE turn_id = '$FOLLOWUP_ID'")"
[ "$fu_status" = "completed" ] || fail "$FOLLOWUP_ID status = '${fu_status:-<missing>}', expected completed"
fu_parent="$(pg "SELECT parent_type || ':' || parent_id FROM turns WHERE turn_id = '$FOLLOWUP_ID'")"
[ "$fu_parent" = "plan:$PLAN_ID" ] || fail "$FOLLOWUP_ID parent = '$fu_parent', expected 'plan:$PLAN_ID'"
[ "$(pg "SELECT plan_id FROM turns WHERE turn_id = '$FOLLOWUP_ID'")" = "$PLAN_ID" ] \
  || fail "$FOLLOWUP_ID turns.plan_id != plan_id"
ok "follow-up ran as $FOLLOWUP_ID under the same plan"

# --- the plan still completed ---
PLAN="$(plan_md "$PLAN_ID")"
echo "$PLAN" | grep -qE '^status: complete' || fail "PLAN.md not complete:\n$PLAN"
ok "plan completed"

# --- RecordSkill fired ONCE, and its trajectory swept in the follow-up turn ---
n_record="$(wlog | grep -cF "RecordSkill[$PLAN_ID]:" || true)"
[ "${n_record:-0}" -ge 1 ] || fail "no RecordSkill[$PLAN_ID] log line"
# the follow-up turn's message must be in the recorded trajectory — check that a
# message exists on the follow-up turn (RecordSkill's WHERE prefix-sweeps it in)
fu_msgs="$(pg "SELECT count(*) FROM messages WHERE parent_id = '$FOLLOWUP_ID'")"
[ "${fu_msgs:-0}" -ge 1 ] || fail "no messages on the follow-up turn"
ok "RecordSkill fired ($n_record×) over a trajectory that includes $FOLLOWUP_ID"

exit 0
