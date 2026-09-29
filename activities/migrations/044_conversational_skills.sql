-- User selection is durable session configuration. Temporal remains the sole
-- owner of skill lifecycle, revision, pending work and command ordering.
ALTER TABLE sessions ADD COLUMN mode_selected_by_message text;
COMMENT ON COLUMN sessions.mode IS 'Explicit user-selected skill or chat; never a workflow dispatch target';

UPDATE skills SET description = 'User-selected journaling: prepare and approve exact diary text, then append and verify in Notion. Selection remains active until the user leaves.', input_schema = '{}'::jsonb WHERE name = 'journaling';
UPDATE skills SET description = 'User-selected service monitoring: inspect Grafana evidence and arm an approved one-shot condition intention. Selection remains active until the user leaves.', input_schema = '{}'::jsonb WHERE name = 'service_monitoring';

-- Immutable domain proposals and provider IO are content/audit, not a task queue
-- or orchestration ledger. Workflow history owns execution state.
CREATE TABLE skill_content (
  content_id text PRIMARY KEY REFERENCES tool_calls(tool_call_id),
  session_key text NOT NULL REFERENCES sessions(session_key),
  skill text NOT NULL REFERENCES skills(name),
  content text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE skill_io (
  step_id text NOT NULL REFERENCES tool_calls(tool_call_id),
  iteration int NOT NULL CHECK (iteration >= 0),
  request jsonb NOT NULL,
  response jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (step_id, iteration)
);
