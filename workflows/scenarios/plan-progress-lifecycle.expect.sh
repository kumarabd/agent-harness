#!/usr/bin/env bash
# Expectations for plan-progress-lifecycle.json — request-pipeline/08-planning.md.
#
# REVISED 2026-09-03 (Phase 3 slice B): the checkpoint ledger is a PLAN.md file
# on the tenant PV, not the turn_plan table. Read it off the worker pod.
#
# The three scripted plan_progress updates (c1/c2/c3 — ids ComposeSkill's
# cp1..cpN seeding never uses) must land in PLAN.md:
#   c1 -> appended (unknown id + intent), status done
#   c2 -> appended in step 1 with NO status (-> pending), updated to done in step 2
#   c3 -> appended in step 2 with status skipped
# appended in order, each below any pipeline-seeded cpN lines.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
# PLAN.md lives at $SESSION_ROOT/session/<key>/plans/<episode_id ':'->'_'>/PLAN.md
# ($SESSION_ROOT = /sessions in the deploy — activities/plan.py, tools.py).
plan_md() {
  local ep_safe="${1//:/_}"
  kubectl exec -n "$NAMESPACE" deploy/abishekk-worker -- \
    cat "/sessions/session/$SESSION_KEY/plans/$ep_safe/PLAN.md" 2>/dev/null || true
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

root_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$root_status" = "completed" ] || fail "root turn status = '$root_status', expected 'completed'"
ok "root turn completed"

ep="$(pg_query "SELECT episode_id FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$ep" = "$ROOT_TURN_ID" ] || fail "turns.episode_id = '$ep', expected the root turn id"
ok "episode opened, episode_id == root turn id"

PLAN="$(plan_md "$ROOT_TURN_ID")"
[ -n "$PLAN" ] || fail "PLAN.md is empty/missing — plan_progress append path did not run (or the PV path is wrong)"
echo "--- PLAN.md ---"; echo "$PLAN"; echo "---"

line() { echo "$PLAN" | grep -E "^- $1 " || true; }
c1="$(line c1)"; c2="$(line c2)"; c3="$(line c3)"
[ -n "$c1" ] || fail "c1 not in PLAN.md"
[ -n "$c2" ] || fail "c2 not in PLAN.md"
[ -n "$c3" ] || fail "c3 not in PLAN.md"

echo "$c1" | grep -q '\[x\]'  || fail "c1 not marked done: $c1"
echo "$c1" | grep -qi definition || fail "c1 intent lost: $c1"
ok "c1 appended with its intent, marked done"

echo "$c2" | grep -q '\[x\]' || fail "c2 not done (appended without status in step 1, updated in step 2): $c2"
ok "c2 appended without status, later updated to done"

echo "$c3" | grep -q '\[-\]' || fail "c3 not skipped: $c3"
ok "c3 appended with status skipped"

# order: c1, c2, c3 lines appear in that order in the file
order="$(echo "$PLAN" | grep -nE '^- c[123] ' | sed 's/:.*- \(c[123]\) .*/\1/' | tr -d '\n')"
[ "$order" = "c1c2c3" ] || fail "appended checkpoints out of order in PLAN.md: '$order'"
ok "the three were appended in order"

seeded="$(echo "$PLAN" | grep -cE '^- cp[0-9]+ ' || true)"
echo "  (pipeline also seeded ${seeded:-0} cpN checkpoint(s) from a matched skill)"

exit 0
