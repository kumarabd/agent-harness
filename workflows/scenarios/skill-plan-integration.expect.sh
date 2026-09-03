#!/usr/bin/env bash
# Expectations for skill-plan-integration.json — the full pre-LLM pipeline
# against the live deploy (request-pipeline.md, skill-subsystem.md,
# request-pipeline/08-planning.md).
#
# Steps 2-8 use REAL model/embedding/agent-brain/mcp-hub calls. The hard
# assertions are the deterministic pipeline mechanics (skill rows staged,
# composed block staged, turn_plan seeded, plan_progress advances it) — all
# keyed on episode_id (== the root turn_id for this single-turn scenario).
#
# RecordSkillOutcome is now episode-scoped (episode-lifecycle.md): it fires
# once when the episode closes. This scenario's scripted plan_progress does NOT
# drive every checkpoint terminal, so the episode does not close at turn end —
# it closes on the coordinator's idle-exit (~idleTTL, 30s on this deploy) via
# CloseSessionEpisodes. The candidate poll below is sized to wait that out.
# The check is still gated on the real classifier returning complexity
# moderate|complex — it reads the classify log line to decide whether an
# absent candidate is a bug or the correct skip.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
# Phase 3 slice B: the checkpoint ledger is PLAN.md on the tenant PV, read off
# the worker pod (SESSION_ROOT=/sessions in the deploy).
plan_md() {
  kubectl exec -n "$NAMESPACE" deploy/abishekk-worker -- \
    cat "/sessions/session/$SESSION_KEY/plans/${1//:/_}/PLAN.md" 2>/dev/null || true
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

root_status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$root_status" = "completed" ] || fail "root turn status = '$root_status', expected 'completed'"
ok "root turn completed"

# --- step 5: SkillDiscover ---
skill_ids="$(pg_query "SELECT string_agg(metadata->>'procedure_id', ',') FROM turn_retrieval WHERE owner_id = '$ROOT_TURN_ID' AND kind = 'skill'")"
[ -n "$skill_ids" ] || fail "no kind='skill' rows staged — SkillDiscover found nothing (embeddings down, or below score floor)"
ok "SkillDiscover staged skill rows: $skill_ids"
echo "$skill_ids" | grep -q "investigate-failure" || echo "  NOTE: 'investigate-failure' not among matches (retrieval-quality, not a pipeline bug)"

# --- step 6: ComposeSkill ---
composed="$(pg_query "SELECT length(content) FROM turn_retrieval WHERE owner_id = '$ROOT_TURN_ID' AND kind = 'composed'")"
[ "${composed:-0}" -gt 0 ] || fail "no kind='composed' row — ComposeSkill did not run or produced nothing"
ok "ComposeSkill staged a composed procedure ($composed chars)"
pg_query "SELECT metadata FROM turn_retrieval WHERE owner_id = '$ROOT_TURN_ID' AND kind = 'composed'" | grep -q "procedure_ids" \
  || fail "composed row missing procedure_ids provenance"
ok "composed row carries procedure_ids provenance"

# --- step 8: PLAN.md seeded by ComposeSkill, advanced by plan_progress ---
PLAN="$(plan_md "$ROOT_TURN_ID")"
[ -n "$PLAN" ] || fail "PLAN.md empty/missing — ComposeSkill did not seed the ledger (or the PV path is wrong)"
echo "--- PLAN.md ---"; echo "$PLAN"; echo "---"
plan_n="$(echo "$PLAN" | grep -cE '^- cp[0-9]+ ' || true)"
[ "${plan_n:-0}" -ge 3 ] || fail "PLAN.md has only $plan_n cpN checkpoints — expected >= 3 from the merged procedure"
ok "ComposeSkill seeded $plan_n checkpoints (cp1..cp$plan_n)"

echo "$PLAN" | grep -qE '^- cp1 \[x\]' || fail "cp1 not marked done (scripted plan_progress step 1)"
echo "$PLAN" | grep -qE '^- cp2 \[x\]' || fail "cp2 not marked done (scripted plan_progress step 2)"
ok "plan_progress advanced cp1 and cp2 to done"

echo "$PLAN" | grep -qE '^- cp3 \[~\]' || fail "cp3 not marked revised"
echo "$PLAN" | grep -qi "flaky test fixture" || fail "cp3 revised but correction note lost"
ok "plan_progress marked cp3 revised and kept the correction note"

# --- RecordSkill (classifier-gated: intent=task AND complexity moderate|complex) ---
# RecordSkill (skill-subsystem.md REVISION 2026-09-02) match-or-inserts directly
# — no skill_candidates queue, no SkillSynthesize. A recorded episode either
# creates a new learned:* procedure or re-versions an existing one; either way
# a skill_procedures row carries this episode_id in source_ids.
rec=""
# ~55s: idleTTL (30s) + CloseSessionEpisodesWorkflow + RecordSkill (which now
# makes the generalize model call inline) + margin.
for _ in $(seq 1 70); do
  rec="$(pg_query "SELECT count(*) FROM skill_procedures WHERE source_ids @> '[\"$ROOT_TURN_ID\"]'::jsonb")"
  [ "${rec:-0}" -gt 0 ] && break
  sleep 1
done

if [ "${rec:-0}" -gt 0 ]; then
  ok "RecordSkill wrote/versioned a skill_procedures row for this episode ($rec)"
else
  cx="$(kubectl logs -n "$NAMESPACE" deploy/abishekk-worker --since=10m 2>/dev/null \
        | grep -F "classify[$ROOT_TURN_ID]" | grep -oE 'complexity=[a-z]+' | tail -1 || true)"
  echo "  no procedure carries this episode in source_ids; classify said: ${cx:-<not found in logs>}"
  case "$cx" in
    complexity=moderate|complexity=complex)
      fail "classify returned $cx (task) but RecordSkill produced no procedure — real bug (or generalize model call failed)" ;;
    complexity=trivial|complexity=simple)
      ok "correctly skipped RecordSkill — $cx below the moderate|complex gate" ;;
    *)
      fail "no procedure and could not read the classify complexity from logs to explain it" ;;
  esac
fi

exit 0
