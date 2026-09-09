-- turn-pipeline redesign (docs/components/turn-pipeline.md) — Phase 9 cleanup.
-- The pre-LLM pipeline (classify / lane / routing / plan-and-execute) and the
-- harness-owned skill subsystem are gone. Their schema is now unreferenced by
-- any code path.
--
-- Safe: turns.plan_id has been NULL on every row since Phase 8 stopped writing
-- it; skill_procedures / skill_cooccurrence have not been read or written since
-- the skill subsystem was removed. Nothing FKs into any of these.

DROP TABLE IF EXISTS skill_cooccurrence;
DROP TABLE IF EXISTS skill_procedures;

DROP INDEX IF EXISTS turns_plan_idx;
ALTER TABLE turns DROP COLUMN IF EXISTS plan_id;

-- parent_type 'plan' was only ever set by a PlanWorkflow checkpoint turn
-- (migration 024). Narrow the check back to the two values a turn can have now.
ALTER TABLE turns DROP CONSTRAINT IF EXISTS turns_parent_type_check;
ALTER TABLE turns ADD  CONSTRAINT turns_parent_type_check
  CHECK (parent_type IN ('session', 'turn'));
