#!/usr/bin/env bash
# Expectations for plan-checkpoint-revise.json — request-pipeline/08-planning.md,
# `checkpoint_done`'s revised_tail.
#
# cp2 replaced the pending tail. Final PLAN.md must:
#   - keep cp1 + cp2 exactly as executed (done, cp2's note intact)
#   - carry the TWO revised steps as cp3/cp4, both done
#   - be `complete`, 4 checkpoints total (not 6 — the old cp3/cp4 were dropped)
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

SESSION_KEY="$1"
PLAN_ID="$2"

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

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$PLAN_ID'")" = "completed" ] || fail "planning turn not completed"

# 4 checkpoint turns ran (cp1, cp2, then the 2 revised ones as cp3/cp4)
n_cp="$(pg "SELECT count(*) FROM turns WHERE parent_id = '$PLAN_ID' AND parent_type = 'plan' AND turn_id LIKE '${PLAN_ID}:cp:%'")"
[ "${n_cp:-0}" = "4" ] || fail "expected 4 checkpoint turns, found $n_cp"
ok "4 checkpoint turns ran"

PLAN="$(plan_md "$PLAN_ID")"
[ -n "$PLAN" ] || fail "no PLAN.md"
echo "$PLAN"

echo "$PLAN" | grep -qE '^status: complete' || fail "PLAN.md not complete"
total="$(echo "$PLAN" | grep -cE '^- cp[0-9]+ ' || true)"
[ "$total" = "4" ] || fail "PLAN.md has $total checkpoints, want 4 (the old cp3/cp4 should have been dropped, not kept)"
done_n="$(echo "$PLAN" | grep -cE '^- cp[0-9]+ \[x\]' || true)"
[ "$done_n" = "4" ] || fail "only $done_n/4 checkpoints marked done"

# cp1 + cp2 untouched by the revise
echo "$PLAN" | grep -qE '^- cp1 \[x\] Pick a tracing library' || fail "cp1 changed / not done"
echo "$PLAN" | grep -qE '^- cp2 \[x\] Instrument the gateway ingress' || fail "cp2 changed / not done"
echo "$PLAN" | grep -qE '^      note: coordinator \+ worker share' || fail "cp2's revise note lost"

# the tail is the REVISED steps
echo "$PLAN" | grep -qE '^- cp3 \[x\] Add one Temporal interceptor' || fail "cp3 is not the first revised step"
echo "$PLAN" | grep -qE '^- cp4 \[x\] Run a request end-to-end' || fail "cp4 is not the second revised step"
# and NOT the originals
echo "$PLAN" | grep -q 'Instrument the coordinator' && fail "old 'Instrument the coordinator' checkpoint survived the revise"
ok "revised_tail replaced only the pending tail; cp1/cp2 + cp2's note intact"

exit 0
