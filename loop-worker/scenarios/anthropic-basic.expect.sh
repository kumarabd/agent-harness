#!/usr/bin/env bash
# anthropic-basic.json — fixture-only smoke test: the loop runs one step with
# no tool calls and delivers the scripted answer.
set -euo pipefail
ROOT_TURN_ID="$2"
pg() { kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""; }
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }
[ "$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")" = "completed" ] || fail "turn not completed"
n="$(pg "SELECT count(*) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role='assistant' AND coalesce(content,'') <> ''")"
[ "${n:-0}" = "1" ] || fail "expected one non-empty assistant message, got $n"
ok "single-step turn delivered its answer"
exit 0
