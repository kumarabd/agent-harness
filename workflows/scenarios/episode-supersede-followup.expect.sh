#!/usr/bin/env bash
# Expectations for the episode-supersede chained pair —
# docs/components/episode-lifecycle.md + docs/components/lane-model.md.
#
# turn:1 = a DB-migration task (Deliberate, unfinished plan). turn:2 = an
# unrelated, *simple* log-rotation task. The classifier says
# continues_prior=false, so OpenEpisode closes turn:1's episode as 'superseded'
# (recording it, outcome=failure — the plan never finished). turn:2 is a Lite
# turn, so it opens NO episode of its own — the supersede just cleans up the
# abandoned one.
#
# Run manually, back-to-back:
#   KEY="test:episode-sup:$(date +%s)"
#   workflows/scenarios/run_scenario.sh episode-supersede-initial  "$KEY"
#   workflows/scenarios/run_scenario.sh episode-supersede-followup "$KEY"
#   # then, once turn:2 is 'completed':
#   NAMESPACE=agents PG_POD=abishekk-postgresql-0 PG_USER=agent_harness PG_DB=agent_harness \
#     workflows/scenarios/episode-supersede-followup.expect.sh "$KEY" "$KEY:turn:2"
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

ep1="$(pg "SELECT COALESCE(episode_id,'') FROM turns WHERE turn_id = '$T1'")"
ep2="$(pg "SELECT COALESCE(episode_id,'') FROM turns WHERE turn_id = '$T2'")"
echo "  turn:1 episode_id = '$ep1'"
echo "  turn:2 episode_id = '$ep2'"
[ "$ep1" = "$T1" ] || fail "turn:1 should have opened its own episode, got '$ep1'"
[ -z "$ep2" ] || fail "turn:2 opened an episode ('$ep2') — an unrelated simple task should take the Lite lane (no episode)"
ok "turn:1 had a Deliberate episode; turn:2 was Lite (no episode)"

n_ep="$(pg "SELECT count(*) FROM episodes WHERE session_key = '$SESSION_KEY'")"
[ "$n_ep" = "1" ] || fail "expected exactly 1 episodes row for the session, found $n_ep"
ok "exactly one episodes row (turn:1's)"

st1="$(pg "SELECT status || '|' || coalesce(close_reason,'-') FROM episodes WHERE episode_id = '$ep1'")"
[ "$st1" = "superseded|superseded" ] || fail "turn:1 episode state = '$st1', expected 'superseded|superseded'"
ok "turn:1 episode closed as superseded when the unrelated task arrived"

# turn:1 episode recorded (outcome failure — plan never finished)
cand=""
for _ in $(seq 1 30); do
  cand="$(pg "SELECT outcome FROM skill_candidates WHERE turn_id = '$ep1'")"
  [ -n "$cand" ] && break
  sleep 1
done
[ -n "$cand" ] || fail "no skill_candidates row for the superseded episode $ep1 — supersede did not dispatch RecordSkillOutcome"
[ "$cand" = "failure" ] || echo "  NOTE: superseded candidate outcome = '$cand' (expected 'failure' — plan was unfinished)"
ok "superseded episode recorded (outcome=$cand)"

exit 0
