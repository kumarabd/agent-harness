#!/usr/bin/env bash
# Deletes every session (and its turns/messages/tool_calls/context_summaries)
# whose session_key starts with "test:" — the prefix run_scenario.sh always
# uses for scenario runs (see its own header). Safe to run any time; real
# sessions never use this prefix. Does NOT terminate still-running Temporal
# workflows for those sessions — run this only after they've reached a
# terminal status (or been manually terminated, e.g. via `temporal workflow
# terminate`), otherwise a still-running workflow's activities may try to
# write back to rows that no longer exist.
#
# Usage: workflows/scenarios/cleanup_test_data.sh
set -euo pipefail

NAMESPACE=agents
PG_POD=abishekk-postgresql-0
PG_USER=agent_harness
PG_DB=agent_harness

kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
  "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -v ON_ERROR_STOP=1" <<'SQL'
DELETE FROM context_summaries WHERE session_key LIKE 'test:%';
-- tool_calls first (FKs to both turns and messages) — deleting messages or
-- turns before this violates tool_calls_message_id_fkey/parent_id_fkey.
DELETE FROM tool_calls WHERE parent_id IN (
  SELECT turn_id FROM turns WHERE parent_id LIKE 'test:%' OR turn_id LIKE 'test:%'
);
DELETE FROM messages WHERE parent_id IN (
  SELECT turn_id FROM turns WHERE parent_id LIKE 'test:%' OR turn_id LIKE 'test:%'
);
DELETE FROM user_input_requests WHERE turn_id IN (
  SELECT turn_id FROM turns WHERE parent_id LIKE 'test:%' OR turn_id LIKE 'test:%'
);
DELETE FROM turns WHERE parent_id LIKE 'test:%' OR turn_id LIKE 'test:%';
DELETE FROM sessions WHERE session_key LIKE 'test:%';
SQL
