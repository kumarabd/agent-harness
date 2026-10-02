-- Setup for lcm-retrieval.json — seeds a session with a fake prior turn
-- (turn_seq=0, so it doesn't collide with the real scenario's own turn:1)
-- whose two messages are already folded through a two-level DAG (leaf ->
-- condensed, matching compaction.py's real folded_into shape) BEFORE the
-- scenario itself runs. lcm_grep/lcm_describe/lcm_expand are real
-- ToolCall-dispatched tools (not subagent-special-cased), so scripting
-- them in lcm-retrieval.json exercises the exact same handler code path a
-- real model would — this setup just gives them real pre-existing DAG
-- state to act on, deterministically, without needing a real LLM call to
-- produce a real compaction.
--
-- {{SESSION_KEY}} is substituted by run_scenario.sh before this runs.
-- Fixed UUIDs (not gen_random_uuid()) so lcm-retrieval.json's scripted
-- tool-call arguments can reference them literally.

-- Idempotency: the message/summary UUIDs below are fixed literals (so the
-- scenario JSON can reference them), which means a prior run of this scenario
-- left them behind under a different {{SESSION_KEY}}. Clear them first so a
-- standalone re-run doesn't hit messages_pkey. (run_all.sh also runs
-- cleanup_test_data.sh up front; this covers `run_scenario.sh <name>` alone.)
DELETE FROM messages WHERE message_id IN (
  'a0000000-1111-2222-3333-444444444444', 'a0000000-1111-2222-3333-444444444445');
DELETE FROM context_summaries WHERE summary_id IN (
  'b0000000-1111-2222-3333-444444444444', 'c0000000-1111-2222-3333-444444444444');

INSERT INTO sessions (session_key, platform, channel_id, system_prompt)
VALUES ('{{SESSION_KEY}}', 'test', 'test-channel', 'test')
ON CONFLICT (session_key) DO NOTHING;

INSERT INTO turns (turn_id, parent_id, parent_type, turn_seq, status)
VALUES ('{{SESSION_KEY}}:turn:0', '{{SESSION_KEY}}', 'session', 0, 'completed');

INSERT INTO messages (message_id, parent_id, role, content, seq) VALUES
  ('a0000000-1111-2222-3333-444444444444', '{{SESSION_KEY}}:turn:0', 'user', 'my favorite fruit is the durian', 0),
  ('a0000000-1111-2222-3333-444444444445', '{{SESSION_KEY}}:turn:0', 'assistant', 'noted, durian it is', 1);

INSERT INTO context_summaries (summary_id, session_key, kind, covers, content, token_count)
VALUES (
  'b0000000-1111-2222-3333-444444444444', '{{SESSION_KEY}}', 'leaf',
  ARRAY['a0000000-1111-2222-3333-444444444444', 'a0000000-1111-2222-3333-444444444445']::uuid[],
  'user discussed favorite fruit', 5
);

INSERT INTO context_summaries (summary_id, session_key, kind, covers, content, token_count)
VALUES (
  'c0000000-1111-2222-3333-444444444444', '{{SESSION_KEY}}', 'condensed',
  ARRAY['b0000000-1111-2222-3333-444444444444']::uuid[],
  'early session small talk', 3
);

UPDATE context_summaries SET folded_into = 'c0000000-1111-2222-3333-444444444444'
WHERE summary_id = 'b0000000-1111-2222-3333-444444444444';
