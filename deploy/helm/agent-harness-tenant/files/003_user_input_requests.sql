-- docs/components/user-input.md — UserInputRequestWorkflow's durable record
-- of a pending/answered mid-turn human interaction. request_id is a stable,
-- Temporal-ID-style text key (not a generated uuid) — same convention as
-- turn_id/tool_call_id elsewhere in this schema, since it doubles as (part
-- of) the workflow ID used to route the eventual SignalWorkflow response.

CREATE TABLE user_input_requests (
  request_id        text PRIMARY KEY,
  turn_id            text NOT NULL REFERENCES turns(turn_id),
  workflow_id        text NOT NULL,  -- the UserInputRequestWorkflow execution to SignalWorkflow
  kind               text NOT NULL,  -- 'permission' | 'decision' | ... — open-ended, not a CHECK-constrained enum
  prompt             text NOT NULL,
  options            jsonb NOT NULL DEFAULT '[]',  -- [{id, label}]
  allow_free_text    boolean NOT NULL DEFAULT false,
  context            jsonb NOT NULL DEFAULT '{}',  -- kind-specific payload, opaque to this table
  status             text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'answered', 'cancelled')),
  selected_option_id text,
  free_text          text,
  created_at         timestamptz NOT NULL DEFAULT now(),
  answered_at        timestamptz
);
CREATE INDEX ON user_input_requests (turn_id);
