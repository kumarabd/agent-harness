-- Setup for real-assembly.json.
--
-- Seeds a completed prior top-level turn (turn_seq=0, so it never collides
-- with the scenario's own turn:1) with a 14-message incident conversation.
-- Two of the assistant turns carry tool calls whose *results* live only in
-- tool_calls.result (that's where real ToolCall writes them) — lcm.assemble
-- reconstructs those into the verbatim window on the real ModelCall path this
-- scenario forces.
--
-- The scenario then asks a question answerable ONLY from that seeded history:
--   - "14:12" (the bad-deploy time) is in verbatim assistant messages, not
--     the summary  -> proves the session-message window assembled.
--   - the "0.4%" error rate is only in a tool_calls.result row -> proves the
--     batched tool_calls fetch (assembly.py + migration 019) reconstructed it.
--
-- {{SESSION_KEY}} is substituted by run_scenario.sh before this runs.

INSERT INTO sessions (session_key, platform, channel_id, system_prompt)
VALUES ('{{SESSION_KEY}}', 'test', 'test-channel', 'You are a helpful engineering assistant. Answer from the conversation so far.')
ON CONFLICT (session_key) DO NOTHING;

INSERT INTO turns (turn_id, parent_id, parent_type, turn_seq, status)
VALUES ('{{SESSION_KEY}}:turn:0', '{{SESSION_KEY}}', 'session', 0, 'completed');

INSERT INTO messages (message_id, parent_id, role, content, seq) VALUES
  ('d1000000-0000-0000-0000-000000000000', '{{SESSION_KEY}}:turn:0', 'user',      'We have a production incident open. Symptoms: checkout latency spiked around 14:30 UTC.', 0),
  ('d1000000-0000-0000-0000-000000000001', '{{SESSION_KEY}}:turn:0', 'assistant', 'Understood. Let me pull the recent deploy history for the checkout service.', 1),
  ('d1000000-0000-0000-0000-000000000002', '{{SESSION_KEY}}:turn:0', 'user',      'Thanks.', 2),
  ('d1000000-0000-0000-0000-000000000003', '{{SESSION_KEY}}:turn:0', 'assistant', 'The 14:12 UTC deploy lowered the connection-pool size. That lines up with the 14:30 latency alert. Rolling it back.', 3),
  ('d1000000-0000-0000-0000-000000000004', '{{SESSION_KEY}}:turn:0', 'user',      'Do it, and check the error rate afterwards.', 4),
  ('d1000000-0000-0000-0000-000000000005', '{{SESSION_KEY}}:turn:0', 'assistant', 'Rollback applied. Checking the error rate now.', 5),
  ('d1000000-0000-0000-0000-000000000006', '{{SESSION_KEY}}:turn:0', 'user',      'How does it look?', 6),
  ('d1000000-0000-0000-0000-000000000007', '{{SESSION_KEY}}:turn:0', 'assistant', 'The check came back and I have the numbers. Latency is recovering.', 7),
  ('d1000000-0000-0000-0000-000000000008', '{{SESSION_KEY}}:turn:0', 'user',      'Great. Anything else pending?', 8),
  ('d1000000-0000-0000-0000-000000000009', '{{SESSION_KEY}}:turn:0', 'assistant', 'No — mitigation is done, monitoring for regressions.', 9),
  ('d1000000-0000-0000-0000-00000000000a', '{{SESSION_KEY}}:turn:0', 'user',      'Write a one-line timeline.', 10),
  ('d1000000-0000-0000-0000-00000000000b', '{{SESSION_KEY}}:turn:0', 'assistant', 'Bad deploy, then latency alert, then rollback, then recovery.', 11),
  ('d1000000-0000-0000-0000-00000000000c', '{{SESSION_KEY}}:turn:0', 'user',      'Perfect, that is all for now.', 12),
  ('d1000000-0000-0000-0000-00000000000d', '{{SESSION_KEY}}:turn:0', 'assistant', 'Acknowledged. Ping me if it regresses.', 13);

-- Tool calls on the assistant turns at seq 1 and seq 7. Results live here,
-- not in a messages row — matching how real ToolCall persists them.
INSERT INTO tool_calls (tool_call_id, parent_id, message_id, tool_name, arguments, status, result, side_effect, started_at, completed_at) VALUES
  ('{{SESSION_KEY}}:turn:0:act:1', '{{SESSION_KEY}}:turn:0', 'd1000000-0000-0000-0000-000000000001', 'search',     '{"query": "recent deploys checkout service"}', 'ok', '{"deploys": ["14:12 UTC connection-pool size change", "09:03 UTC copy tweak"]}', 'none', now() - interval '12 minutes', now() - interval '12 minutes'),
  ('{{SESSION_KEY}}:turn:0:act:2', '{{SESSION_KEY}}:turn:0', 'd1000000-0000-0000-0000-000000000007', 'shell_exec', '{"command": "curl -s internal/metrics/error_rate"}', 'ok', '{"error_rate_pct": 0.4, "baseline_pct": 0.3, "as_of": "14:41 UTC"}', 'none', now() - interval '9 minutes', now() - interval '9 minutes');

-- One non-folded leaf summary — exercises lcm.assemble's summary branch.
-- Deliberately vague: it does NOT contain the deploy time or the error rate,
-- so the scenario's assertions can only pass if the verbatim window and the
-- tool-result reconstruction both assembled.
INSERT INTO context_summaries (summary_id, session_key, kind, covers, content, token_count) VALUES
  ('e1000000-0000-0000-0000-000000000000', '{{SESSION_KEY}}', 'leaf',
   ARRAY['d1000000-0000-0000-0000-000000000000', 'd1000000-0000-0000-0000-000000000001']::uuid[],
   'Earlier in the session: a production incident was opened for a checkout latency spike and the team began investigating deploys.', 22);
