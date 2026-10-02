-- resolved_workflow_type (migration 037) carried the Go workflow type name
-- for a one-shot skill dispatch shape that was deleted outright in a later
-- redesign. Nothing writes or reads it any more, so it's dropped rather than
-- left NULL forever, the same reasoning 039_tool_calls_drop_is_skill.sql
-- already applied to is_skill.
ALTER TABLE tool_calls DROP COLUMN resolved_workflow_type;
