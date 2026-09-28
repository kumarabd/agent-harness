-- docs/05-architecture-domain-control-loops.md — resolved_workflow_type
-- (migration 037) carried the Go workflow type name for a one-shot skill
-- dispatch (turn.go's old runSkill, types.SkillWorkflowInput). 2026-09-27:
-- that whole dispatch shape is deleted — a "skill" is entirely a mode now,
-- entered via tools.switch_mode, never dispatched as a nested child
-- workflow keyed off this column. Nothing writes or reads it any more, so
-- it's dropped rather than left NULL forever, the same reasoning
-- 039_tool_calls_drop_is_skill.sql already applied to is_skill.
ALTER TABLE tool_calls DROP COLUMN resolved_workflow_type;
