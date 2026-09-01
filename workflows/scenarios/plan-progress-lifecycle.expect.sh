#!/usr/bin/env bash
# Expectations for plan-progress-lifecycle.json — request-pipeline/08-planning.md.
#
# The three scripted plan_progress updates must land in turn_plan under the
# ids this fixture chose (c1/c2/c3 — none of which ComposeSkill's cp1..cpN
# seeding uses), regardless of whether the pipeline also seeded a plan from a
# matched skill (it usually does — even a short question routes to skills on
# this deploy):
#   c1 -> appended (unknown id + intent), status done
#   c2 -> appended in step 1 with NO status (-> pending), updated to done in step 2
#   c3 -> appended in step 2 with status skipped
# and the three are appended in order, each at MAX(checkpoint)+1.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

ROOT_TURN_ID="$2"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

root_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$root_status" = "completed" ] || fail "root turn status = '$root_status', expected 'completed'"
ok "root turn completed"

row() { pg_query "SELECT checkpoint || '|' || status || '|' || intent FROM turn_plan WHERE turn_id = '$ROOT_TURN_ID' AND cp_id = '$1'"; }

c1="$(row c1)"; c2="$(row c2)"; c3="$(row c3)"
[ -n "$c1" ] || fail "c1 missing from turn_plan — plan_progress append path did not run"
[ -n "$c2" ] || fail "c2 missing from turn_plan"
[ -n "$c3" ] || fail "c3 missing from turn_plan"
echo "  c1 = $c1"
echo "  c2 = $c2"
echo "  c3 = $c3"

echo "$c1" | grep -q "|done|" || fail "c1 status != done: $c1"
echo "$c1" | grep -qi "definition" || fail "c1 intent lost: $c1"
ok "c1 appended with its intent, status done"

echo "$c2" | grep -q "|done|" || fail "c2 status != done (appended without status in step 1, should be updated to done in step 2): $c2"
ok "c2 appended without status, later updated to done"

echo "$c3" | grep -q "|skipped|" || fail "c3 status != skipped: $c3"
ok "c3 appended with status skipped"

n1="${c1%%|*}"; n2="${c2%%|*}"; n3="${c3%%|*}"
[ "$n1" -lt "$n2" ] && [ "$n2" -lt "$n3" ] || fail "appended checkpoints not in order: c1=$n1 c2=$n2 c3=$n3"
ok "the three were appended in order (ordinals $n1 < $n2 < $n3)"

# any pipeline-seeded checkpoints sit below the appended ones
seeded="$(pg_query "SELECT count(*) FROM turn_plan WHERE turn_id = '$ROOT_TURN_ID' AND cp_id LIKE 'cp%'")"
echo "  (pipeline also seeded $seeded checkpoint(s) from a matched skill — appended ids start above them)"

exit 0
