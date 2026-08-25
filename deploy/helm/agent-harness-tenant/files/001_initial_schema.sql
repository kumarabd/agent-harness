-- Initial schema for the agent-harness reference implementation.
-- Copied verbatim from docs/components/state-layer.md's "Resolved: Schema"
-- section — that doc is the source of truth; this file exists to make the
-- schema applicable, not to redesign it. Applied by hand via psql for now —
-- no migration tool, not warranted at this scale yet.

CREATE EXTENSION IF NOT EXISTS pgcrypto; -- for gen_random_uuid()

-- One session coordinator lifetime. Not one row per coordinator process restart —
-- those recreate freely under the same session_key (see components/session-coordinator.md).
CREATE TABLE sessions (
  session_key     text PRIMARY KEY,        -- e.g. "agent:main:discord:guild:123"
  platform        text NOT NULL,
  channel_id      text NOT NULL,
  model           text,
  system_prompt   text,
  title           text,
  cwd             text,                    -- coding-session metadata, nullable
  repo            text,
  branch          text,
  created_at      timestamptz NOT NULL DEFAULT now(),
  last_active_at  timestamptz NOT NULL DEFAULT now(),  -- updated by persist activity per turn
  last_consolidated_turn_seq int  -- watermark for incremental session consolidation (components/session-consolidation.md)
);

-- Every workflow-shaped execution unit: top-level turns AND subagent turns, since
-- they're recursively the same workflow type. turn_id IS the Temporal workflow ID.
CREATE TABLE turns (
  turn_id       text PRIMARY KEY,          -- "{session_key}:turn:{n}" or "...:sub:{m}"
  parent_id     text NOT NULL,             -- a session_key OR another turn_id
  parent_type   text NOT NULL CHECK (parent_type IN ('session', 'turn')),  -- parent_id is polymorphic
  turn_seq      int,                       -- only meaningful when parent_type = 'session'
  status        text NOT NULL CHECK (status IN ('running', 'completed', 'cancelled', 'failed')),
  started_at    timestamptz NOT NULL DEFAULT now(),
  completed_at  timestamptz
);
CREATE INDEX ON turns (parent_id);
-- Enforces the coordinator's turn_seq semantics: two top-level turns for the same
-- session can never collide. Partial index since turn_seq is only meaningful when
-- parent_type = 'session' (subagent turns don't have one).
CREATE UNIQUE INDEX turns_session_turn_seq_uidx ON turns (parent_id, turn_seq)
  WHERE parent_type = 'session';

-- Messages are the one non-Temporal entity — no workflow/activity ID to borrow —
-- so they get a lightweight synthetic key. parent_id = turn_id is sufficient;
-- session is one join away (turns.parent_id, walked up for nested subagents),
-- never duplicated.
CREATE TABLE messages (
  message_id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  parent_id      text NOT NULL REFERENCES turns(turn_id),
  role           text NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
  content        text,
  seq            int NOT NULL,             -- ordering within the turn
  created_at     timestamptz NOT NULL DEFAULT now(),
  search_vector  tsvector GENERATED ALWAYS AS (to_tsvector('english', coalesce(content, ''))) STORED
);
CREATE INDEX ON messages (parent_id);
CREATE INDEX messages_search_idx ON messages USING GIN (search_vector);

-- One row per tool call. tool_call_id IS the Temporal activity ID, fully qualified
-- as "{turn_id}:act:{n}" since raw activity IDs are only unique within one workflow
-- execution. Under the reference-passing contract (components/temporal-workflow.md,
-- revised 2026-08-14), ModelCall mints this ID and writes this row as part of
-- producing its response — not the workflow assigning it when starting the
-- activity, since ModelCall is the one holding the arguments this row needs.
-- The workflow receives the ID back in ModelCall's output and uses it, unmodified,
-- as the ActivityID when it starts the corresponding ToolCall activity.
-- For a subagent call, tool_call_id IS the child workflow's turn_id directly —
-- no separate child_workflow_id column; the row's own primary key already is that ID.
-- NOTE (implementation-discovered, 2026-08-14): the original doc's status
-- CHECK ('ok'|'error'|'cancelled') assumed one atomic end-of-turn write.
-- Under the reference-passing contract's incremental writes, ModelCall
-- inserts this row (arguments known, outcome not yet) *before* ToolCall runs
-- and updates it — so a genuine 'pending' state exists between those two
-- writes that the original three-value CHECK had no room for. Added here as
-- a real correction, not a redesign: 'ok'/'error'/'cancelled' still mean
-- exactly what the doc says, 'pending' just names the gap that a crash
-- between the two writes would otherwise leave misrepresented as a false 'ok'.
CREATE TABLE tool_calls (
  tool_call_id   text PRIMARY KEY,         -- "{turn_id}:act:{n}", OR a child turn_id if is_subagent
  parent_id      text NOT NULL REFERENCES turns(turn_id),  -- the turn/subagent that issued it
  message_id     uuid NOT NULL REFERENCES messages(message_id),  -- the assistant message that issued it
  tool_name      text NOT NULL,
  arguments      jsonb NOT NULL,
  retry_hint     jsonb,                    -- optional, protocol-level (components/activities-outbound-delivery.md)
  is_subagent    boolean NOT NULL DEFAULT false,
  status         text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ok', 'error', 'cancelled')),
  reason         text,                     -- e.g. 'interrupted_by_new_message'
  side_effect    text CHECK (side_effect IN ('none', 'partial', 'unknown')),  -- only when status='cancelled'
  result         jsonb,
  partial_output text,
  started_at     timestamptz NOT NULL DEFAULT now(),
  completed_at   timestamptz
);
CREATE INDEX ON tool_calls (parent_id);

-- Filesystem lease/metadata index (components/session-filesystem.md).
CREATE TABLE session_filesystem_leases (
  session_key   text NOT NULL REFERENCES sessions(session_key),
  path          text NOT NULL,             -- e.g. '/session/{key}/' or '/session/{key}/sub/2/'
  holder_id     text NOT NULL,             -- worker/activity identity
  acquired_at   timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,      -- renewed periodically, not a fixed TTL
  content_hash  text,
  last_writer   text,
  PRIMARY KEY (session_key, path)
);

-- Deliver idempotency ledger (components/activities-outbound-delivery.md).
-- response_id = turn_id directly, since one turn produces exactly one final response.
CREATE TABLE delivered_responses (
  response_id   text PRIMARY KEY REFERENCES turns(turn_id),
  delivered_at  timestamptz NOT NULL DEFAULT now()
);

-- Inbound dedup ledger (components/gateway.md). Symmetric to delivered_responses
-- on the outbound side: webhook platforms redeliver at-least-once, so the gateway
-- checks-then-inserts before calling SignalWithStart, to avoid double-signaling.
CREATE TABLE ingested_messages (
  platform             text NOT NULL,
  platform_message_id  text NOT NULL,
  session_key          text NOT NULL REFERENCES sessions(session_key),
  ingested_at          timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (platform, platform_message_id)
);

-- Per-shard resume state for connection-based platforms (components/gateway.md).
CREATE TABLE gateway_shard_state (
  platform        text NOT NULL,
  shard_id        int NOT NULL,
  last_sequence   bigint,
  updated_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (platform, shard_id)
);

-- Test-fixture-only, never touched in a real deployment. Written by the
-- starter CLI (workflows/cmd/starter) before SignalWithStart, read by
-- ModelCall's own implementation — never passed through the workflow, so
-- the fixture mechanism doesn't itself violate the reference-passing
-- contract it exists to exercise (components/temporal-workflow.md,
-- "Resolved: Reference/ID Schema").
CREATE TABLE _test_scripted_responses (
  turn_id    text NOT NULL,
  seq        int NOT NULL,               -- matches ModelCallInput.context_seq
  content    text NOT NULL,
  tool_calls jsonb NOT NULL DEFAULT '[]', -- [{tool_name, is_subagent, arguments}], written straight to tool_calls by ModelCall
  usage      jsonb NOT NULL DEFAULT '{"input_tokens":0,"output_tokens":0}',
  PRIMARY KEY (turn_id, seq)
);
-- Note: deliberately NOT `REFERENCES turns(turn_id)` — the starter CLI writes
-- fixture rows (including for not-yet-started subagent turns, keyed by their
-- precomputed deterministic ID) before the corresponding `turns` row exists,
-- since turns.turn_id is only inserted once that workflow actually starts.
