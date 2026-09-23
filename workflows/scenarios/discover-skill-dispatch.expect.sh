#!/usr/bin/env bash
# Expectations for discover-skill-dispatch.json — docs/05-architecture-domain-control-loops.md,
# docs/components/turn-pipeline.md ("Skills"). Proves the whole mechanism end
# to end for real, not just discovery:
#
#   1. the turn completed — a crash anywhere in discover_skills' persist path,
#      the mint path, or the skill child-workflow dispatch/close-out fails
#      the turn.
#   2. turn_retrieval gained a kind='skill' row for this turn, with a real
#      {name, workflow_type} identity in its metadata — what
#      capabilities.mint_resolved_skills needs to bind it as a callable
#      schema (docs/05-architecture-domain-control-loops.md's kind='skill'
#      CHECK value, previously dead, is now exercised for real).
#   3. the draft_note tool_calls row was minted with is_skill=true and
#      resolved_workflow_type='DraftNoteSkillWorkflow' — proof model_call.py's
#      minting loop routed it through the skill path, not the ordinary
#      activity path.
#   4. that row reached status='ok' with a non-null result — proof turn.go's
#      isSkill dispatch branch actually started DraftNoteSkillWorkflow as a
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
ok "root turn completed — discovery, minting, and skill dispatch/close-out all ran without error"

n_skill_rows="$(pg_query "SELECT count(*) FROM turn_retrieval WHERE owner_id = '$ROOT_TURN_ID' AND kind = 'skill'")"
[ "${n_skill_rows:-0}" -ge 1 ] || fail "no turn_retrieval kind='skill' rows for this turn — discover_skills found nothing to persist (check skill_hub.init() ran: EMBEDDING_BASE_URL set?)"
ok "discover_skills persisted $n_skill_rows discovered skill row(s) into turn_retrieval"

n_with_target="$(pg_query "SELECT count(*) FROM turn_retrieval WHERE owner_id = '$ROOT_TURN_ID' AND kind = 'skill' AND metadata->>'workflow_type' = 'DraftNoteSkillWorkflow'")"
[ "${n_with_target:-0}" -ge 1 ] || fail "no persisted row has workflow_type='DraftNoteSkillWorkflow' — mint_resolved_skills would skip it"
ok "the draft_note skill's workflow_type is present in the persisted metadata"

tc="$(pg_query "SELECT is_skill || '|' || resolved_workflow_type || '|' || status FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'draft_note'")"
[ "$tc" = "t|DraftNoteSkillWorkflow|ok" ] || fail "draft_note tool_call is '$tc', expected 't|DraftNoteSkillWorkflow|ok'"
ok "draft_note was minted as a skill (is_skill=true, resolved_workflow_type set) and reached status='ok'"

result="$(pg_query "SELECT result FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'draft_note'")"
[ -n "$result" ] && [ "$result" != "null" ] || fail "draft_note's result is empty — CloseSkillCall may not have run"
ok "draft_note's result was written by CloseSkillCall: $result"

exit 0
