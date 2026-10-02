#!/usr/bin/env bash
# Expectations for load-skill-dispatch.json — a fully fixture-scripted
# scenario (no real LLM call) exercising load_skill's real dispatch path
# (docs/components/memory-slot.md, "Resolved: load_skill"): load_skill is an
# ordinary model-volitional tool, not a pipeline stage, so a scripted call to
# it still mints a real tool_calls row and runs the real handler
# (tenant_worker/tools.py's load_skill -> agent_brain.call_retain_tool).
#
# agent-brain doesn't implement memory_load_skill yet, so this call is
# expected to fail — either AgentBrainNotConfiguredError (base URL unset) or
# AgentBrainCallError (configured, but the backend tool doesn't exist). Both
# propagate uncaught out of the handler exactly like they already do for
# recall/reflect, and the generic ToolCall activity's own catch-all turns
# that into an ordinary status='error' tool_calls row (tool_call.py's
# _finish_error) — not a crashed activity, not a stuck turn. This scenario
# checks exactly that degrade-gracefully behavior:
#
#   1. the turn completed — a failed load_skill call must not crash the turn
#      or leave it stuck; the model's second scripted step (no tool_calls)
#      should run normally after seeing the failed tool observation.
#   2. a tool_calls row exists for this turn naming tool_name='load_skill'.
#   3. that row's status is 'error' — confirming the real handler actually
#      ran (and failed, as expected given agent-brain has no backend for
#      this yet), not that it was silently skipped.
#   4. that row's result carries a non-empty error message (_finish_error's
#      json.dumps({"error": message}) shape) — confirming the error path
#      wrote a real, usable observation rather than an empty one.
#
# Once agent-brain ships memory_load_skill for real, a companion scenario
# should exercise the success path (tool_refs persisted into turn_retrieval,
# then a genuine follow-up call on one of them) — not possible to script
# today since there's nothing real to return.
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
[ "$status" = "completed" ] || fail "root turn status = '$status', expected 'completed' — a failed load_skill call should degrade gracefully, not crash or stick the turn"
ok "root turn completed — a failing load_skill call degraded gracefully"

call_status="$(pg_query "SELECT status FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'load_skill' ORDER BY started_at LIMIT 1")"
[ -n "$call_status" ] || fail "no tool_calls row for tool_name='load_skill' under this turn — the scripted call never dispatched"
ok "load_skill dispatched as a real tool_calls row"

[ "$call_status" = "error" ] || fail "load_skill tool_calls row status = '$call_status', expected 'error' (agent-brain has no memory_load_skill backend yet)"
ok "load_skill failed as expected — agent-brain doesn't implement memory_load_skill yet"

err_msg="$(pg_query "SELECT result->>'error' FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'load_skill' ORDER BY started_at LIMIT 1")"
[ -n "$err_msg" ] || fail "load_skill's error row has no result->>'error' message — _finish_error should always write one"
ok "error observation carries a real message: $err_msg"

exit 0
