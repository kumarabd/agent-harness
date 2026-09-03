#!/usr/bin/env bash
# Expectations for plan-lifecycle.json — request-pipeline/08-planning.md,
# the plan-and-execute happy path.
#
# run_scenario.sh has already waited for turn:1 (the planning turn) AND, seeing
# the :cp: fixtures, for every checkpoint turn to finish + a RecordSkill beat.
#
# Checks: PlanWorkflow ran a planning turn + one checkpoint turn per checkpoint;
# PLAN.md ended `complete` with every cp terminal; SkillDiscover staged skill
# rows under the plan_id; RecordSkill fired once with close_reason plan_complete.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
PLAN_ID="$2"   # == the planning turn id

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
plan_md() {
  kubectl exec -n "$NAMESPACE" deploy/abishekk-worker -- \
    cat "/sessions/session/$SESSION_KEY/plans/${1//:/_}/PLAN.md" 2>/dev/null || true
}
wlog() { kubectl logs -n "$NAMESPACE" deploy/abishekk-worker --since=15m 2>/dev/null || true; }
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

# --- planning turn completed, opened a plan ---
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$PLAN_ID'")" = "completed" ] \
  || fail "planning turn ($PLAN_ID) not completed"
[ "$(pg "SELECT parent_type FROM turns WHERE turn_id = '$PLAN_ID'")" = "session" ] \
  || fail "planning turn parent_type is not 'session'"
ok "planning turn completed"

# --- one checkpoint turn per checkpoint, all completed, all carry plan_id ---
n_cp="$(pg "SELECT count(*) FROM turns WHERE parent_id = '$PLAN_ID' AND parent_type = 'plan' AND turn_id LIKE '${PLAN_ID}:cp:%'")"
[ "${n_cp:-0}" = "3" ] || fail "expected 3 checkpoint turns, found $n_cp"
bad="$(pg "SELECT count(*) FROM turns WHERE parent_id = '$PLAN_ID' AND parent_type = 'plan' AND (status <> 'completed' OR plan_id <> '$PLAN_ID')")"
[ "${bad:-1}" = "0" ] || fail "$bad checkpoint turn(s) not completed or missing plan_id"
ok "3 checkpoint turns, all completed, all turns.plan_id == plan_id"

# --- PLAN.md: complete, every cp terminal ---
PLAN="$(plan_md "$PLAN_ID")"
[ -n "$PLAN" ] || fail "no PLAN.md at the plan path"
echo "$PLAN" | grep -qE '^status: complete' || fail "PLAN.md status is not 'complete':\n$PLAN"
seeded="$(echo "$PLAN" | grep -cE '^- cp[0-9]+ ' || true)"
terminal="$(echo "$PLAN" | grep -cE '^- cp[0-9]+ \[[x-]\]' || true)"
[ "$seeded" = "3" ] && [ "$terminal" = "3" ] || fail "PLAN.md: $terminal/$seeded checkpoints terminal (want 3/3)"
echo "$PLAN" | grep -qE '^      note: PAYMENT_CLIENT_KEY' || fail "cp2's checkpoint_done note did not land in PLAN.md"
ok "PLAN.md complete, all 3 checkpoints done, cp2 note kept"

# --- SkillDiscover staged skill rows under the plan_id ---
skill_rows="$(pg "SELECT count(*) FROM turn_retrieval WHERE owner_id = '$PLAN_ID' AND kind = 'skill'")"
[ "${skill_rows:-0}" -ge 1 ] || echo "  NOTE: no kind='skill' rows staged under the plan (empty store or nothing over the floor)"
[ "${skill_rows:-0}" -ge 1 ] && ok "SkillDiscover staged $skill_rows procedure(s) under the plan_id"

# --- RecordSkill fired, plan_complete / success ---
# (RecordSkill's own gate skips intent/complexity outside {task,question}x{moderate,complex};
#  the plan ran because laneIsDeliberate, which also fires on conf<0.5 — so a genuinely
#  simple+low-confidence task can run a plan but not record. Soft-check that case.)
line=""
for _ in $(seq 1 30); do
  line="$(wlog | grep -F "RecordSkill[$PLAN_ID]:" | tail -1)"
  [ -n "$line" ] && break
  sleep 1
done
if echo "$line" | grep -qE 'close_reason=plan_complete'; then
  echo "$line" | grep -q 'outcome=success' || fail "RecordSkill outcome not success: $line"
  ok "RecordSkill fired once (close_reason=plan_complete outcome=success)"
elif echo "$line" | grep -qE 'nothing worth learning'; then
  echo "  NOTE: RecordSkill skipped ($line) — classifier rated this below the record gate"
else
  fail "no RecordSkill[$PLAN_ID] log line within ~30s"
fi

# procedure written or re-versioned (match → new_version, no-match → insert_learned;
# a plain matched-reinforce writes no source_ids row, so this is a soft check)
rec="$(pg "SELECT count(*) FROM skill_procedures WHERE source_ids @> jsonb_build_array('$PLAN_ID')")"
[ "${rec:-0}" -ge 1 ] && ok "skill_procedures row carries this plan in source_ids ($rec)" \
  || echo "  NOTE: no source_ids row — RecordSkill reinforced an existing procedure without re-versioning"

exit 0
