#!/usr/bin/env bash
# Suite B / b1 — REAL model, FRESH store (no sp-* seeds). The agent follows the
# inline brainstorming process, succeeds, and synthesis generalizes the
# trajectory into a learned:* procedure. This is "the agent learned a skill
# purely from doing it."
set -euo pipefail

SESSION_KEY="$1"
ROOT_TURN_ID="$2"
pg() { kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
  "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""; }
fail() { echo "  FAIL: $1"; exit 1; }
ok() { echo "  ok: $1"; }
warn() { echo "  NOTE: $1"; }

# fresh-store guard
sp="$(pg "SELECT count(*) FROM skill_procedures WHERE id LIKE 'sp-%'")"
[ "${sp:-0}" = 0 ] || warn "$sp sp-* seeds are still present — this isn't a fresh Suite B setup (see README)"

learned_before="$(pg "SELECT count(*) FROM skill_procedures WHERE provenance='learned' AND valid_to IS NULL")"
echo "  learned procedures before this run: $learned_before"

[ "$(pg "SELECT status FROM turns WHERE turn_id='$ROOT_TURN_ID'")" = completed ] || fail "root turn not completed"
ok "root turn completed"

reply="$(pg "SELECT string_agg(content, ' ' ORDER BY seq) FROM messages WHERE parent_id='$ROOT_TURN_ID' AND role='assistant'")"
lc="$(printf '%s' "$reply" | tr 'A-Z' 'a-z')"
[ "${#reply}" -ge 400 ] || fail "reply only ${#reply} chars — no real design produced"
echo "$lc" | grep -qE "approach|option|trade-?off|alternativ" || fail "reply didn't propose approaches — process not followed"
ok "followed the process (approaches + trade-offs, ${#reply} chars)"
printf '%s' "$reply" | grep -q '```' && warn "reply has a code fence — check it's a sketch, not an implementation" || ok "hard gate held (no code)"

# RecordSkill (skill-subsystem.md REVISION 2026-09-02) generalizes inline at
# episode close — no candidates queue, no separate SkillSynthesize step. A new
# learned:* procedure appears directly, reflecting the brainstorming shape.
learned_after=""
new_proc=""
for _ in $(seq 1 90); do
  learned_after="$(pg "SELECT count(*) FROM skill_procedures WHERE provenance='learned' AND valid_to IS NULL")"
  if [ "${learned_after:-0}" -gt "${learned_before:-0}" ]; then
    new_proc="$(pg "SELECT id || ' :: ' || title FROM skill_procedures WHERE provenance='learned' AND valid_to IS NULL ORDER BY created_at DESC LIMIT 1")"
    break
  fi
  sleep 2
done
[ -n "$new_proc" ] || fail "no new learned:* procedure after 3min — RecordSkill didn't generalize the trajectory (check worker logs for RecordSkill)"
ok "RecordSkill created a learned procedure: $new_proc"

new_id="${new_proc%% ::*}"
body="$(pg "SELECT lower(string_agg(value->>'instruction', ' | ')) FROM skill_procedures, jsonb_array_elements(body) WHERE id='$new_id'")"
echo "  learned body steps: $body"
hits=0
for kw in clarif question approach design approv spec plan; do echo "$body" | grep -q "$kw" && hits=$((hits+1)); done
[ "$hits" -ge 3 ] || fail "learned procedure doesn't reflect the brainstorming process (matched $hits/7 process keywords)"
ok "the learned procedure captured the brainstorming process ($hits/7 keywords)"

echo
echo "  >>> learned procedure id for b2: $new_id"
exit 0
