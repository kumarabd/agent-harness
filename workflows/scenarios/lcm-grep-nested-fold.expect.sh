#!/usr/bin/env bash
# Expectations for lcm-grep-nested-fold.json — the real bug found and fixed
# 2026-08-29: lcm_grep's covered_by_summary_id used to walk to the topmost
# folded_into ancestor (S7), collapsing two genuinely different regions
# (S3, S4) into one unhelpfully-broad answer. This scenario proves the fix:
# a grep hit under S3 must report S3, a hit under S4 must report S4, and S7
# must never appear in covered_by_summary_id at all.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

ROOT_TURN_ID="$2"
S3="f0000000-0000-0000-0000-000000000003"
S4="f0000000-0000-0000-0000-000000000004"
S7="a0000000-0000-0000-0000-000000000007"
MSG_GQA_1="d0000000-0000-0000-0000-000000000001"
MSG_GQA_2="d0000000-0000-0000-0000-000000000003"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}

fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

root_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$root_status" = "completed" ] || fail "root turn status = '$root_status', expected 'completed'"
ok "root turn completed"

grep_result="$(pg_query "SELECT result::text FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'lcm_grep'")"
echo "$grep_result" | grep -qi "GQA" || fail "lcm_grep result doesn't mention GQA: '$grep_result'"

echo "$grep_result" | python3 -c "
import json, sys
result = json.load(sys.stdin)
by_id = {r['message_id']: r['covered_by_summary_id'] for r in result['results']}
assert by_id.get('$MSG_GQA_1') == '$S3', f\"expected S3 for msg 1, got {by_id.get('$MSG_GQA_1')!r}\"
assert by_id.get('$MSG_GQA_2') == '$S4', f\"expected S4 for msg 2, got {by_id.get('$MSG_GQA_2')!r}\"
assert '$S7' not in by_id.values(), 'S7 (topmost) leaked into results — the topmost-ancestor bug is back'
print('nearest-covering-node check passed inside python')
" || fail "covered_by_summary_id did not resolve to the nearest node (S3/S4), not the topmost (S7) — see script output above"
ok "lcm_grep reports S3/S4 (nearest covering node) for each hit, never S7 (topmost ancestor)"

exit 0
