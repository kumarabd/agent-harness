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
# both facts unambiguous. Acted on 2026-09-08 (they were flaking on model
# quality — the fast-tier model either answers incompletely or thrashes
# lcm_grep to max_iterations, while a direct build_conversation check confirms
# the assembled context DOES carry both facts): 3/4 now WARN, not fail. 1/2
# (assembly ran without crashing, ModelCall produced output) stay hard — those
# are the real regression signal for step 9.
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
if echo "$answer" | grep -q "14:12"; then
  ok "answer cites a fact only present in the verbatim message window"
else
  echo "  WARN: answer missing '14:12' (verbatim window) — model quality, not an assembly regression (see header): '$answer'"
fi

if echo "$answer" | grep -Eq "0\.4|0,4"; then
  ok "answer cites a fact only present in a reconstructed tool result"
else
  echo "  WARN: answer missing the '0.4%' error rate (reconstructed tool result) — model quality, not an assembly regression (see header): '$answer'"
fi

exit 0
