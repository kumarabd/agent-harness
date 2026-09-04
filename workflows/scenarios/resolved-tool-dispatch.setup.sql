-- Setup for resolved-tool-dispatch.json.
--
-- Seeds a turn_retrieval 'tool' row directly under the scenario's OWN
-- upcoming turn (turn:1) — as if ToolDiscover had staged it for real, from a
-- (fictitious) "demo" mcp-hub backend registering one "echo" tool. Exercises
-- docs/components/tool-registry.md's "Resolved: Three-Layer Tool Taxonomy &
-- Per-Task Resolution": capabilities.mint_resolved turns this row into a
-- directly-callable "echo" function schema, additive to the always-present
-- shell_exec/search_tools — not routed through call_tool at all.
--
-- Requires pre-creating turn:1's `turns` row (InsertMessage's own INSERT is
-- `ON CONFLICT (turn_id) DO NOTHING`, so the real one just no-ops against
-- this when the scenario's message actually arrives — no collision).
--
-- {{SESSION_KEY}} is substituted by run_scenario.sh before this runs.

-- Idempotency: a prior run of this scenario under a fixed (non-random)
-- SESSION_KEY would leave these rows behind.
DELETE FROM turn_retrieval WHERE owner_id = '{{SESSION_KEY}}:turn:1';
DELETE FROM turns WHERE turn_id = '{{SESSION_KEY}}:turn:1';

INSERT INTO sessions (session_key, platform, channel_id, system_prompt)
VALUES ('{{SESSION_KEY}}', 'test', 'test-channel', 'You are a helpful assistant with real tools. Use them when asked to.')
ON CONFLICT (session_key) DO NOTHING;

INSERT INTO turns (turn_id, parent_id, parent_type, turn_seq, status, initiated_by)
VALUES ('{{SESSION_KEY}}:turn:1', '{{SESSION_KEY}}', 'session', 0, 'running', 'user');

INSERT INTO turn_retrieval (owner_id, kind, seq, content, metadata) VALUES (
  '{{SESSION_KEY}}:turn:1', 'tool', 0,
  'demo/echo — echoes back whatever text you give it, verbatim',
  '{"server": "demo", "tool": "echo", "input_schema": {"type": "object", "properties": {"text": {"type": "string", "description": "text to echo"}}, "required": ["text"]}}'::jsonb
);
