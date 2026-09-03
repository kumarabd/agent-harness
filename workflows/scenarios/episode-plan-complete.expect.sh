#!/usr/bin/env bash
# Expectations for episode-plan-complete.json — docs/components/episode-lifecycle.md,
# the `plan_complete` close trigger.
#
# The scripted plan_progress drives every seeded checkpoint terminal, so the
# episode must close AT TURN END (CompleteEpisode), not on the ~idleTTL idle
# sweep — close_reason='plan_complete' and the candidate lands within seconds.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
plan_md() {
  kubectl exec -n "$NAMESPACE" deploy/abishekk-worker -- \
    cat "/sessions/session/$SESSION_KEY/plans/${1//:/_}/PLAN.md" 2>/dev/null || true
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

st="$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$st" = "completed" ] || fail "root turn status = '$st'"
ok "root turn completed"

# seeded plan, every seeded cp terminal (PLAN.md — Phase 3 slice B)
PLAN="$(plan_md "$ROOT_TURN_ID")"
seeded="$(echo "$PLAN" | grep -cE '^- cp[0-9]+ ' || true)"
terminal="$(echo "$PLAN" | grep -cE '^- cp[0-9]+ \[[x-]\]' || true)"
[ "${seeded:-0}" -ge 3 ] || fail "expected >=3 seeded cpN checkpoints in PLAN.md, got $seeded"
[ "$seeded" = "$terminal" ] || fail "not all seeded checkpoints terminal: $terminal/$seeded done|skipped"
ok "all $seeded seeded checkpoints driven terminal"

# episode closed as plan_complete, at turn end (fast — poll briefly, not ~idleTTL)
ep=""
for _ in $(seq 1 20); do
  ep="$(pg "SELECT status || '|' || coalesce(close_reason,'-') FROM episodes WHERE episode_id = '$ROOT_TURN_ID'")"
  echo "$ep" | grep -q '^complete|' && break
  sleep 1
done
[ "$ep" = "complete|plan_complete" ] || fail "episode state = '$ep', expected 'complete|plan_complete' (CompleteEpisode should close it at turn end, not the idle sweep)"
ok "episode closed at turn end: $ep"

# RecordSkill (skill-subsystem.md REVISION 2026-09-02) match-or-inserts a
# skill_procedures row directly on the turn-end dispatch — no candidates queue.
rec=""
for _ in $(seq 1 40); do
  rec="$(pg "SELECT count(*) FROM skill_procedures WHERE source_ids @> '[\"$ROOT_TURN_ID\"]'::jsonb")"
  [ "${rec:-0}" -gt 0 ] && break
  sleep 1
done
[ "${rec:-0}" -gt 0 ] || fail "no skill_procedures row for this episode within ~40s — plan_complete RecordSkill dispatch did not fire"
ok "RecordSkill wrote/versioned a procedure at turn end ($rec)"

exit 0
