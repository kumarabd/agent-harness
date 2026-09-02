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

ROOT_TURN_ID="$2"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

st="$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$st" = "completed" ] || fail "root turn status = '$st'"
ok "root turn completed"

# seeded plan, every seeded cp terminal
plan="$(pg "SELECT count(*) FILTER (WHERE cp_id LIKE 'cp%') || '|' || count(*) FILTER (WHERE cp_id LIKE 'cp%' AND status IN ('done','skipped')) FROM turn_plan WHERE episode_id = '$ROOT_TURN_ID'")"
seeded="${plan%%|*}"; terminal="${plan##*|}"
[ "${seeded:-0}" -ge 3 ] || fail "expected >=3 seeded cp checkpoints, got $seeded"
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

# candidate written promptly by the turn-end dispatch
cand=""
for _ in $(seq 1 20); do
  cand="$(pg "SELECT outcome || '|' || (task_embedding IS NOT NULL) || '|' || (composed_from <> '[]'::jsonb) FROM skill_candidates WHERE turn_id = '$ROOT_TURN_ID'")"
  [ -n "$cand" ] && break
  sleep 1
done
[ -n "$cand" ] || fail "no skill_candidates row within ~20s — plan_complete dispatch did not fire"
echo "  candidate: outcome|has_embedding|has_composed_from = $cand"
echo "$cand" | grep -q '^success|true|true$' || fail "candidate not a complete success: $cand"
ok "RecordSkillOutcome wrote a complete success candidate at turn end"

n="$(pg "SELECT count(*) FROM skill_candidates WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$n" = "1" ] || fail "expected exactly 1 candidate, got $n"
ok "exactly one candidate"

exit 0
