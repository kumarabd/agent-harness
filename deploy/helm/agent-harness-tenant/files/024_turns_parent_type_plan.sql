-- docs/components/request-pipeline/08-planning.md (Phase 3C, plan-and-execute):
-- a checkpoint turn dispatched by a PlanWorkflow has parent_type 'plan' (its
-- parent_id is the episode/anchor turn id). The planning turn itself is the
-- anchor turn and stays parent_type 'session'.
--
-- An inline CHECK constraint gets an auto-generated name; drop-by-name is
-- fragile across environments, so redefine it via a NOT VALID add + validate is
-- overkill for a widening — just recreate the column check.

ALTER TABLE turns DROP CONSTRAINT IF EXISTS turns_parent_type_check;
ALTER TABLE turns ADD  CONSTRAINT turns_parent_type_check
  CHECK (parent_type IN ('session', 'turn', 'plan'));
