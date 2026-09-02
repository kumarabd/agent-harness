#!/usr/bin/env bash
# Expectations for the episode-upgrade chained pair — docs/components/episode-lifecycle.md,
# episode._upgrade_classification.
#
# turn:1 is a pure question (episode opens intent='question', not recordable).
# turn:2 continues the topic with an actionable request (intent='task',
# continues_prior=true). OpenEpisode attaches turn:2 AND upgrades the episode's
# intent to 'task', so the episode is now recorded on close.
#
# Run manually, back-to-back, then this expect.sh once turn:2 is 'completed':
#   NAMESPACE=agents PG_POD=abishekk-postgresql-0 PG_USER=agent_harness PG_DB=agent_harness \
#     workflows/scenarios/episode-upgrade-followup.expect.sh "$KEY" "$KEY:turn:2"
set -euo pipefail

SESSION_KEY="$1"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

T1="${SESSION_KEY}:turn:1"
T2="${SESSION_KEY}:turn:2"

s1="$(pg "SELECT status FROM turns WHERE turn_id = '$T1'")"
s2="$(pg "SELECT status FROM turns WHERE turn_id = '$T2'")"
[ "$s1" = "completed" ] && [ "$s2" = "completed" ] || fail "turn statuses: turn:1='$s1' turn:2='$s2'"
ok "both turns completed"

ep1="$(pg "SELECT episode_id FROM turns WHERE turn_id = '$T1'")"
ep2="$(pg "SELECT episode_id FROM turns WHERE turn_id = '$T2'")"
echo "  turn:1 episode_id = $ep1"
echo "  turn:2 episode_id = $ep2"
if [ "$ep1" != "$ep2" ]; then
  cx="$(kubectl logs -n "$NAMESPACE" deploy/harness --since=10m 2>/dev/null | grep -F "turn_id $T1" | grep -oE 'intent [a-z]+' | tail -1 || true)"
  fail "turn:2 opened a separate episode ($ep2) — the actionable follow-up was not seen as a continuation of the question ($cx)"
fi
ok "turn:2 attached to turn:1's episode ($ep1)"

# the anchor turn was a question; the episode intent must have been upgraded
row="$(pg "SELECT intent || '|' || complexity FROM episodes WHERE episode_id = '$ep1'")"
echo "  episode classification now: $row"
echo "$row" | grep -q '^task|' || fail "episode intent = '${row%%|*}', expected 'task' — _upgrade_classification did not bump it from the anchor's 'question'"
ok "episode intent upgraded to task"

# because it upgraded, the episode is recorded (a question-intent episode is skipped)
cand=""
for _ in $(seq 1 40); do
  cand="$(pg "SELECT outcome || '|' || (task_embedding IS NOT NULL) FROM skill_candidates WHERE turn_id = '$ep1'")"
  [ -n "$cand" ] && break
  sleep 1
done
[ -n "$cand" ] || fail "no skill_candidates row for episode $ep1 — the upgraded (task) episode was not recorded"
echo "  candidate: outcome|has_embedding = $cand"
ok "the upgraded episode was recorded"

n="$(pg "SELECT count(*) FROM skill_candidates WHERE turn_id = '$ep1'")"
[ "$n" = "1" ] || fail "expected exactly 1 candidate, got $n"
ok "exactly one candidate for the two-turn episode"

exit 0
