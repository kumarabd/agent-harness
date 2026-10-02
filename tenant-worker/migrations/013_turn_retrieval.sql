-- Originally the staging table for the request pipeline's retrieval phase
-- (that pipeline — classify/lane/routing/planning — was removed; see
-- turn-pipeline.md). Still live today for one purpose: a mid-turn
-- discover_tools call persists its results here (kind='tool') so they're
-- directly callable by name on the turn's next step — this is the "bulk
-- retrieved content" side of the reference-passing split, kept out of the
-- workflow's own history. 'memory'/'skill'/'composed' are vestigial kind
-- values nothing writes any more; only 'tool' is live.
--
-- One row set per top-level turn, sharing the turn's lifecycle (same
-- retention story as messages / tool_calls). PK is (turn_id, kind, seq) so a
-- Temporal activity retry re-running a subsystem upserts its rows rather
-- than colliding.
CREATE TABLE turn_retrieval (
  turn_id    text NOT NULL REFERENCES turns(turn_id),
  kind       text NOT NULL CHECK (kind IN ('memory', 'tool', 'skill', 'composed')),
  seq        int  NOT NULL,             -- rank within (turn_id, kind)
  content    text NOT NULL,
  score      real,                      -- subsystem-native relevance score, nullable
  metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (turn_id, kind, seq)
);
CREATE INDEX turn_retrieval_turn_id_idx ON turn_retrieval (turn_id);
