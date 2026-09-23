-- docs/05-architecture-domain-control-loops.md, docs/components/turn-pipeline.md
-- ("Skills" section) — a skill is a Temporal workflow, not an activity or a
-- resolved mcp-hub/shell-hub tool. `is_skill` mirrors `is_subagent`: turn.go
-- trusts it blindly to decide whether to dispatch a child workflow instead of
-- the generic ToolCall activity. `resolved_workflow_type` carries the Go
-- workflow type name ModelCall resolved at mint time (from the Capability
-- built off turn_retrieval's kind='skill' rows, mirroring resolved_server/
-- resolved_tool's kind='tool' path in 026_tool_calls_resolved_target.sql) —
-- turn.go passes this string straight to workflow.ExecuteChildWorkflow, no
-- Go-side name-to-function registry needed. NULL/false for every ordinary
-- tool call.

ALTER TABLE tool_calls ADD COLUMN is_skill BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE tool_calls ADD COLUMN resolved_workflow_type TEXT;
