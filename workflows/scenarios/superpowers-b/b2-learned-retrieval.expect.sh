#!/usr/bin/env bash
# Suite B / b2 — REAL model. Run after b1. A fresh design task, no process doc.
# SkillDiscover must now retrieve the learned:* procedure b1 created — the agent
# reusing a skill it taught itself, without being reminded.
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"
pg() { kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
  "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""; }
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }
warn() { echo "  NOTE: $1"; }

learned="$(pg "SELECT count(*) FROM skill_procedures WHERE provenance='learned' AND valid_to IS NULL")"
[ "${learned:-0}" -ge 1 ] || fail "no learned procedures exist — run b1 first"

[ "$(pg "SELECT status FROM turns WHERE turn_id='$ROOT_TURN_ID'")" = completed ] || fail "root turn not completed"
ok "root turn completed"

skills="$(pg "SELECT string_agg(metadata->>'procedure_id', ',' ORDER BY seq) FROM turn_retrieval WHERE episode_id='$ROOT_TURN_ID' AND kind='skill'")"
echo "  retrieved skills (by rank): $skills"
[ -n "$skills" ] || fail "no skills retrieved for a design task"

echo "$skills" | grep -q "learned:" \
  || fail "no learned:* procedure retrieved — the self-taught skill isn't being surfaced for a new design task (got: $skills)"
ok "a learned:* procedure was retrieved for this design task"

composed="$(pg "SELECT metadata->>'procedure_ids' FROM turn_retrieval WHERE episode_id='$ROOT_TURN_ID' AND kind='composed'")"
echo "  composed from: $composed"
echo "$composed" | grep -q "learned:" && ok "ComposeSkill merged the learned procedure into the plan" \
  || warn "learned procedure retrieved but not in the composed set (check ComposeSkill logs)"

reply="$(pg "SELECT string_agg(content, ' ' ORDER BY seq) FROM messages WHERE parent_id='$ROOT_TURN_ID' AND role='assistant'")"
lc="$(printf '%s' "$reply" | tr 'A-Z' 'a-z')"
echo "$lc" | grep -qE "approach|option|trade-?off" && ok "response still shows the brainstorming shape (self-taught skill is guiding it)" \
  || warn "response doesn't obviously show the process — the learned procedure may be low quality"

exit 0
