#!/usr/bin/env bash
# Expectations for real-assembly.json — the scenario forces the real
# (non-fixture) ModelCall path, so this checks that request-pipeline step 9
# actually assembled the seeded multi-turn context:
#
#   1. the turn completed at all — a crash in build_conversation /
#      lcm.assemble / prompt.assemble (e.g. a broken batched tool_calls
#      fetch) raises inside ModelCall and fails the turn (no fallback).
#   2. the real ModelCall wrote an assistant response.
#   3. the response contains "14:12" — present only in verbatim window
#      messages (turn:0 seq 3 / seq 11), NOT in the seeded summary. Proves
#      lcm.assemble's session-message window assembled.
#   4. the response contains the "0.4%" error rate — present only in a
#      tool_calls.result row (turn:0 act:2), NOT in any messages row. Proves
#      the batched tool_calls fetch + tool-result reconstruction worked.
#
# Assertions 3/4 depend on a cooperative fast-tier model; the context makes
# both facts unambiguous. If this flakes on model quality rather than an
# assembly regression, loosen to 1/2 only.
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

status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$status" = "completed" ] || fail "root turn status = '$status', expected 'completed' — a build_conversation/lcm.assemble crash fails the turn"
ok "root turn completed — real prompt assembly ran without error"

n_assistant="$(pg_query "SELECT count(*) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' AND coalesce(content, '') <> ''")"
[ "${n_assistant:-0}" -ge 1 ] || fail "no non-empty assistant message on the root turn — the real ModelCall produced nothing"
ok "real ModelCall produced an assistant response"

answer="$(pg_query "SELECT string_agg(content, ' ') FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant'")"
echo "$answer" | grep -q "14:12" \
  || fail "answer missing '14:12' (only in verbatim window messages) — the session-message window may not have assembled: '$answer'"
ok "answer cites a fact only present in the verbatim message window"

echo "$answer" | grep -Eq "0\.4|0,4" \
  || fail "answer missing the '0.4%' error rate (only in tool_calls.result) — tool-result reconstruction may be broken: '$answer'"
ok "answer cites a fact only present in a reconstructed tool result"

exit 0
