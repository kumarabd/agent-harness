-- Widened for a skill's own scoped reasoning turn — a real turns row seeded
-- by an authored objective rather than a real user message. That whole
-- scoped-reasoning-turn mechanism was replaced and then removed some time
-- later; the 'skill' value stayed allowed (nothing currently writes it) since
-- narrowing the constraint back is a separate, standalone schema change this
-- migration doesn't need to make after the fact. Same widen-by-recreate
-- pattern as 024/030's own turns_parent_type_check edits — an inline CHECK
-- constraint gets an auto-generated name, so drop-by-name is fragile across
-- environments; recreate the column check instead.

ALTER TABLE turns DROP CONSTRAINT IF EXISTS turns_parent_type_check;
ALTER TABLE turns ADD  CONSTRAINT turns_parent_type_check
  CHECK (parent_type IN ('session', 'turn', 'skill'));
