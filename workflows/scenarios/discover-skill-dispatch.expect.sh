#!/usr/bin/env bash
# Expectations for discover-skill-dispatch.json — docs/05-architecture-domain-control-loops.md,
# docs/components/turn-pipeline.md ("Skills"). draft_note is a static,
# always-on capability (capabilities.py, llm.TOOLS_SCHEMA) — directly
# callable by its own real name from turn 1, no discover_skills/minting step
# required. This proves the dispatch/close-out half of the mechanism end to
# end for real:
#
#   1. the turn completed — a crash anywhere in the skill child-workflow
#      dispatch/close-out fails the turn.
#   2. the draft_note tool_calls row was minted with
#      resolved_workflow_type='DraftNoteSkillWorkflow' — proof
#      model_call.py's minting loop routed it through the skill path (the
#      static _cap.BY_NAME lookup), not the ordinary activity path.
#   3. that row reached status='ok' with a non-null result — proof turn.go's
#      UseSkill dispatch branch actually started DraftNoteSkillWorkflow as a
#      child workflow, its internal approval gate was auto-approved by
#      run_scenario.sh's existing AUTO_APPROVE poller (no changes needed:
#      the request's turn_id is this turn's own root id), and the skill
#      closed its own row out via CloseSkillCall before returning.
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
[ "$status" = "completed" ] || fail "root turn status = '$status', expected 'completed'"
ok "root turn completed — the skill dispatch/close-out ran without error"

tc="$(pg_query "SELECT resolved_workflow_type || '|' || status FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'draft_note'")"
[ "$tc" = "DraftNoteSkillWorkflow|ok" ] || fail "draft_note tool_call is '$tc', expected 'DraftNoteSkillWorkflow|ok'"
ok "draft_note was minted as a skill (resolved_workflow_type set) and reached status='ok'"

result="$(pg_query "SELECT result FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'draft_note'")"
[ -n "$result" ] && [ "$result" != "null" ] || fail "draft_note's result is empty — CloseSkillCall may not have run"
ok "draft_note's result was written by CloseSkillCall: $result"

exit 0
