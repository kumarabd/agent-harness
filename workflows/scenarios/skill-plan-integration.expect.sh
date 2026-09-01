#!/usr/bin/env bash
# Expectations for skill-plan-integration.json — the full pre-LLM pipeline
# against the live deploy (request-pipeline.md, skill-subsystem.md,
# request-pipeline/08-planning.md).
#
# Steps 2-8 use REAL model/embedding/agent-brain/mcp-hub calls. The hard
# assertions are the deterministic pipeline mechanics (skill rows staged,
# composed block staged, turn_plan seeded, plan_progress advances it). The
# skill_candidates check is gated on the real classifier returning
# complexity moderate|complex — it reads the classify log line to decide
# whether an absent candidate is a bug or the correct skip.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

root_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$root_status" = "completed" ] || fail "root turn status = '$root_status', expected 'completed'"
ok "root turn completed"

# --- step 5: SkillDiscover ---
skill_ids="$(pg_query "SELECT string_agg(metadata->>'procedure_id', ',') FROM turn_retrieval WHERE turn_id = '$ROOT_TURN_ID' AND kind = 'skill'")"
[ -n "$skill_ids" ] || fail "no kind='skill' rows staged — SkillDiscover found nothing (embeddings down, or below score floor)"
ok "SkillDiscover staged skill rows: $skill_ids"
echo "$skill_ids" | grep -q "investigate-failure" || echo "  NOTE: 'investigate-failure' not among matches (retrieval-quality, not a pipeline bug)"

# --- step 6: ComposeSkill ---
composed="$(pg_query "SELECT length(content) FROM turn_retrieval WHERE turn_id = '$ROOT_TURN_ID' AND kind = 'composed'")"
[ "${composed:-0}" -gt 0 ] || fail "no kind='composed' row — ComposeSkill did not run or produced nothing"
ok "ComposeSkill staged a composed procedure ($composed chars)"
pg_query "SELECT metadata FROM turn_retrieval WHERE turn_id = '$ROOT_TURN_ID' AND kind = 'composed'" | grep -q "procedure_ids" \
  || fail "composed row missing procedure_ids provenance"
ok "composed row carries procedure_ids provenance"

# --- step 8: turn_plan seeded by ComposeSkill, advanced by plan_progress ---
plan_n="$(pg_query "SELECT count(*) FROM turn_plan WHERE turn_id = '$ROOT_TURN_ID' AND cp_id LIKE 'cp%'")"
[ "${plan_n:-0}" -ge 3 ] || fail "turn_plan seeded with only $plan_n cp* checkpoints — expected >= 3 from the merged procedure"
ok "ComposeSkill seeded $plan_n checkpoints (cp1..cp$plan_n)"

cp1="$(pg_query "SELECT status FROM turn_plan WHERE turn_id = '$ROOT_TURN_ID' AND cp_id = 'cp1'")"
[ "$cp1" = "done" ] || fail "cp1 = '${cp1:-<missing>}', expected 'done' (scripted plan_progress step 1)"
cp2="$(pg_query "SELECT status FROM turn_plan WHERE turn_id = '$ROOT_TURN_ID' AND cp_id = 'cp2'")"
[ "$cp2" = "done" ] || fail "cp2 = '${cp2:-<missing>}', expected 'done' (scripted plan_progress step 2)"
ok "plan_progress advanced cp1 and cp2 to done on the seeded plan"

cp3="$(pg_query "SELECT status || '|' || coalesce(note,'') FROM turn_plan WHERE turn_id = '$ROOT_TURN_ID' AND cp_id = 'cp3'")"
echo "$cp3" | grep -q "^revised|" || fail "cp3 = '$cp3', expected status 'revised'"
echo "$cp3" | grep -qi "flaky test fixture" || fail "cp3 revised but correction note lost: '$cp3'"
ok "plan_progress marked cp3 revised and kept the correction note"

# --- RecordSkillOutcome (classifier-gated: intent=task AND complexity moderate|complex) ---
cand=""
for _ in $(seq 1 20); do
  cand="$(pg_query "SELECT outcome || '|' || (task_embedding IS NOT NULL) || '|' || (composed_from <> '[]'::jsonb) FROM skill_candidates WHERE turn_id = '$ROOT_TURN_ID'")"
  [ -n "$cand" ] && break
  sleep 1
done

if [ -n "$cand" ]; then
  ok "RecordSkillOutcome wrote a skill_candidates row: outcome|has_embedding|has_composed_from = $cand"
  echo "$cand" | grep -q "|true|true$" || fail "skill_candidate incomplete (missing embedding or composed_from): $cand"
  ok "skill_candidate is complete (task_embedding + composed_from provenance)"
else
  cx="$(kubectl logs -n "$NAMESPACE" deploy/abishekk-worker --since=10m 2>/dev/null \
        | grep -F "classify[$ROOT_TURN_ID]" | grep -oE 'complexity=[a-z]+' | tail -1 || true)"
  echo "  no skill_candidates row; classify said: ${cx:-<not found in logs>}"
  case "$cx" in
    complexity=moderate|complexity=complex)
      fail "classify returned $cx (task) but RecordSkillOutcome did not write a skill_candidates row — real bug" ;;
    complexity=trivial|complexity=simple)
      ok "correctly skipped RecordSkillOutcome — $cx below the moderate|complex gate" ;;
    *)
      fail "no skill_candidates row and could not read the classify complexity from logs to explain it" ;;
  esac
fi

exit 0
