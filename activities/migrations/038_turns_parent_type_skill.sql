-- docs/05-architecture-domain-control-loops.md — a skill's own scoped
-- reasoning turn (skills.RunReasoningTurn) is a real turns row, seeded by an
-- authored objective rather than a real user message or a subagent's own
-- tool_calls.arguments row. Neither existing value fits: 'session' implies a
-- real inbound user message, 'turn' (insert_message.py's subagent case)
-- requires deriving content from a tool_calls row that doesn't exist for a
-- skill-authored objective. A third, explicit value is the honest fix, not a
-- workaround forced through either existing path.
--
-- Same widen-by-recreate pattern as 024/030's own turns_parent_type_check
-- edits — an inline CHECK constraint gets an auto-generated name; drop-by-name
-- is fragile across environments, so just recreate the column check. Current
-- baseline is ('session', 'turn') per 030_drop_dead_pipeline_schema.sql
-- (which deliberately narrowed 024's earlier 'plan' value back out once the
-- plan/checkpoint machinery was removed) — 'plan' is NOT reintroduced here.

ALTER TABLE turns DROP CONSTRAINT IF EXISTS turns_parent_type_check;
ALTER TABLE turns ADD  CONSTRAINT turns_parent_type_check
  CHECK (parent_type IN ('session', 'turn', 'skill'));
