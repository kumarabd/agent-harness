-- Part of a one-shot skill-dispatch mechanism (a "skill" invoked as its own
-- named Temporal child workflow, mirroring is_subagent/resolved_server). Both
-- columns added here were dropped later (migrations 039, 043) once that whole
-- mechanism was replaced and then removed; kept here only as the historical
-- record of what this migration once added.

ALTER TABLE tool_calls ADD COLUMN is_skill BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE tool_calls ADD COLUMN resolved_workflow_type TEXT;
