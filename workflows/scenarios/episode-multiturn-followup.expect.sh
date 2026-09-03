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

# --- the payoff: RecordSkill fires ONCE for the whole two-turn task ---
# (skill-subsystem.md REVISION 2026-09-02: RecordSkill match-or-inserts one
# skill_procedures row per episode — no candidates queue. Fragmentation is
# fixed by the episode boundary, same as before.)
rec=""
for _ in $(seq 1 75); do
  rec="$(pg "SELECT count(*) FROM skill_procedures WHERE source_ids @> '[\"$ep1\"]'::jsonb")"
  [ "${rec:-0}" -ge 1 ] && break
  sleep 1
done

cx="$(kubectl logs -n "$NAMESPACE" deploy/abishekk-worker --since=10m 2>/dev/null \
      | grep -F "classify[$T1]" | grep -oE 'complexity=[a-z]+' | tail -1 || true)"

if [ "${rec:-0}" = "0" ]; then
  case "$cx" in
    complexity=trivial|complexity=simple)
      ok "no record — classify said $cx for turn:1, below the moderate|complex gate (acceptable)"
      exit 0 ;;
    *)
      fail "no skill_procedures row for episode $ep1 (classify said '${cx:-unknown}') — episode never recorded" ;;
  esac
fi

[ "$rec" = "1" ] || fail "expected exactly ONE procedure carrying the episode in source_ids, found $rec (fragmentation not fixed)"
ok "RecordSkill wrote exactly ONE procedure for the two-turn episode"
exit 0
