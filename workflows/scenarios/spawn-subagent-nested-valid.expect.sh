#!/usr/bin/env bash
# Expectations for spawn-subagent-nested-valid.json — a subagent that
# delegates further, WITH genuine delegated_scope/kept_work, should succeed
# at every level: root -> subagent -> grandchild, all real child workflows,
# all completed.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"
SUBAGENT_TURN_ID="${ROOT_TURN_ID}:sub:1"
GRANDCHILD_TURN_ID="${SUBAGENT_TURN_ID}:sub:1"

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
ok "first-level subagent turn completed"

grandchild_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$GRANDCHILD_TURN_ID'")"
[ "$grandchild_status" = "completed" ] || fail "grandchild turn ($GRANDCHILD_TURN_ID) status = '$grandchild_status', expected 'completed' — nested delegation with genuine delegated_scope/kept_work should have been dispatched as a real child workflow, not rejected"
ok "grandchild (nested subagent) turn completed — the guard correctly allowed genuine delegation"

# The subagent's own spawn_subagent call (its tool_calls row, on the
# subagent's turn) must have been minted as a real subagent dispatch, not
# rejected — is_subagent=true and a subagent-shaped tool_call_id (the
# grandchild's turn_id, not an :act: activity ID).
spawn_row="$(pg_query "SELECT tool_call_id, is_subagent FROM tool_calls WHERE parent_id = '$SUBAGENT_TURN_ID' AND tool_name = 'spawn_subagent'")"
echo "$spawn_row" | grep -q "$GRANDCHILD_TURN_ID|t" || fail "subagent's spawn_subagent tool_calls row not found or not marked is_subagent=true: '$spawn_row'"
ok "subagent's nested spawn_subagent call minted as a real subagent dispatch (tool_call_id=$GRANDCHILD_TURN_ID, is_subagent=true)"

# The grandchild's real content should be recoverable from tool_calls.result
# by the time the whole tree is done (SubagentManifest / observation
# fold-in already ran) — confirms the dispatch produced a real result, not
# an empty/error placeholder.
grandchild_result="$(pg_query "SELECT result::text FROM tool_calls WHERE tool_call_id = '$GRANDCHILD_TURN_ID'")"
[ -n "$grandchild_result" ] && [ "$grandchild_result" != "" ] || fail "grandchild's own tool_calls.result is empty — expected a real manifest/result after completion"
ok "grandchild's tool_calls.result is populated (not empty)"

exit 0
