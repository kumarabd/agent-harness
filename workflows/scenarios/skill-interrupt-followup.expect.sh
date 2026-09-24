#!/usr/bin/env bash
# Second half of the skill-interrupt pair (run with AUTO_APPROVE=0 on the
# -initial half — see its own _comment). skill-interrupt-initial's turn is
# mid draft_note (blocked on its own internal, still-pending permission
# gate) when this follow-up arrives: turn.go must cascade-cancel the
# in-flight skill child workflow — and, one level deeper, its own approval
# child — fold the message into the SAME turn, and let the model act on it.
# Proves docs/components/turn-pipeline.md's "Skill workflow (child)"
# interrupt-table row for real: RequestCancelChildWorkflowExecution
# cascading two levels deep, and CloseSkillCall's own cancel-path write.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

ROOT_TURN_ID="$2"
pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] \
  || fail "root turn not completed after the follow-up"
ok "the interrupted turn resumed and completed (one turn, not two)"

# the in-flight draft_note skill was cancelled by the interrupt, cascading
# down through its own internal approval child.
tc="$(pg "SELECT resolved_workflow_type || '|' || status FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'draft_note'")"
[ "$tc" = "DraftNoteSkillWorkflow|cancelled" ] || fail "draft_note tool_call is '$tc', expected 'DraftNoteSkillWorkflow|cancelled'"
ok "the in-flight skill was cancelled by the follow-up (CloseSkillCall's cancel path ran)"

# the follow-up landed in THIS turn as a user message
n_user="$(pg "SELECT count(*) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'user' AND content ILIKE '%never mind%'")"
[ "${n_user:-0}" -ge 1 ] || fail "the follow-up message was not folded into the turn"
ok "the follow-up was folded in as a user message on the same turn"

# the model acted on the follow-up
last="$(pg "SELECT content FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant' ORDER BY seq DESC LIMIT 1")"
echo "$last" | grep -qi "dropped" || fail "final assistant message did not act on the follow-up: '$last'"
ok "the turn's final response acted on the follow-up"

exit 0
