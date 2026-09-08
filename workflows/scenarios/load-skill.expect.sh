#!/usr/bin/env bash
# Expectations for load-skill.json — the load_skill meta-tool plumbing
# (docs/components/turn-pipeline.md, Phase 3). The handler runs for real in a
# scripted turn; this checks it dispatched cleanly and produced an observation.
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

[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] \
  || fail "root turn not completed — the load_skill handler may have crashed"
ok "root turn completed"

row="$(pg "SELECT status || '|' || COALESCE(length(result::text),0) FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'load_skill'")"
[ -n "$row" ] || fail "no load_skill tool_calls row for this turn"
echo "$row" | grep -qE '^ok\|[1-9]' || fail "load_skill row not status=ok with a non-empty result: '$row'"
ok "load_skill dispatched (status=ok) and wrote a non-empty observation ($row)"

exit 0
