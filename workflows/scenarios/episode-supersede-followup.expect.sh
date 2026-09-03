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
# turn:2 must NOT be attached to turn:1's episode — it either takes the Lite
# lane (no episode) or, if the classifier rated it Deliberate, opens its own.
[ "$ep2" != "$ep1" ] || fail "turn:2 ATTACHED to turn:1's episode — an unrelated task must supersede, not continue"
if [ -z "$ep2" ]; then
  ok "turn:2 took the Lite lane (no episode)"
else
  ok "turn:2 opened its own episode ($ep2) — classifier rated it Deliberate; supersede still applies"
fi

st1="$(pg "SELECT status || '|' || coalesce(close_reason,'-') FROM episodes WHERE episode_id = '$ep1'")"
[ "$st1" = "superseded|superseded" ] || fail "turn:1 episode state = '$st1', expected 'superseded|superseded'"
ok "turn:1 episode closed as superseded when the unrelated task arrived"

# supersede dispatches RecordSkill for turn:1's (failed, unfinished) episode.
# RecordSkill (skill-subsystem.md REVISION 2026-09-02): a failed episode that
# matches an existing procedure gets a caution note; one that matches nothing
# is correctly dropped (no candidates queue to hold negative-only signal).
seen=""
for _ in $(seq 1 40); do
  seen="$(kubectl logs -n "$NAMESPACE" deploy/abishekk-worker --since=10m 2>/dev/null \
          | grep -F "RecordSkill[$ep1]" | grep -oE 'outcome=[a-z]+' | tail -1 || true)"
  [ -n "$seen" ] && break
  sleep 1
done
[ -n "$seen" ] || fail "no RecordSkill[$ep1] log line — supersede did not dispatch RecordSkill for turn:1"
[ "$seen" = "outcome=failure" ] || echo "  NOTE: RecordSkill saw '$seen' for the superseded episode (expected outcome=failure)"
ok "supersede dispatched RecordSkill for the abandoned episode ($seen)"

exit 0
