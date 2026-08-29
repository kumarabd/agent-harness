-- Setup for lcm-grep-nested-fold.json — reproduces the exact two-level
-- fold shape a real user walked through by hand while reviewing this
-- design, which caught a real bug: lcm_grep's covered_by_summary_id used
-- to walk to the TOPMOST folded_into ancestor, collapsing two genuinely
-- different regions into one unhelpfully-broad answer once both had been
-- folded under a common higher-level summary.
--
--   S7
--    |-- S3 -- L1(covers msg_gqa_1)
--    |      `- L2(covers msg_other_1)
--    `-- S4 -- L3(covers msg_gqa_2)
--           `- L4(covers msg_other_2)
--
-- A grep for "GQA" must report S3 for msg_gqa_1 and S4 for msg_gqa_2 —
-- NEVER S7, even though S7 is the topmost surviving (folded_into IS NULL)
-- ancestor for both. See activities/activities/lcm/retrieval.py's
-- _active_covering_summary_ids docstring for the full reasoning.
--
-- {{SESSION_KEY}} is substituted by run_scenario.sh before this runs.
-- Fixed UUIDs so lcm-grep-nested-fold.json's scripted lcm_describe calls
-- can reference them literally.

INSERT INTO sessions (session_key, platform, channel_id, system_prompt)
VALUES ('{{SESSION_KEY}}', 'test', 'test-channel', 'test')
ON CONFLICT (session_key) DO NOTHING;

INSERT INTO turns (turn_id, parent_id, parent_type, turn_seq, status)
VALUES ('{{SESSION_KEY}}:turn:0', '{{SESSION_KEY}}', 'session', 0, 'completed');

INSERT INTO messages (message_id, parent_id, role, content, seq) VALUES
  ('d0000000-0000-0000-0000-000000000001', '{{SESSION_KEY}}:turn:0', 'user', 'we chose GQA for attention because of KV cache savings', 0),
  ('d0000000-0000-0000-0000-000000000002', '{{SESSION_KEY}}:turn:0', 'assistant', 'unrelated: lets talk about the deployment pipeline', 1),
  ('d0000000-0000-0000-0000-000000000003', '{{SESSION_KEY}}:turn:0', 'user', 'later we revisited GQA and confirmed the decision', 2),
  ('d0000000-0000-0000-0000-000000000004', '{{SESSION_KEY}}:turn:0', 'assistant', 'unrelated: lets talk about the release schedule', 3);

INSERT INTO context_summaries (summary_id, session_key, kind, covers, content, token_count) VALUES
  ('e0000000-0000-0000-0000-000000000001', '{{SESSION_KEY}}', 'leaf', ARRAY['d0000000-0000-0000-0000-000000000001']::uuid[], 'L1: attention architecture discussion', 5),
  ('e0000000-0000-0000-0000-000000000002', '{{SESSION_KEY}}', 'leaf', ARRAY['d0000000-0000-0000-0000-000000000002']::uuid[], 'L2: deployment pipeline discussion', 5),
  ('e0000000-0000-0000-0000-000000000003', '{{SESSION_KEY}}', 'leaf', ARRAY['d0000000-0000-0000-0000-000000000003']::uuid[], 'L3: attention decision revisit', 5),
  ('e0000000-0000-0000-0000-000000000004', '{{SESSION_KEY}}', 'leaf', ARRAY['d0000000-0000-0000-0000-000000000004']::uuid[], 'L4: release schedule discussion', 5);

INSERT INTO context_summaries (summary_id, session_key, kind, covers, content, token_count) VALUES
  ('f0000000-0000-0000-0000-000000000003', '{{SESSION_KEY}}', 'condensed', ARRAY['e0000000-0000-0000-0000-000000000001','e0000000-0000-0000-0000-000000000002']::uuid[], 'S3: attention-optimization discussion', 3),
  ('f0000000-0000-0000-0000-000000000004', '{{SESSION_KEY}}', 'condensed', ARRAY['e0000000-0000-0000-0000-000000000003','e0000000-0000-0000-0000-000000000004']::uuid[], 'S4: later session discussion', 3);

UPDATE context_summaries SET folded_into = 'f0000000-0000-0000-0000-000000000003'
WHERE summary_id IN ('e0000000-0000-0000-0000-000000000001', 'e0000000-0000-0000-0000-000000000002');
UPDATE context_summaries SET folded_into = 'f0000000-0000-0000-0000-000000000004'
WHERE summary_id IN ('e0000000-0000-0000-0000-000000000003', 'e0000000-0000-0000-0000-000000000004');

INSERT INTO context_summaries (summary_id, session_key, kind, covers, content, token_count) VALUES
  ('a0000000-0000-0000-0000-000000000007', '{{SESSION_KEY}}', 'condensed', ARRAY['f0000000-0000-0000-0000-000000000003','f0000000-0000-0000-0000-000000000004']::uuid[], 'S7: whole early session', 2);

UPDATE context_summaries SET folded_into = 'a0000000-0000-0000-0000-000000000007'
WHERE summary_id IN ('f0000000-0000-0000-0000-000000000003', 'f0000000-0000-0000-0000-000000000004');
