-- docs/components/context-slot.md, "Resolved: Summary Storage Schema" —
-- LCM's Summary DAG (leaf summaries over spans of messages, condensed
-- summaries folding several leaves together as they age further). A
-- dedicated table, not reuse of messages — a summary is a genuinely
-- different kind of object, same reasoning already applied to
-- session_filesystem_leases/_test_scripted_responses.

CREATE TABLE context_summaries (
  summary_id    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  session_key   text NOT NULL REFERENCES sessions(session_key),
  kind          text NOT NULL CHECK (kind IN ('leaf', 'condensed')),
  covers        uuid[] NOT NULL, -- leaf: message_ids it summarizes; condensed: child summary_ids
  content       text NOT NULL,
  token_count   int NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON context_summaries (session_key, created_at);
