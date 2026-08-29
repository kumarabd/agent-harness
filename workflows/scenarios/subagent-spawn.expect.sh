#!/usr/bin/env bash
# Expectations for the pre-existing subagent-spawn.json — added 2026-08-29
# alongside the recursion-termination guard suite. This scenario is exactly
# what the caller_is_subagent NameError bug (found and fixed while building
# that guard — see components/temporal-workflow.md's Notes Log) would have
# broken: ANY fixture spawning a subagent crashed ModelCall the moment it
# hit `if is_subagent and caller_is_subagent:` with caller_is_subagent
# undefined. This file exists so that regression can never silently ship
# again unnoticed.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

ROOT_TURN_ID="$2"
SUBAGENT_TURN_ID="${ROOT_TURN_ID}:sub:1"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}

fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

root_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$root_status" = "completed" ] || fail "root turn status = '$root_status', expected 'completed'"
ok "root turn completed"

sub_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$SUBAGENT_TURN_ID'")"
[ "$sub_status" = "completed" ] || fail "subagent turn ($SUBAGENT_TURN_ID) status = '$sub_status', expected 'completed'"
ok "subagent turn completed"

final_message="$(pg_query "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
echo "$final_message" | grep -qi "wrapping up" || fail "root's final message doesn't match expected fixture content: '$final_message'"
ok "root produced its expected final response after the subagent completed"

exit 0
