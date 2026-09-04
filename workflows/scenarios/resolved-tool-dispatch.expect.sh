#!/usr/bin/env bash
# Expectations for resolved-tool-dispatch.json — a fully fixture-scripted
# scenario (no real LLM call) exercising the MID-TURN half of per-task tool
# resolution (docs/components/tool-registry.md, "Resolved: Three-Layer Tool
# Taxonomy & Per-Task Resolution"): tools.search_tools' handler runs for real
# even in a scripted turn (fixtures only script the model's own response, not
# the tool dispatch), and tools._persist_discovered should write whatever it
# found into turn_retrieval under this turn's own id — the same table
# ToolDiscover's pre-turn scan writes to, which is what lets a mid-turn
# discovery be invoked by name on the turn's next step now that call_tool is
# no longer in the model's schema at all.
#
#   1. the turn completed — a crash in _persist_discovered's write path fails
#      the turn (search_tools' own results still reach the model either way;
#      this only checks the persistence side-effect it's supposed to have).
#   2. turn_retrieval gained at least one kind='tool' row for this turn.
#   3. that row has a real {server, tool} identity in its metadata — what
#      capabilities.mint_resolved needs to bind it as a callable schema.
#
# Assertion 2/3 depend on mcp-hub or shell-hub actually having *something* to
# find for a deliberately broad query — if this flakes because the live
# catalog is genuinely empty, that's real signal about the deployment, not
# this scenario.
#
# Called by run_scenario.sh as: expect.sh <session_key> <root_turn_id>
set -euo pipefail

ROOT_TURN_ID="$2"

pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}

fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }

status="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'")"
[ "$status" = "completed" ] || fail "root turn status = '$status', expected 'completed' — search_tools' persist-on-discovery path may have crashed"
ok "root turn completed — mid-turn tool persistence ran without error"

n_rows="$(pg_query "SELECT count(*) FROM turn_retrieval WHERE owner_id = '$ROOT_TURN_ID' AND kind = 'tool'")"
[ "${n_rows:-0}" -ge 1 ] || fail "no turn_retrieval kind='tool' rows for this turn — search_tools found nothing to persist, or _persist_discovered didn't write (check mcp-hub/shell-hub have SOMETHING registered)"
ok "search_tools persisted $n_rows discovered tool row(s) into turn_retrieval"

n_with_target="$(pg_query "SELECT count(*) FROM turn_retrieval WHERE owner_id = '$ROOT_TURN_ID' AND kind = 'tool' AND metadata->>'tool' IS NOT NULL AND metadata->>'tool' <> ''")"
[ "${n_with_target:-0}" -ge 1 ] || fail "persisted row(s) have no usable metadata->>'tool' — capabilities.mint_resolved would skip them all"
ok "at least one persisted row has a real {server,tool} identity mint_resolved can use"

exit 0
