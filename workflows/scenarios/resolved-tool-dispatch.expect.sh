#!/usr/bin/env bash
# Expectations for resolved-tool-dispatch.json — forces the real ModelCall
# path against a pre-seeded turn_retrieval 'tool' row (setup.sql), checking
# that per-task tool resolution (docs/components/tool-registry.md, "Resolved:
# Three-Layer Tool Taxonomy & Per-Task Resolution") ran end to end for real:
#
#   1. the turn completed — a crash in mint_resolved / schema_for / the
#      tool_calls minting loop's resolved-tool branch fails the turn (no
#      fallback).
#   2. a tool_calls row named 'echo' exists on the root turn — the model
#      called the RESOLVED name directly, not search_tools then call_tool
#      (call_tool isn't even in its schema anymore).
#   3. that row's resolved_server/resolved_tool (migration 026) are
#      'demo'/'echo' — the exact identity ModelCall must have looked up from
#      the Capability mint_resolved built off the seeded row, proving the
#      mint-time resolution (not just the schema) worked.
#
# Assertion 2 depends on a cooperative model actually calling the tool it was
# explicitly, unambiguously told to call. If this flakes on model compliance
# rather than a real regression, loosen to 1 only.
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
[ "$status" = "completed" ] || fail "root turn status = '$status', expected 'completed' — a resolved-tool schema/mint/dispatch crash fails the turn"
ok "root turn completed — per-task tool resolution ran without error"

n_echo="$(pg_query "SELECT count(*) FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'echo'")"
[ "${n_echo:-0}" -ge 1 ] || fail "no tool_calls row named 'echo' — the model didn't call the resolved tool by name (or wasn't offered it)"
ok "model called the resolved tool directly by name, not via search_tools/call_tool"

target="$(pg_query "SELECT resolved_server || '/' || resolved_tool FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'echo' LIMIT 1")"
[ "$target" = "demo/echo" ] || fail "resolved_server/resolved_tool = '$target', expected 'demo/echo' — ModelCall's mint-time resolution didn't match the seeded Capability"
ok "resolved_server/resolved_tool correctly routed to demo/echo"

exit 0
