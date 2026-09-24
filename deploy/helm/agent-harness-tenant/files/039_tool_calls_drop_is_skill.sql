-- docs/05-architecture-domain-control-loops.md — is_skill (migration 037) was
-- a redundant companion boolean: resolved_workflow_type IS NOT NULL already
-- fully carries "is this a skill call," the exact same convention
-- resolved_server/resolved_tool already use with no separate boolean
-- (026_tool_calls_resolved_target.sql). Dropping it collapses
-- types.ToolCallRef's IsSkill+ResolvedWorkflowType pair into one field,
-- UseSkill string ("" = not a skill).

ALTER TABLE tool_calls DROP COLUMN is_skill;
