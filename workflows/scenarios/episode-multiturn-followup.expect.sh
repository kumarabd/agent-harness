#!/usr/bin/env bash
# Expectations for the episode-multiturn chained pair — docs/components/episode-lifecycle.md.
#
# THE core assertion of the episode refactor: a task that spans two user turns
# is ONE episode and produces ONE skill_candidates row over the whole
# trajectory — not one per turn (the fragmentation this change fixes).
#
# Run manually, back-to-back:
#   KEY="test:episode-mt:$(date +%s)"
#   workflows/scenarios/run_scenario.sh episode-multiturn-initial  "$KEY"
#   workflows/scenarios/run_scenario.sh episode-multiturn-followup "$KEY"
#
# Called by run_scenario.sh as: expect.sh <session_key> <followup_turn_id>
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
[ "$s1" = "completed" ] && [ "$s2" = "completed" ] || fail "turn statuses: turn:1='$s1' turn:2='$s2' (both should be completed)"
ok "both turns completed"

ep1="$(pg "SELECT episode_id FROM turns WHERE turn_id = '$T1'")"
ep2="$(pg "SELECT episode_id FROM turns WHERE turn_id = '$T2'")"
echo "  turn:1 episode_id = $ep1"
echo "  turn:2 episode_id = $ep2"
[ -n "$ep1" ] || fail "turn:1 has no episode_id"
[ "$ep1" = "$ep2" ] || fail "turn:2 opened a SEPARATE episode ($ep2) instead of attaching to turn:1's ($ep1) — continuation not detected"
ok "both turns share one episode ($ep1) — the follow-up attached"

n_ep="$(pg "SELECT count(*) FROM episodes WHERE session_key = '$SESSION_KEY'")"
[ "${n_ep:-0}" = "1" ] || fail "expected exactly 1 episodes row for the session, found $n_ep"
ok "exactly one episodes row for the session"

# --- the payoff: ONE candidate for the whole two-turn task, after idle close ---
cand=""
for _ in $(seq 1 60); do
  cand="$(pg "SELECT count(*) FROM skill_candidates WHERE turn_id = '$ep1'")"
  [ "${cand:-0}" -ge 1 ] && break
  sleep 1
done

cx="$(kubectl logs -n "$NAMESPACE" deploy/abishekk-worker --since=10m 2>/dev/null \
      | grep -F "classify[$T1]" | grep -oE 'complexity=[a-z]+' | tail -1 || true)"

if [ "${cand:-0}" = "0" ]; then
  case "$cx" in
    complexity=trivial|complexity=simple)
      ok "no candidate — classify said $cx for turn:1, below the moderate|complex record gate (acceptable)"
      exit 0 ;;
    *)
      fail "no skill_candidates row for episode $ep1 (classify said '${cx:-unknown}') — episode never recorded" ;;
  esac
fi

n_cand="$(pg "SELECT count(*) FROM skill_candidates WHERE turn_id = '$ep1'")"
[ "$n_cand" = "1" ] || fail "expected exactly ONE candidate for the episode, found $n_cand (fragmentation not fixed)"
ok "exactly ONE skill_candidates row for the two-turn episode"

# the transcript must span BOTH turns
tx="$(pg "SELECT (transcript LIKE '%peak requests-per-second%')::int + (transcript LIKE '%token-bucket%')::int FROM skill_candidates WHERE turn_id = '$ep1'")"
[ "$tx" = "2" ] || fail "candidate transcript does not span both turns (marker hits: $tx/2)"
ok "the single candidate's transcript spans both turns' messages"

um="$(pg "SELECT required_correction FROM skill_candidates WHERE turn_id = '$ep1'")"
echo "  required_correction = $um (expected t — the episode had >1 user message)"

exit 0
